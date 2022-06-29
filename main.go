package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-co-op/gocron"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

type backup struct {
	name, schedule, status string
	time                   time.Time
	in_use                 bool
}

func main() {

	dailyBackupsRetainedPtr := flag.Int("daily-backups", 7, "the amount of daily backups which should be retained")
	weeklyBackupsRetainedPtr := flag.Int("weekly-backups", 4, "the amount of weekly backups which should be retained")
	kanisterNamespacePtr := flag.String("kanister-namespace", "kanister", "the kubernetes namespace in which kanister resides")
	blueprintNamePtr := flag.String("blueprint-name", "postgres-bp", "the blueprint of the backups which should be managed")
	s3ProfileNamePtr := flag.String("s3-profile", "s3-profile", "the S3 profile of the S3 bucket which contains the backups")
	evalSchedulePtr := flag.String("eval-schedule", "1/5 * * * *", "The cron schedule at which backups should be evaluated")

	//creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	//initialise dynamicclient
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	//specify the crds which should be queried
	gvr := schema.GroupVersionResource{
		Group:    "cr.kanister.io",
		Version:  "v1alpha1",
		Resource: "actionsets",
	}

	log.Printf("Retaining %v daily, %v weekly backups\n", *dailyBackupsRetainedPtr, *weeklyBackupsRetainedPtr)

	//scheduler test
	s := gocron.NewScheduler(time.UTC)
	job, err := s.Cron(*evalSchedulePtr).Do(evaluateBackups, dynamicClient, gvr, *kanisterNamespacePtr, *dailyBackupsRetainedPtr, *weeklyBackupsRetainedPtr, *blueprintNamePtr, *s3ProfileNamePtr)
	if err != nil {
		log.Fatalf("Error creating job: %v", err)
	}

	s.StartAsync()
	log.Println("Last run:", job.LastRun())
	log.Println("Next run:", job.NextRun())
	log.Println("Current run status:", job.IsRunning())

	//prometheus metrics export test

	http.Handle("/metrics", promhttp.Handler())
	http.ListenAndServe(":2112", nil)
}

var (
	dailyBackupCount = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "backup_count_daily",
		Help: "The total amount of daily backups",
	})
	weeklyBackupCount = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "backup_count_weekly",
		Help: "The total amount of weekly backups",
	})
)

func evaluateBackups(dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, kanisterNamespace string, dailyBackupsRetained int, weeklyBackupsRetained int, blueprintName string, s3ProfileName string) {

	log.Println("Evaluating backups")

	backups := getBackupActionsets(dynamicClient, gvr, kanisterNamespace)

	dailyBackups, weeklyBackups := categoriseBackups(backups, dailyBackupsRetained, weeklyBackupsRetained)

	dailyBackupCount.Set(float64(len(dailyBackups)))
	weeklyBackupCount.Set(float64(len(weeklyBackups)))

	//if there are excess daily backups, delete the oldest excess
	if len(dailyBackups) > dailyBackupsRetained {
		deleteOldestBackups(dailyBackups, (len(dailyBackups) - dailyBackupsRetained), dynamicClient, gvr, kanisterNamespace, blueprintName, s3ProfileName)
	} else {
		log.Printf("No daily backups deleted: Current: %v Limit: %v\n", len(dailyBackups), dailyBackupsRetained)
	}

	//if there are excess weekly backups, delete the oldest excess
	if len(weeklyBackups) > weeklyBackupsRetained {
		deleteOldestBackups(weeklyBackups, (len(weeklyBackups) - weeklyBackupsRetained), dynamicClient, gvr, kanisterNamespace, blueprintName, s3ProfileName)
	} else {
		log.Printf("No weekly backups deleted: Current: %v Limit: %v\n", len(weeklyBackups), weeklyBackupsRetained)
	}

	log.Printf("Backup evaluation complete\n---\n")
}

//queries Kubernetes for Actionsets, adds the actionsets with action name 'backup' to a slice of backup objects and returns the slice
func getBackupActionsets(dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, kanisterNamespace string) []backup {
	var backups []backup

	log.Println("Retrieving actionsets from Kubernetes")

	//get actionsets
	actionsets, err := dynamicClient.Resource(gvr).Namespace(kanisterNamespace).List(context.Background(), v1.ListOptions{})
	if err != nil {
		log.Printf("Error getting actionsets: %v\n", err)
		os.Exit(1)
	}

	log.Println("Filtering backup actionsets from Kubernetes")

	//loop through actionsets
	for _, actionset := range actionsets.Items {

		//check if the actionset is a backup
		if actionset.Object["spec"].(map[string]interface{})["actions"].([]interface{})[0].(map[string]interface{})["name"] == "backup" {

			//initialise backup object and assign name
			thisBackup := backup{name: fmt.Sprintf("%v", actionset.Object["metadata"].(map[string]interface{})["name"])}

			//get creationTimestamp of actionset and assign to backup object time attribute
			thisBackup.time, _ = time.Parse(time.RFC3339, fmt.Sprintf("%v", actionset.Object["metadata"].(map[string]interface{})["creationTimestamp"]))

			//get status of actionset and assign to backup object status attribute
			thisBackup.status = fmt.Sprintf("%v", actionset.Object["status"].(map[string]interface{})["state"])

			//check if the options map has been initialised before getting backup-schedule value. If the options map is not initialised, then trying to retrieve the backup-schedule value will result in a panic.
			if actionset.Object["spec"].(map[string]interface{})["actions"].([]interface{})[0].(map[string]interface{})["options"] != nil {

				//check if the backup-schedule has a value assigned to it
				if actionset.Object["spec"].(map[string]interface{})["actions"].([]interface{})[0].(map[string]interface{})["options"].(map[string]interface{})["backup-schedule"] != nil {

					//assign backup-schedule value to backup object schedule attribute
					thisBackup.schedule = fmt.Sprintf("%v", actionset.Object["spec"].(map[string]interface{})["actions"].([]interface{})[0].(map[string]interface{})["options"].(map[string]interface{})["backup-schedule"])
				} else {

					//if the backup-schedule attribute has no value or does not exist, assign the schedule attribute of the backup object to none.
					thisBackup.schedule = "none"
				}
			} else {

				//if the options map is not initialised, assign the schedule attribute of the backup object to none.
				thisBackup.schedule = "none"
			}
			backups = append(backups, thisBackup)
		}
	}
	return backups
}

