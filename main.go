package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-co-op/gocron"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type backup struct {
	name, schedule, status string
	time                   time.Time
	in_use                 bool
}

type backupconfig struct {
	Name              string `yaml:"name"`
	KanisterNamespace string `yaml:"kanisterNamespace"`
	BlueprintName     string `yaml:"blueprintName"`
	ProfileName       string `yaml:"profileName"`
	Retention         struct {
		Backups string `yaml:"backups"`
		Minutes string `yaml:"minutes"`
		Hours   string `yaml:"hours"`
		Days    string `yaml:"days"`
		Months  string `yaml:"months"`
		Years   string `yaml:"years"`
	}
}

func main() {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	// creates the out-of-cluster config
	// var kubeconfig *string
	// if home := homedir.HomeDir(); home != "" {
	// 	kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	// } else {
	// 	kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	// }
	// flag.Parse()

	// use the current context in kubeconfig
	// config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	// if err != nil {
	// 	panic(err.Error())
	// }

	// initialise dynamicclient
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// specify the crds which should be queried
	gvr := schema.GroupVersionResource{
		Group:    "cr.kanister.io",
		Version:  "v1alpha1",
		Resource: "actionsets",
	}

	// prometheus gauge vector definition
	backupCount := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "backup_count",
			Help: "The amount of backups",
		},
		[]string{
			// which backup config
			"backup_config_name",
			// state of the backups
			"backup_status",
		},
	)

	oldestBackup := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "oldest_backup",
			Help: "The amount of backups",
		},
		[]string{
			// which backup config
			"backup_config_name",
		},
	)

	youngestBackup := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "youngest_count",
			Help: "The amount of backups",
		},
		[]string{
			// which backup config
			"backup_config_name",
		},
	)

	prometheus.MustRegister(backupCount)
	prometheus.MustRegister(oldestBackup)
	prometheus.MustRegister(youngestBackup)

	backupConfigs := getBackupConfigs(clientset, gvr)

	scheduleEvaluations(dynamicClient, gvr, backupCount, oldestBackup, youngestBackup, backupConfigs)

	http.Handle("/metrics", promhttp.Handler())
	http.ListenAndServe(":2112", nil)
}

func getBackupConfigs(clientset *kubernetes.Clientset, gvr schema.GroupVersionResource) []backupconfig {
	var backupConfigs []backupconfig
	// get configmaps
	configmaps, err := clientset.CoreV1().ConfigMaps("kanister").List(context.TODO(), v1.ListOptions{})
	if err != nil {
		log.Printf("error getting actionsets: %v\n", err)
		os.Exit(1)
	}

	for _, configmap := range configmaps.Items {
		if configmap.Data["backup-config.yaml"] != "" {
			var backupConfig backupconfig

			err = yaml.Unmarshal([]byte(configmap.Data["backup-config.yaml"]), &backupConfig)
			if err != nil {
				log.Printf("error unmarshalling backup-config.yaml: %v\n", err)
				os.Exit(1)
			}

			backupConfigs = append(backupConfigs, backupConfig)

			log.Printf("backup config:\n name: %v\n kanister namespace: %v\n blueprint name: %v\n profile name: %v\n retention:\n backups: %v\n years: %v months: %v days: %v hours %v minutes: %v", backupConfig.Name, backupConfig.KanisterNamespace, backupConfig.BlueprintName, backupConfig.ProfileName, backupConfig.Retention.Backups, backupConfig.Retention.Years, backupConfig.Retention.Months, backupConfig.Retention.Days, backupConfig.Retention.Hours, backupConfig.Retention.Minutes)
		}
	}
	return backupConfigs
}

func scheduleEvaluations(dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, backupCount *prometheus.GaugeVec, oldestBackup *prometheus.GaugeVec, youngestBackup *prometheus.GaugeVec, backupConfigs []backupconfig) {

	// set eval schedule
	const evalSchedule string = "1/1 * * * *"

	// schedule evaluation of every backupConfig
	for _, backupConfig := range backupConfigs {
		s := gocron.NewScheduler(time.UTC)
		job, err := s.Cron(evalSchedule).Do(evaluateBackups, dynamicClient, gvr, backupCount, oldestBackup, youngestBackup, backupConfig)
		if err != nil {
			log.Fatalf("%v: error creating job: %v", backupConfig.Name, err)
		}
		s.StartAsync()
		log.Printf("%v: next run: %v\n", backupConfig.Name, job.NextRun())
	}
}

