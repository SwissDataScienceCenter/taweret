// Main is the only package for Taweret
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-co-op/gocron"
	"github.com/kanisterio/kanister/pkg/apis/cr/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type backup struct {
	name, schedule, status, backupLocation string
	time                                   time.Time
	inUse                                  bool
}

type backupconfig struct {
	Name              string `yaml:"name"`
	KanisterNamespace string `yaml:"kanisterNamespace"`
	BlueprintName     string `yaml:"blueprintName"`
	ProfileName       string `yaml:"profileName"`
	Retention         struct {
		Backups StringInt `yaml:"backups"`
		Minutes StringInt `yaml:"minutes"`
		Hours   StringInt `yaml:"hours"`
		Days    StringInt `yaml:"days"`
		Months  StringInt `yaml:"months"`
		Years   StringInt `yaml:"years"`
	}
}

// StringInt is a type for custom YAML unmarshalling
type StringInt int

type taweretmetrics struct {
	backupCount  *prometheus.GaugeVec
	oldestBackup *prometheus.GaugeVec
	newestBackup *prometheus.GaugeVec
}

type backupcounts struct {
	pending  int
	running  int
	failed   int
	skipped  int
	deleting int
	expired  int
}

func main() {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	// initialise dynamicClient
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// create the clientSet
	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// specify the crds which should be queried
	gvr := schema.GroupVersionResource{
		Group:    "cr.kanister.io",
		Version:  "v1alpha1",
		Resource: "actionsets",
	}

	taweretMetrics := initialiseMetrics()

	scheduleEvaluations(dynamicClient, gvr, clientSet, taweretMetrics)

	http.Handle("/metrics", promhttp.Handler())
	if err := http.ListenAndServe(":2112", nil); err != nil {
		log.Fatalf("metrics server failed: %v", err)
	}
}

func scheduleEvaluations(dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, clientSet *kubernetes.Clientset, taweretMetrics taweretmetrics) {
	// set evaluation schedule
	const evalSchedule string = "1/1 * * * *"

	// schedule backup evaluations
	s := gocron.NewScheduler(time.UTC)
	job, err := s.Cron(evalSchedule).Do(startEvaluation, dynamicClient, gvr, clientSet, taweretMetrics)
	if err != nil {
		log.Fatalf("error creating job: %v", err)
	}
	s.StartAsync()
	log.Printf("first evaluation scheduled: %v, evaluation schedule: %v", job.NextRun(), evalSchedule)
}

func startEvaluation(dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, clientSet *kubernetes.Clientset, taweretMetrics taweretmetrics) {
	log.Printf("starting backup config evaluations\n")

	backupConfigs, err := getBackupConfigs(clientSet)
	if err != nil {
		log.Printf("skipping evaluation: %v\n", err)
		return
	}

	actionSetCache := make(map[string][]unstructured.Unstructured)
	for _, bc := range backupConfigs {
		if _, ok := actionSetCache[bc.KanisterNamespace]; !ok {
			items, err := listActionSets(dynamicClient, gvr, bc.KanisterNamespace)
			if err != nil {
				log.Printf("error listing actionsets in namespace %v: %v\n", bc.KanisterNamespace, err)
				continue
			}
			actionSetCache[bc.KanisterNamespace] = items
		}
	}

	for _, backupConfig := range backupConfigs {
		items, ok := actionSetCache[backupConfig.KanisterNamespace]
		if !ok {
			log.Printf("%v: skipping evaluation: failed to list actionsets in namespace %v\n", backupConfig.Name, backupConfig.KanisterNamespace)
			continue
		}
		evaluateBackups(items, dynamicClient, gvr, taweretMetrics, backupConfig)
	}
	log.Printf("backup config evaluations complete\n---\n")
}

