# Taweret
A [Kanister](https://github.com/kanisterio/kanister) backup management system.

Taweret sets retention periods for Kanister backups and deletes them once they expire by interacting with Kanister CRDs.

This project is in an early development phase. Please check the issues tracker for planned features, or to submit any feature requests.

## How to

Taweret should be deployed to a Kubernetes cluster which already runs Kanister. 

### Taweret

Taweret can be installed through its Helm chart:

    helm repo add renku https://swissdatasciencecenter.github.io/helm-charts
    helm install taweret renku/taweret

Backup configurations can be defined in the Helm values file. The default backup configuration is:

    backupConfigs:
      daily-postgres:
        name: daily-postgres
        kanisterNamespace: kanister
        blueprintName: postgres-bp
        profileName: default-profile
        retention:
          backups: 7
          minutes: 0
          hours: 0
          days: 7
          months: 0
          years: 0

The Taweret version which is installed can be set by specifying the image tag used by the Helm chart. To see the available image tags, please check the tags in the GitHub repo.

Please be aware that the default image tag set in the Helm chart may not always be the most up to date Taweret image.

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
                      value: default-profile
                  args:
                    - |
                      curl -L https://github.com/kanisterio/kanister/releases/download/0.78.0/kanister_0.78.0_linux_amd64.tar.gz | tar xvz -C /usr/local/bin/
                      kanctl -n kanister create actionset --action backup --namespace kanister --blueprint $BLUEPRINT --statefulset $STATEFULSET --profile $PROFILE --options backup-schedule=weekly
                  serviceAccountName: kanister-sa
              restartPolicy: Never

### Kanister ServiceAccount

The `ServiceAccount` used by a `CronJob`, which in the case of the example above is `kanister-sa`, should have permissions to create `ActionSet`s, read `Blueprint`s and `Profile`s in the namespace to which Kanister has been deployed, and read `StatefulSet`s which Kanister is instructed to create backups for.

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
