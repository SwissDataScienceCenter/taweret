package main

import (
	"sort"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fake "k8s.io/client-go/dynamic/fake"
)

func newUnstructuredBackup(name, namespace, creationTimestamp, actionName, schedule, status, backupLocation string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cr.kanister.io/v1alpha1",
			"kind":       "ActionSet",
			"metadata": map[string]interface{}{
				"namespace":         namespace,
				"name":              name,
				"creationTimestamp": creationTimestamp,
			},
			"spec": map[string]interface{}{
				"actions": []interface{}{
					map[string]interface{}{
						"name": actionName,
						"options": map[string]interface{}{
							"backup-schedule": schedule,
						},
					},
				},
			},
			"status": map[string]interface{}{
				"state": status,
				"actions": []interface{}{
					map[string]interface{}{
						"artifacts": map[string]interface{}{
							"cloudObject": map[string]interface{}{
								"keyValue": map[string]interface{}{
									"backupLocation": backupLocation,
								},
							},
						},
					},
				},
			},
		},
	}
}

func TestGetBackups(t *testing.T) {
	defaultTime, _ := time.Parse(time.RFC3339, "2022-01-01T02:03:04.52Z")
	expectedBackups := []backup{
		{name: "backup-foo", schedule: "weekly", status: "complete", time: defaultTime, backupLocation: "pg_backups/renku/renku-postgresql/2022-01-01T02:03:04.52Z/backup.sql.gz"},
		{name: "backup-bar", schedule: "daily", status: "complete", time: defaultTime, backupLocation: "pg_backups/renku/renku-postgresql/2022-01-01T02:03:04.52Z/backup.sql.gz"},
	}
	sort.Slice(expectedBackups, func(i, j int) bool { return expectedBackups[i].name < expectedBackups[j].name })
	gvr := schema.GroupVersionResource{
		Group:    "cr.kanister.io",
		Version:  "v1alpha1",
		Resource: "actionsets",
	}
	scheme := runtime.NewScheme()

	client := fake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			{Group: "cr.kanister.io", Version: "v1alpha1", Resource: "actionsets"}: "ActionSetsList",
		},
		newUnstructuredBackup("backup-foo", "kanister", "2022-01-01T02:03:04.52Z", "backup", "weekly", "complete", "pg_backups/renku/renku-postgresql/2022-01-01T02:03:04.52Z/backup.sql.gz"),
		newUnstructuredBackup("backup-bar", "kanister", "2022-01-01T02:03:04.52Z", "backup", "daily", "complete", "pg_backups/renku/renku-postgresql/2022-01-01T02:03:04.52Z/backup.sql.gz"),
		newUnstructuredBackup("backup-baz", "kanister", "2022-01-01T02:03:04.52Z", "not-a-backup", "daily", "complete", "pg_backups/renku/renku-postgresql/2022-01-01T02:03:04.52Z/backup.sql.gz"),
	)

	var backupConfig backupconfig
	backupConfig.KanisterNamespace = "kanister"
	backupConfig.Name = "daily"

	backups := getBackups(client, gvr, backupConfig)
	if len(backups) < 1 {
		t.Fatal("Empty backups")
	}
	sort.Slice(backups, func(i, j int) bool { return expectedBackups[i].name < expectedBackups[j].name })
	for i, backup := range backups {
		if backup != expectedBackups[i] {
			t.Fatal("Returned backup different from the expected one.")
		}
	}
}