func evaluateBackups(cachedActionSets []unstructured.Unstructured, dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, taweretMetrics taweretmetrics, backupConfig backupconfig) {
	log.Printf("%v: evaluating backups\n", backupConfig.Name)

	backups, err := filterBackups(cachedActionSets, backupConfig)
	if err != nil {
		log.Printf("%v: skipping evaluation: %v\n", backupConfig.Name, err)
		return
	}

	categorisedBackups, backupCounts := categoriseBackups(backups, backupConfig)

	if len(categorisedBackups) > int(backupConfig.Retention.Backups) {
		if err := deleteOldestBackups(categorisedBackups, (len(categorisedBackups) - int(backupConfig.Retention.Backups)), dynamicClient, gvr, backupConfig); err != nil {
			log.Printf("%v: skipping metrics update after deletion error: %v\n", backupConfig.Name, err)
			return
		}
		backups, err = getBackups(dynamicClient, gvr, backupConfig)
		if err != nil {
			log.Printf("%v: skipping metrics update after refetch error: %v\n", backupConfig.Name, err)
			return
		}
		categorisedBackups, backupCounts = categoriseBackups(backups, backupConfig)
	} else {
		log.Printf("%v: no backups deleted: current: %v limit: %v\n", backupConfig.Name, len(categorisedBackups), backupConfig.Retention.Backups)
	}

	taweretMetrics.setMetrics(categorisedBackups, backupConfig, backupCounts)

	log.Printf("%v: backup evaluation complete\n", backupConfig.Name)
}

func getBackupConfigs(clientset *kubernetes.Clientset) ([]backupconfig, error) {
	var backupConfigs []backupconfig
	configmaps, err := clientset.CoreV1().ConfigMaps("kanister").List(context.TODO(), v1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("error getting configmaps: %w", err)
	}

	for _, configmap := range configmaps.Items {
		if configmap.Data["backup-config.yaml"] != "" {
			var backupConfig backupconfig

			err = yaml.Unmarshal([]byte(configmap.Data["backup-config.yaml"]), &backupConfig)
			if err != nil {
				return nil, fmt.Errorf("error unmarshalling backup-config.yaml from configmap %v: %w", configmap.Name, err)
			}

			backupConfigs = append(backupConfigs, backupConfig)

			log.Printf("backup config:\n name: %v\n kanister namespace: %v\n blueprint name: %v\n profile name: %v\n retention:\n backups: %v\n years: %v months: %v days: %v hours %v minutes: %v", backupConfig.Name, backupConfig.KanisterNamespace, backupConfig.BlueprintName, backupConfig.ProfileName, backupConfig.Retention.Backups, backupConfig.Retention.Years, backupConfig.Retention.Months, backupConfig.Retention.Days, backupConfig.Retention.Hours, backupConfig.Retention.Minutes)
		}
	}
	return backupConfigs, nil
}

// listActionSets fetches all ActionSets from the given namespace.
func listActionSets(dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, namespace string) ([]unstructured.Unstructured, error) {
	log.Printf("retrieving actionsets from namespace %v", namespace)
	actionsets, err := dynamicClient.Resource(gvr).Namespace(namespace).List(context.Background(), v1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("error listing actionsets in namespace %v: %w", namespace, err)
	}
	return actionsets.Items, nil
}

// getBackups lists and filters ActionSets matching the backup config.
// Used for post-deletion refetches where a fresh API call is required.
func getBackups(dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, backupConfig backupconfig) ([]backup, error) {
	items, err := listActionSets(dynamicClient, gvr, backupConfig.KanisterNamespace)
	if err != nil {
		return nil, fmt.Errorf("%v: %w", backupConfig.Name, err)
	}
	return filterBackups(items, backupConfig)
}