func evaluateBackups(dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, backupCount *prometheus.GaugeVec, oldestBackup *prometheus.GaugeVec, youngestBackup *prometheus.GaugeVec, backupConfig backupconfig) {

	log.Printf("%v: evaluating backups\n", backupConfig.Name)

	backups := getBackups(dynamicClient, gvr, backupConfig)

	categorisedBackups, failedBackupCount := categoriseBackups(backups, backupConfig)

	maxBackups, err := strconv.Atoi(backupConfig.Retention.Backups)
	if err != nil {
		log.Printf("%v: error converting maxBackups to int: %v\n", backupConfig.Name, err)
		os.Exit(1)
	}

	// if there are excess daily backups, delete the oldest excess, then refetch and recategorise the backups
	if len(categorisedBackups) > maxBackups {
		deleteOldestBackups(categorisedBackups, (len(categorisedBackups) - maxBackups), dynamicClient, gvr, backupConfig)
		backups = getBackups(dynamicClient, gvr, backupConfig)
		categorisedBackups, failedBackupCount = categoriseBackups(backups, backupConfig)
	} else {
		log.Printf("%v: no backups deleted: current: %v limit: %v\n", backupConfig.Name, len(categorisedBackups), maxBackups)
	}

	setPrometheusMetrics(categorisedBackups, backupCount, oldestBackup, youngestBackup, backupConfig, failedBackupCount)

	log.Printf("%v: backup evaluation complete\n", backupConfig.Name)
}

func setPrometheusMetrics(backups []backup, backupCount *prometheus.GaugeVec, oldestBackup *prometheus.GaugeVec, youngestBackup *prometheus.GaugeVec, backupConfig backupconfig, failedBackupCount int) {

	log.Printf("%v: updating Prometheus metrics\n", backupConfig.Name)
	// set oldest and youngest backup metric values if backups exist, else set values to 0
	if len(backups) > 0 {
		oldestBackup.WithLabelValues(backupConfig.Name).Set(float64(backups[0].time.Unix()))
		youngestBackup.WithLabelValues(backupConfig.Name).Set(float64(backups[len(backups)-1].time.Unix()))

	} else {
		oldestBackup.WithLabelValues(backupConfig.Name).Set(0)
		youngestBackup.WithLabelValues(backupConfig.Name).Set(0)
	}

	// set backupCount for completed and failed backups
	backupCount.WithLabelValues(backupConfig.Name, "completed").Set(float64(len(backups)))
	backupCount.WithLabelValues(backupConfig.Name, "failed").Set(float64(failedBackupCount))
}

// queries Kubernetes for Actionsets, adds the actionsets with action name 'backup' to a slice of backup objects and returns the slice
func getBackups(dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, backupConfig backupconfig) []backup {
	var backups []backup

	log.Printf("%v: retrieving actionsets from Kubernetes", backupConfig.Name)

	// get actionsets
	actionsets, err := dynamicClient.Resource(gvr).Namespace(backupConfig.KanisterNamespace).List(context.Background(), v1.ListOptions{})
	if err != nil {
		log.Printf("%v: error getting actionsets: %v\n", backupConfig.Name, err)
		os.Exit(1)
	}

	log.Printf("%v: filtering backup actionsets from Kubernetes", backupConfig.Name)

	// loop through actionsets
	for _, actionset := range actionsets.Items {
		actionSpec := actionset.Object["spec"].(map[string]interface{})["actions"].([]interface{})[0].(map[string]interface{})
		actionMetadata := actionset.Object["metadata"].(map[string]interface{})

		// skip ahead if the ActionSet is not a backup
		if actionSpec["name"] != "backup" {
			continue
		}
		if actionOptions, ok := actionSpec["options"]; ok {
			if backupSchedule, ok := actionOptions.(map[string]interface{})["backup-schedule"]; ok {
				thisBackup := backup{
					name:     fmt.Sprintf("%v", actionMetadata["name"]),
					status:   fmt.Sprintf("%v", actionset.Object["status"].(map[string]interface{})["state"]),
					schedule: fmt.Sprintf("%v", backupSchedule),
				}
				thisBackup.time, _ = time.Parse(time.RFC3339, fmt.Sprintf("%v", actionMetadata["creationTimestamp"]))
				if thisBackup.schedule == backupConfig.Name {
					backups = append(backups, thisBackup)
				}
			}
		}
	}
	return backups
}

// determine whether individual backups are required based on max retention dates and their category (daily, weekly, none)
func categoriseBackups(uncategorisedBackups []backup, backupConfig backupconfig) ([]backup, int) {
	var categorisedBackups []backup
	failedBackupCount := 0

	log.Printf("%v: categorising backups\n", backupConfig.Name)

	maxBackupDateTime := time.Now()

	retentionMinutes, err := strconv.Atoi(backupConfig.Retention.Minutes)
	if err != nil {
		log.Printf("%v: error converting retention minutes to int: %v\n", backupConfig.Name, err)
		os.Exit(1)
	}
	retentionHours, err := strconv.Atoi(backupConfig.Retention.Hours)
	if err != nil {
		log.Printf("%v: error converting retention hours to int: %v\n", backupConfig.Name, err)
		os.Exit(1)
	}
	retentionDays, err := strconv.Atoi(backupConfig.Retention.Days)
	if err != nil {
		log.Printf("%v: error converting retention days to int: %v\n", backupConfig.Name, err)
		os.Exit(1)
	}
	retentionMonths, err := strconv.Atoi(backupConfig.Retention.Hours)
	if err != nil {
		log.Printf("%v: error converting retention months to int: %v\n", backupConfig.Name, err)
		os.Exit(1)
	}
	retentionYears, err := strconv.Atoi(backupConfig.Retention.Minutes)
	if err != nil {
		log.Printf("%v: error converting retention years to int: %v\n", backupConfig.Name, err)
		os.Exit(1)
	}

	maxBackupDateTime = maxBackupDateTime.Add(time.Minute * time.Duration(retentionMinutes) * -1)
	maxBackupDateTime = maxBackupDateTime.Add(time.Hour * time.Duration(retentionHours) * -1)
	maxBackupDateTime = maxBackupDateTime.AddDate(retentionYears*-1, retentionMonths*-1, retentionDays*-1)

	for _, aBackup := range uncategorisedBackups {
		if aBackup.time.After(maxBackupDateTime) && aBackup.status == "complete" {
			aBackup.in_use = true
			categorisedBackups = append(categorisedBackups, aBackup)
		} else if aBackup.status != "complete" {
			failedBackupCount++
		}
	}

	categorisedAndSortedBackups := sortBackups(categorisedBackups, backupConfig)

	return categorisedAndSortedBackups, failedBackupCount
}

