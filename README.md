# Taweret
A [Kanister](https://github.com/kanisterio/kanister) backup management system.

Taweret sets retention periods for Kanister backups and deletes them once they expire by interacting with Kanister CRDs.

This project is in a very early development phase. Please check the issues tracker for planned features, or to submit any feature requests.

## How to

Taweret should be deployed to a Kubernetes cluster which already runs Kanister. A Taweret Helm chart does not yet exist.

### Backup CronJob

The `backup-schedule` option at the end of the `kanctl` command labels the `ActionSet` created by the `CronJob` and is used by Taweret to evaluate the backup schedule assigned to the `ActionSet`.

Taweret currently supports two backups schedules, `daily` and `weekly`.

Backup `CronJob`s can be configured in Kubernetes following the example backup `CronJob` configuration below. 

    apiVersion: batch/v1
    kind: CronJob
    metadata:
      name: backup-weekly-postgres
      namespace: kanister
    spec:
      schedule: "0 0 * * */7"
      jobTemplate:
        spec:
          template:
            spec:
              containers:
                - name: backup-postgres
                  image: bitnami/kubectl:1.21
                  imagePullPolicy: IfNotPresent
                  securityContext:
                    runAsUser: 0
                    capabilities:
                    drop:
                      - all
                  command:
                    - /bin/bash
                    - -c
                  env:
                    - name: BLUEPRINT
                      value: postgres-bp
                    - name: STATEFULSET
                      value: postgres/my-postgresql-db
                    - name: PROFILE
                      value: s3-profile
                  args:
                    - |
                      curl -L https://github.com/kanisterio/kanister/releases/download/0.78.0/kanister_0.78.0_linux_amd64.tar.gz | tar xvz -C /usr/local/bin/
                      kanctl -n kanister create actionset --action backup --namespace kanister --blueprint $BLUEPRINT --statefulset $STATEFULSET --profile $PROFILE --options backup-schedule=weekly
                  serviceAccountName: kanister-sa
              restartPolicy: Never


### Kanister ServiceAccount

The `ServiceAccount` used by a `CronJob`, which in the case of the example above is `kanister-sa`, should have permission to create `ActionSet`s, read `Blueprint`s and `Profile`s in the namespace to which Kanister has been deployed, and read `StatefulSet`s which Kanister is instructed to create backups for.

Below is an example of a `ServiceAccount` configuration with appropriate permissions across an entire cluster.

    apiVersion: v1
    kind: ServiceAccount
    metadata:
      name: kanister-sa
      namespace: kanister
    ---
    kind: ClusterRole
    apiVersion: rbac.authorization.k8s.io/v1
    metadata:
      name: kanister-sa
    rules:
      - apiGroups: ['cr.kanister.io']
        resources: ['actionsets']
        verbs: ['create', 'delete', 'get', 'list', 'watch']
      - apiGroups: ['cr.kanister.io']
        resources: ['blueprints', 'profiles']
        verbs: ['get']
      - apiGroups: ['apps']
        resources: ['statefulsets']
        verbs: ['get']
    ---
    kind: ClusterRoleBinding
    apiVersion: rbac.authorization.k8s.io/v1
    metadata:
      name: kanister-sa
    subjects:
    - kind: ServiceAccount
      name: kanister-sa
      namespace: kanister
    roleRef:
      kind: ClusterRole
      name: kanister-sa
      apiGroup: rbac.authorization.k8s.io

### Taweret pod

A Taweret pod can be run by adapting the example pod configuration below. 

The `DAILY_BACKUPS` values and `WEEKLY_BACKUPS` values should be assigned the amount of daily and weekly backups which should be retained, respectively. The default value for `DAILY_BACKUPS` is `7`, and `4` for `WEEKLY_BACKUPS`.

The `KANISTER_NAMESPACE` value should be the name of the namespace to which Kanister and Kanister CRDs have been deployed. The default value is `kanister`.

`BLUEPRINT_NAME` and `S3_PROFILE_NAME` values should be the respective `Blueprint` and `Profile` CRD names used by the backup `ActionSet`. The default values are `postgres-bp` and `s3-profile`.

The `EVAL_SCHEDULE` value should be a cron expression specifying how often existing backup `ActionSets` should be evaluated. The default value is `"1/5 * * * *"` (every 5 minutes).

Taweret exposes Prometheus metrics on port `2112`. In addition to the standard Go metrics, Taweret also exposes the current count of daily and weekly backups as `backup_count_daily` and `backup_count_weekly`.

    apiVersion: v1
    kind: Pod
    metadata:
      name: taweret
      namespace: kanister
    labels:
      app: kanister
    spec:
      containers:
      - name: taweret
        image: renku/taweret:0.2.0-beta4
        env:
        - name: DAILY_BACKUPS
          value: "7"
        - name: WEEKLY_BACKUPS
          value: "4"
        - name: KANISTER_NAMESPACE
          value: kanister
        - name: BLUEPRINT_NAME
          value: postgres-bp
        - name: S3_PROFILE_NAME
          value: s3-profile
        - name: EVAL_SCHEDULE
          value: "1/5 * * * *"
        ports:
        - containerPort: 2112
          name: metrics
          protocol: TCP
    serviceAccountName: kanister-sa