// filterBackups filters a slice of ActionSets to those matching the given backup config.
// Malformed ActionSets are skipped with a warning rather than panicking.
func filterBackups(actionsets []unstructured.Unstructured, backupConfig backupconfig) ([]backup, error) {
	var backups []backup

	log.Printf("%v: filtering backup actionsets", backupConfig.Name)

	for _, actionset := range actionsets {
		// Safe extraction of spec.actions[0].
		actions, found, err := unstructured.NestedSlice(actionset.Object, "spec", "actions")
		if err != nil || !found || len(actions) == 0 {
			continue
		}
		actionMap, ok := actions[0].(map[string]interface{})
		if !ok {
			continue
		}

		actionName, ok := actionMap["name"].(string)
		if !ok || actionName != "backup" {
			continue
		}

		optionsMap, ok := actionMap["options"].(map[string]interface{})
		if !ok {
			continue
		}
		schedule, ok := optionsMap["backup-schedule"].(string)
		if !ok {
			continue
		}

		// Only process ActionSets belonging to this backup config's schedule.
		if schedule != backupConfig.Name {
			continue
		}

		name, _, _ := unstructured.NestedString(actionset.Object, "metadata", "name")
		status, _, _ := unstructured.NestedString(actionset.Object, "status", "state")
		creationTimestamp, _, _ := unstructured.NestedString(actionset.Object, "metadata", "creationTimestamp")

		// Safe extraction of the nested backupLocation artifact.
		statusActions, found, err := unstructured.NestedSlice(actionset.Object, "status", "actions")
		if err != nil || !found || len(statusActions) == 0 {
			log.Printf("%v: actionset %v has no status.actions, skipping\n", backupConfig.Name, name)
			continue
		}
		statusActionMap, ok := statusActions[0].(map[string]interface{})
		if !ok {
			log.Printf("%v: actionset %v status.actions[0] is not a map, skipping\n", backupConfig.Name, name)
			continue
		}
		artifacts, ok := statusActionMap["artifacts"].(map[string]interface{})
		if !ok {
			log.Printf("%v: actionset %v has no artifacts, skipping\n", backupConfig.Name, name)
			continue
		}
		cloudObject, ok := artifacts["cloudObject"].(map[string]interface{})
		if !ok {
			log.Printf("%v: actionset %v has no cloudObject artifact, skipping\n", backupConfig.Name, name)
			continue
		}
		keyValue, ok := cloudObject["keyValue"].(map[string]interface{})
		if !ok {
			log.Printf("%v: actionset %v cloudObject has no keyValue, skipping\n", backupConfig.Name, name)
			continue
		}
		backupLocation, ok := keyValue["backupLocation"].(string)
		if !ok {
			log.Printf("%v: actionset %v has no backupLocation, skipping\n", backupConfig.Name, name)
			continue
		}

		thisBackup := backup{
			name:           name,
			status:         status,
			schedule:       schedule,
			backupLocation: backupLocation,
		}
		thisBackup.time, err = time.Parse(time.RFC3339, creationTimestamp)
		if err != nil {
			log.Printf("%v: failed to parse creationTimestamp for actionset %v, skipping: %v\n", backupConfig.Name, name, err)
			continue
		}

		backups = append(backups, thisBackup)
	}
	return backups, nil
}

// determine whether individual backups are required based on max retention dates and their category
func categoriseBackups(uncategorisedBackups []backup, backupConfig backupconfig) ([]backup, backupcounts) {
	var categorisedBackups []backup
	backupCounts := backupcounts{}

	log.Printf("%v: categorising backups\n", backupConfig.Name)

	maxBackupDateTime := time.Now()
	maxBackupDateTime = maxBackupDateTime.Add(time.Minute * time.Duration(backupConfig.Retention.Minutes) * -1)
	maxBackupDateTime = maxBackupDateTime.Add(time.Hour * time.Duration(backupConfig.Retention.Hours) * -1)
	maxBackupDateTime = maxBackupDateTime.AddDate(int(backupConfig.Retention.Years)*-1, int(backupConfig.Retention.Months)*-1, int(backupConfig.Retention.Days)*-1)

	for _, aBackup := range uncategorisedBackups {
		if aBackup.time.After(maxBackupDateTime) && aBackup.status == "complete" {
			aBackup.inUse = true
			categorisedBackups = append(categorisedBackups, aBackup)
		} else if aBackup.status == "complete" {
			// complete backup outside the retention window
			backupCounts.expired++
		} else if aBackup.status == "pending" {
			backupCounts.pending++
		} else if aBackup.status == "running" {
			backupCounts.running++
		} else if aBackup.status == "failed" || aBackup.status == "attemptfailed" {
			backupCounts.failed++
		} else if aBackup.status == "skipped" {
			backupCounts.skipped++
		} else if aBackup.status == "deleting" {
			backupCounts.deleting++
		}
	}

	categorisedAndSortedBackups := sortBackups(categorisedBackups)

	return categorisedAndSortedBackups, backupCounts
}

// delete a specified number of the oldest backups in a backup slice
func deleteOldestBackups(backups []backup, count int, dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, backupConfig backupconfig) error {
	backups = sortBackups(backups)
	for i := 0; i < count; i++ {
		log.Printf("%v: deleting backup %v, backup time: %v, deletion nr %v, total to delete %v, total backups in category: %v\n", backupConfig.Name, backups[i].name, backups[i].time.UTC(), i+1, count, len(backups))
		if err := deleteBackup(backups[i], dynamicClient, gvr, backupConfig); err != nil {
			return err
		}
	}
	return nil
}