// delete a specified number of the oldest backups in a backup slice
func deleteOldestBackups(backups []backup, count int, dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, backupConfig backupconfig) {
	backups = sortBackups(backups, backupConfig)
	for i := 0; i < count; i++ {
		log.Printf("%v: deleting backup %v, backup time: %v, deletion nr %v, total to delete %v, total backups in category: %v\n", backupConfig.Name, backups[i].name, backups[i].time.UTC(), i+1, count, len(backups))
		deleteBackup(backups[i], dynamicClient, gvr, backupConfig)
	}
}

// sort the backup slices with the oldest backups placed at the start of the slice
func sortBackups(backups []backup, backupConfig backupconfig) []backup {
	log.Printf("%v: sorting backups chronologically\n", backupConfig.Name)
	sort.Slice(backups, func(q, p int) bool {
		return backups[p].time.After(backups[q].time)
	})
	return backups
}

// deletes a specified backup by creating an actionset with the action 'delete'
func deleteBackup(unusedBackup backup, dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, backupConfig backupconfig) {

	// create kanctl deletion actionset
	args := []string{"create", "actionset", "--action", "delete", "--from", unusedBackup.name, "--blueprint", backupConfig.BlueprintName, "--profile", backupConfig.ProfileName, "-n", backupConfig.KanisterNamespace, "--namespacetargets", backupConfig.KanisterNamespace}
	cmd := exec.Command("/usr/local/bin/kanctl", args...)
	stdout, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("%v: %v\n", backupConfig.Name, err)
	}

	// get name of deletion actionset
	kanctlOutput := strings.TrimSpace(string(stdout))
	deletionActionsetName := parseKanctlStdout(kanctlOutput)

	// loop to check status of deletion actionset whilst actionset is running
	for {
		log.Printf("%v: waiting for %v to complete... ", backupConfig.Name, deletionActionsetName)
		time.Sleep(5 * time.Second)

		// get deletion actionset
		actionset, err := dynamicClient.Resource(gvr).Namespace(backupConfig.KanisterNamespace).Get(context.Background(), deletionActionsetName, v1.GetOptions{})
		if err != nil {
			log.Printf("%v: error retrieving deletion actionset: %v\n", backupConfig.Name, err)
			os.Exit(1)
		}

		// check if deletion actionset status is "complete"
		if actionset.Object["status"].(map[string]interface{})["state"] == "complete" {
			log.Printf("%v: %v has completed\n", backupConfig.Name, deletionActionsetName)
			break
		}

		// check if deletion actionset status is "failed"
		if actionset.Object["status"].(map[string]interface{})["state"] == "failed" {
			log.Printf("%v: error deleting backup: %v\n", backupConfig.Name, deletionActionsetName)
			break
		}

		// print current state of deletion actionset
		log.Printf("%v\n", actionset.Object["status"].(map[string]interface{})["state"])
	}

	// delete backup actionset
	err = dynamicClient.Resource(gvr).Namespace(backupConfig.KanisterNamespace).Delete(context.Background(), unusedBackup.name, v1.DeleteOptions{})
	if err != nil {
		log.Printf("%v: error deleting backup actionset: %v\n", backupConfig.Name, err)
		os.Exit(1)
	}

}

// parse the stdout from kanctl. If kanctl created the deletion actionset successfully, returns the name of the deletion actionset, else prints the kanctl error and exits.
func parseKanctlStdout(kanctlOutput string) string {
	log.Println("parsing kanctl output")
	match, err := regexp.MatchString("^actionset.*created$", kanctlOutput)
	if match {
		deletionActionsetName := strings.TrimPrefix(kanctlOutput, "actionset ")
		deletionActionsetName = strings.TrimSuffix(deletionActionsetName, " created")
		return deletionActionsetName
	} else {
		log.Printf("error getting kanctl output: %v match: %v error: %v \n ", kanctlOutput, match, err)
		os.Exit(1)
	}
	return kanctlOutput
}