//determine whether individual backups are required based on max retention dates and their category (daily, weekly, none)
func categoriseBackups(uncategorisedBackups []backup, dailyBackupsRetained int, weeklyBackupsRetained int) ([]backup, []backup) {
	var dailyBackups []backup
	var weeklyBackups []backup

	log.Println("Categorising backups")

	start := time.Now()

	//find the oldest date a daily backup should be
	maxDailyBackupDate := start.AddDate(0, 0, dailyBackupsRetained*-1)
	//find the oldest date a weekly backup should be
	maxWeeklyBackupDate := start.AddDate(0, 0, weeklyBackupsRetained*7*-1)

	for _, aBackup := range uncategorisedBackups {
		if aBackup.time.After(maxDailyBackupDate) && aBackup.status == "complete" && aBackup.schedule == "daily" {
			aBackup.in_use = true
			dailyBackups = append(dailyBackups, aBackup)
		} else if aBackup.time.After(maxWeeklyBackupDate) && aBackup.status == "complete" && aBackup.schedule == "weekly" {
			aBackup.in_use = true
			weeklyBackups = append(weeklyBackups, aBackup)
		}
	}
	return dailyBackups, weeklyBackups
}

//delete a specified number of the oldest backups in a backup slice
func deleteOldestBackups(backups []backup, count int, dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, kanisterNamespace string, blueprintName string, s3ProfileName string) {
	backups = sortBackups(backups)
	for i := 0; i < count; i++ {
		log.Printf("Deleting backup %v, backup time: %v, deletion nr %v, total to delete %v, total backups in category: %v\n", backups[i].name, backups[i].time.UTC(), i+1, count, len(backups))
		deleteBackup(backups[i], dynamicClient, gvr, kanisterNamespace, blueprintName, s3ProfileName)
	}
}

//sort the backup slices with the oldest backups placed at the start of the slice
func sortBackups(backups []backup) []backup {
	log.Println("Sorting backups chronologically")
	sort.Slice(backups, func(q, p int) bool {
		return backups[p].time.After(backups[q].time)
	})
	return backups
}

//deletes a specified backup by creating an actionset with the action 'delete'
func deleteBackup(unusedBackup backup, dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, kanisterNamespace string, blueprintName string, s3ProfileName string) {

	//create kanctl deletion actionset
	args := []string{"create", "actionset", "--action", "delete", "--from", unusedBackup.name, "--blueprint", blueprintName, "--profile", s3ProfileName, "-n", kanisterNamespace, "--namespacetargets", kanisterNamespace}
	cmd := exec.Command("/usr/local/bin/kanctl", args...)
	stdout, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("%v\n", err)
	}

	//get name of deletion actionset
	kanctlOutput := strings.TrimSpace(string(stdout))
	deletionActionsetName := parseKanctlStdout(kanctlOutput)

	//loop to check status of deletion actionset whilst actionset is running
	for {
		log.Printf("Waiting for %v to complete... ", deletionActionsetName)
		time.Sleep(5 * time.Second)

		//get deletion actionset
		actionset, err := dynamicClient.Resource(gvr).Namespace(kanisterNamespace).Get(context.Background(), deletionActionsetName, v1.GetOptions{})
		if err != nil {
			log.Printf("Error retrieving deletion actionset: %v\n", err)
			os.Exit(1)
		}

		//check if deletion actionset status is "complete"
		if actionset.Object["status"].(map[string]interface{})["state"] == "complete" {
			log.Printf("%v has completed\n", deletionActionsetName)
			break
		}

		//check if deletion actionset status is "failed"
		if actionset.Object["status"].(map[string]interface{})["state"] == "failed" {
			log.Printf("Error deleting backup: %v\n", deletionActionsetName)
			break
		}

		//print current state of deletion actionset
		log.Printf("%v\n", actionset.Object["status"].(map[string]interface{})["state"])
	}

	//delete backup actionset
	err = dynamicClient.Resource(gvr).Namespace(kanisterNamespace).Delete(context.Background(), unusedBackup.name, v1.DeleteOptions{})
	if err != nil {
		log.Printf("Error deleting backup actionset: %v\n", err)
		os.Exit(1)
	}

}

//Parse the stdout from kanctl. If kanctl created the deletion actionset successfully, returns the name of the deletion actionset, else prints the kanctl error and exits.
func parseKanctlStdout(kanctlOutput string) string {
	log.Println("Parsing kanctl output")
	match, err := regexp.MatchString("^actionset.*created$", kanctlOutput)
	if match {
		deletionActionsetName := strings.TrimPrefix(kanctlOutput, "actionset ")
		deletionActionsetName = strings.TrimSuffix(deletionActionsetName, " created")
		return deletionActionsetName
	} else {
		log.Printf("Error getting kanctl output: %v match: %v error: %v \n ", kanctlOutput, match, err)
		os.Exit(1)
	}
	return kanctlOutput
}