// sort the backup slices with the oldest backups placed at the start of the slice
func sortBackups(backups []backup) []backup {
	sort.Slice(backups, func(q, p int) bool {
		return backups[p].time.After(backups[q].time)
	})
	return backups
}

// deletes a specified backup by creating an actionset with the action 'delete'
func deleteBackup(unusedBackup backup, dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, backupConfig backupconfig) error {
	const deletionTimeout = 30 * time.Minute

	deletionActionsetName := fmt.Sprintf("delete-%v", unusedBackup.name)

	// construct actionset crd manifest to delete backup
	deletionActionSet := v1alpha1.ActionSet{
		Spec: &v1alpha1.ActionSetSpec{
			Actions: []v1alpha1.ActionSpec{
				{
					Name:      "delete",
					Blueprint: backupConfig.BlueprintName,
					Artifacts: map[string]v1alpha1.Artifact{
						"cloudObject": {
							KeyValue: map[string]string{
								"backupLocation": unusedBackup.backupLocation,
							},
						},
					},
					Object: v1alpha1.ObjectReference{
						Kind:      "namespace",
						Name:      backupConfig.KanisterNamespace,
						Namespace: backupConfig.KanisterNamespace,
					},
					Profile: &v1alpha1.ObjectReference{
						Name:      backupConfig.ProfileName,
						Namespace: backupConfig.KanisterNamespace,
					},
				},
			},
		},
		TypeMeta: v1.TypeMeta{
			APIVersion: "cr.kanister.io/v1alpha1",
			Kind:       "ActionSet",
		},
		ObjectMeta: v1.ObjectMeta{
			Name:      deletionActionsetName,
			Namespace: backupConfig.KanisterNamespace,
		},
	}

	// convert to unstructured to apply with dynamicClient
	myCRAsUnstructured, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&deletionActionSet)
	if err != nil {
		panic(err.Error())
	}
	myCRUnstructured := &unstructured.Unstructured{Object: myCRAsUnstructured}

	// apply deletion actionset
	_, err = dynamicClient.Resource(gvr).Namespace(backupConfig.KanisterNamespace).Create(context.Background(), myCRUnstructured, v1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("%v: error applying deletion actionset %v: %w", backupConfig.Name, deletionActionsetName, err)
	}
	log.Printf("%v: applied deletion actionset %v", backupConfig.Name, deletionActionsetName)

	ctx, cancel := context.WithTimeout(context.Background(), deletionTimeout)
	defer cancel()

	// cleanupDeletionAS is set to true when we need to delete the deletion ActionSet
	// on the way out (failure, error, or timeout). On success we leave it for auditing.
	cleanupDeletionAS := false
	defer func() {
		if !cleanupDeletionAS {
			return
		}
		if err := dynamicClient.Resource(gvr).Namespace(backupConfig.KanisterNamespace).Delete(
			context.Background(), deletionActionsetName, v1.DeleteOptions{},
		); err != nil {
			log.Printf("%v: warning: failed to clean up deletion actionset %v: %v\n", backupConfig.Name, deletionActionsetName, err)
		}
	}()

	// poll until the deletion ActionSet reaches a terminal state or times out
pollLoop:
	for {
		select {
		case <-ctx.Done():
			cleanupDeletionAS = true
			return fmt.Errorf("%v: timed out after %v waiting for deletion actionset %v", backupConfig.Name, deletionTimeout, deletionActionsetName)
		default:
		}

		time.Sleep(5 * time.Second)

		actionset, err := dynamicClient.Resource(gvr).Namespace(backupConfig.KanisterNamespace).Get(ctx, deletionActionsetName, v1.GetOptions{})
		if err != nil {
			cleanupDeletionAS = true
			if ctx.Err() != nil {
				return fmt.Errorf("%v: timed out after %v waiting for deletion actionset %v", backupConfig.Name, deletionTimeout, deletionActionsetName)
			}
			return fmt.Errorf("%v: error retrieving deletion actionset %v: %w", backupConfig.Name, deletionActionsetName, err)
		}

		state, _, _ := unstructured.NestedString(actionset.Object, "status", "state")
		switch state {
		case "complete":
			log.Printf("%v: %v has completed\n", backupConfig.Name, deletionActionsetName)
			break pollLoop
		case "failed":
			cleanupDeletionAS = true
			errMsg, _, _ := unstructured.NestedString(actionset.Object, "status", "error", "message")
			return fmt.Errorf("%v: deletion actionset %v failed: %v", backupConfig.Name, deletionActionsetName, errMsg)
		default:
			log.Printf("%v: deletion actionset %v state: %v\n", backupConfig.Name, deletionActionsetName, state)
		}
	}

	// delete the original backup actionset
	if err := dynamicClient.Resource(gvr).Namespace(backupConfig.KanisterNamespace).Delete(context.Background(), unusedBackup.name, v1.DeleteOptions{}); err != nil {
		return fmt.Errorf("%v: error deleting backup actionset %v: %w", backupConfig.Name, unusedBackup.name, err)
	}

	return nil
}

// UnmarshalYAML is a custom YAML unmarshaller to allow string to stringint type conversion
func (st *StringInt) UnmarshalYAML(b []byte) error {
	var item interface{}
	if err := yaml.Unmarshal(b, &item); err != nil {
		return err
	}
	switch v := item.(type) {
	case int:
		*st = StringInt(v)
	case float64:
		*st = StringInt(int(v))
	case string:
		i, err := strconv.Atoi(v)
		if err != nil {
			return err
		}
		*st = StringInt(i)
	}
	return nil
}

// initialise Prometheus metrics
func initialiseMetrics() taweretmetrics {
	var taweretMetrics taweretmetrics
	taweretMetrics.backupCount = prometheus.NewGaugeVec(
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
	taweretMetrics.oldestBackup = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "oldest_backup_timestamp",
			Help: "Unix timestamp of the oldest retained complete backup",
		},
		[]string{
			// which backup config
			"backup_config_name",
		},
	)
	taweretMetrics.newestBackup = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "newest_backup_timestamp",
			Help: "Unix timestamp of the newest retained complete backup",
		},
		[]string{
			// which backup config
			"backup_config_name",
		},
	)

	prometheus.MustRegister(taweretMetrics.backupCount)
	prometheus.MustRegister(taweretMetrics.oldestBackup)
	prometheus.MustRegister(taweretMetrics.newestBackup)

	return taweretMetrics
}

// set Prometheus metrics values
func (taweretMetrics *taweretmetrics) setMetrics(backups []backup, backupConfig backupconfig, backupCounts backupcounts) {
	log.Printf("%v: setting Prometheus metrics\n", backupConfig.Name)

	// set newestBackup and oldestBackup to corresponding backup timestamps if backups are present
	if len(backups) > 0 {
		taweretMetrics.oldestBackup.WithLabelValues(backupConfig.Name).Set(float64(backups[0].time.Unix()))
		taweretMetrics.newestBackup.WithLabelValues(backupConfig.Name).Set(float64(backups[len(backups)-1].time.Unix()))
	} else {
		taweretMetrics.oldestBackup.WithLabelValues(backupConfig.Name).Set(0)
		taweretMetrics.newestBackup.WithLabelValues(backupConfig.Name).Set(0)
	}

	// set backupCount for all states
	taweretMetrics.backupCount.WithLabelValues(backupConfig.Name, "completed").Set(float64(len(backups)))
	taweretMetrics.backupCount.WithLabelValues(backupConfig.Name, "expired").Set(float64(backupCounts.expired))
	taweretMetrics.backupCount.WithLabelValues(backupConfig.Name, "pending").Set(float64(backupCounts.pending))
	taweretMetrics.backupCount.WithLabelValues(backupConfig.Name, "running").Set(float64(backupCounts.running))
	taweretMetrics.backupCount.WithLabelValues(backupConfig.Name, "failed").Set(float64(backupCounts.failed))
	taweretMetrics.backupCount.WithLabelValues(backupConfig.Name, "skipped").Set(float64(backupCounts.skipped))
	taweretMetrics.backupCount.WithLabelValues(backupConfig.Name, "deleting").Set(float64(backupCounts.deleting))
}
