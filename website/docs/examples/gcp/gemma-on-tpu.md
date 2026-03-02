---
sidebar_position: 407
---


# GemmaOnTPUServer

A **Platform Administrator** wants to give end users in their organization self-service access to deploy Gemma on TPU in a GKE cluster. The platform administrator creates a kro ResourceGraphDefinition called *gemmaontpuserver.kro.run* that defines the required Kubernetes resources and a CRD called *GemmaOnTPUServer* that exposes only the options they want to be configurable by end users. The ResourceGraphDefinition defines the following resources ([KCC](https://github.com/GoogleCloudPlatform/k8s-config-connector) to provide the mappings from K8s CRDs to Google Cloud APIs):

* GCP Project (external reference)
* IAMServiceAccount
* IAMPolicyMember
* IAMPartialPolicy
* StorageBucket

It also defines these Kubernetes resources that use the GCP resources:
* ServiceAccount (annotation)
* Job
* Deployment
* Service

Everything related to these resources would be hidden from the end user, simplifying their experience.  

## End User: GemmaOnTPUServer

The end user creates a `GemmaOnTPUServer` resource something like this:

```yaml
apiVersion: kro.run/v1alpha1
kind: GemmaOnTPUServer
metadata:
  name: gemma-tpu
  namespace: config-connector
spec:
  kaggelSecret: kaggle-credentials
  replicas: 1
```

They can then check the status of the applied resource:

```
kubectl get gemmaontpuservers
kubectl get gemmaontpuservers gemma-tpu -n config-connector -o yaml
```

Once done, the user can delete the `GemmaOnTPUServer` instance:

```
kubectl delete gemmaontpuserver gemma-tpu -n config-connector
```

## Administrator

### 1. Set Environment variables

```bash
export PROJECT_ID=k8sai-${USERNAME?} 
export REGION=us-central1 # << CHANGE region here 
```

### 2. GKE Autopilot Cluster with KCC and KRO

#### Create GKE Cluster

```bash
export CLUSTER_NAME="inference-cluster" # name for the admin cluster
export CHANNEL="rapid" # or "regular"

## Create a cluster with kcc addon
gcloud container clusters create-auto ${CLUSTER_NAME} \
    --release-channel ${CHANNEL} \
    --location=${REGION}
```

Setup Kubectl to target the cluster

```bash
gcloud container clusters get-credentials ${CLUSTER_NAME} --project ${PROJECT_ID} --location ${REGION}
```

#### Install KCC 

Install KCC from manifests
```bash
gcloud storage cp gs://configconnector-operator/latest/release-bundle.tar.gz release-bundle.tar.gz
tar zxvf release-bundle.tar.gz
kubectl apply -f operator-system/autopilot-configconnector-operator.yaml

# wait for the pods to be ready
kubectl wait -n configconnector-operator-system --for=condition=Ready pod --all
```

#### Give KCC permissions to manage GCP project

Create SA and bind with KCC KSA

```bash
# Instructions from here: https://cloud.google.com/config-connector/docs/how-to/install-manually#identity

# Create KCC operator KSA
gcloud iam service-accounts create kcc-operator

# Add GCP iam role bindings and use WI bind with KSA

## project owner role
gcloud projects add-iam-policy-binding ${PROJECT_ID}\
    --member="serviceAccount:kcc-operator@${PROJECT_ID}.iam.gserviceaccount.com" \
    --role="roles/owner"

## storage admin role
gcloud projects add-iam-policy-binding ${PROJECT_ID}\
    --member="serviceAccount:kcc-operator@${PROJECT_ID}.iam.gserviceaccount.com" \
    --role="roles/storage.admin"

gcloud iam service-accounts add-iam-policy-binding kcc-operator@${PROJECT_ID}.iam.gserviceaccount.com \
    --member="serviceAccount:${PROJECT_ID}.svc.id.goog[cnrm-system/cnrm-controller-manager]" \
    --role="roles/iam.workloadIdentityUser"
```

Create the `ConfigConnector` object that sets up the KCC controller

```bash
# from here: https://cloud.google.com/config-connector/docs/how-to/install-manually#addon-configuring

kubectl apply -f - <<EOF
apiVersion: core.cnrm.cloud.google.com/v1beta1
kind: ConfigConnector
metadata:
  name: configconnector.core.cnrm.cloud.google.com
spec:
  mode: cluster
  googleServiceAccount: "kcc-operator@${PROJECT_ID?}.iam.gserviceaccount.com"
  stateIntoSpec: Absent
EOF
```

#### Setup Team namespace

Create a namespace for KCC resources
```bash
export NAMESPACE=config-connector # or team-a
# from here: https://cloud.google.com/config-connector/docs/how-to/install-manually#specify
kubectl create namespace ${NAMESPACE}

# associate the gcp project with this namespace
kubectl annotate namespace ${NAMESPACE} cnrm.cloud.google.com/project-id=${PROJECT_ID?}
```

Verify KCC Installation
```bash
# wait for namespace reconcilers to be created
kubectl get pods -n cnrm-system

# wait for namespace reconcilers to be ready 
kubectl wait -n cnrm-system --for=condition=Ready pod --all
```

#### Create KCC Project object

Create the `Project` object that is used as an external reference in the RGD.

```bash
export GCP_PROJECT_PARENT_TYPE=`gcloud projects  describe ${PROJECT_ID} --format json | jq -r ".parent.type"`
export GCP_PROJECT_PARENT_ID=`gcloud projects  describe ${PROJECT_ID} --format json | jq -r ".parent.id"`

parentRefKey=$(if [[ "$GCP_PROJECT_PARENT_TYPE" == "organization" ]]; then echo "organizationRef"; else echo "folderRef"; fi)

kubectl apply -f - <<EOF
apiVersion: resourcemanager.cnrm.cloud.google.com/v1beta1
kind: Project
metadata:
  annotations:
    cnrm.cloud.google.com/auto-create-network: "false"
  name: acquire-namespace-project
  namespace: ${NAMESPACE}
spec:
  name: ""
  resourceID: ${PROJECT_ID}
  ${parentRefKey}:
    external: "${GCP_PROJECT_PARENT_ID}"
EOF
```

#### Install KRO

Install KRO following [instructions here](https://kro.run/docs/getting-started/Installation/)

```bash
export KRO_VERSION=$(curl -sL \
    https://api.github.com/repos/kubernetes-sigs/kro/releases/latest | \
    jq -r '.tag_name | ltrimstr("v")'
  )
echo $KRO_VERSION

helm install kro oci://ghcr.io/kro-run/kro/kro \
  --namespace kro \
  --create-namespace \
  --version=${KRO_VERSION}

helm -n kro list

kubectl wait -n kro --for=condition=Ready pod --all
```
### 3. Model Registry access

#### Kaggle API access
* **Kaggle Account:** You need a Kaggle account.
* **Accept Gemma License:** You must accept the Gemma model license terms and usage policy on Kaggle for the specific model version you intend to use.
* **Kaggle API Credentials:**
  * You will need your Kaggle username and a Kaggle API key.
  * To get these, download your `kaggle.json` API token from your Kaggle account page (typically `https://www.kaggle.com/YOUR_USERNAME/account`, navigate to the "API" section, and click "Create New Token").
  * The downloaded `kaggle.json` file contains your username and key. You will use these individual values for Kubernetes secret literals.

#### Create Kubernetes Secret for Kaggle

```bash
export KAGGLE_USERNAME=`jq  -r .username kaggle.json` #username from kaggle.json
export KAGGLE_KEY=`jq  -r .key kaggle.json` #key from kaggle.json
kubectl create secret generic kaggle-secret \
   --namespace=${NAMESPACE} \
   --from-literal=username=$KAGGLE_USERNAME \
   --from-literal=key=$KAGGLE_KEY
```

### 4. Install the KRO RGDs

```bash

kubectl apply -f rgd.yaml
```

Validate the RGD is installed correctly:

```
kubectl get rgd gemmaontpuserver.kro.run
```

## Cleanup

Once all user created instances are deleted, the administrator can choose to deleted the RGD.

<details>
  <summary>ResourceGraphDefinition</summary>
  ```yaml title="rgd.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: gemmaontpuserver.kro.run
spec:
  schema:
    apiVersion: kro.run/v1alpha1
    kind: GemmaOnTPUServer
    spec:
      replicas: integer | default=1
      kaggleSecret: string | default=kaggle-credentials
    #status:
    #  service: ${service.status.service}
  resources:
  - id: project ## << EXTERNAL REFERENCE setup by admin at the namespace level
    externalRef:
      apiVersion: resourcemanager.cnrm.cloud.google.com/v1beta1
      kind: Project
      metadata:
      name: acquire-namespace-project
      namespace: ${schema.metadata.namespace}
  - id: serviceaccount
    template:
      apiVersion: iam.cnrm.cloud.google.com/v1beta1
      kind: IAMServiceAccount
      metadata:
        name: ${schema.metadata.name}-wi-jetstream
        namespace: ${schema.metadata.namespace}
      spec:
        displayName: ${schema.metadata.name}-jetstream
  - id: objectuserbinding
    template:
      apiVersion: iam.cnrm.cloud.google.com/v1beta1
      kind: IAMPolicyMember
      metadata:
        name: ${schema.metadata.name}-storage-object-user
        namespace: ${schema.metadata.namespace}
      spec:
        member: serviceAccount:${serviceaccount.status.email}
        role: roles/storage.objectUser
        resourceRef:
          apiVersion: resourcemanager.cnrm.cloud.google.com/v1beta1
          kind: Project
          name: acquire-namespace-project
  - id: objectinsightsbinding
    template:
      apiVersion: iam.cnrm.cloud.google.com/v1beta1
      kind: IAMPolicyMember
      metadata:
        name: ${schema.metadata.name}-storage-insights
        namespace: ${schema.metadata.namespace}
      spec:
        member: serviceAccount:${serviceaccount.status.email}
        role: roles/storage.insightsCollectorService
        resourceRef:
          apiVersion: resourcemanager.cnrm.cloud.google.com/v1beta1
          kind: Project
          name: acquire-namespace-project
  - id: workloadidentitybinding
    #gcloud iam service-accounts add-iam-policy-binding wi-jetstream@${PROJECT_ID}.iam.gserviceaccount.com \
    #  --role roles/iam.workloadIdentityUser \
    #  --member "serviceAccount:${PROJECT_ID}.svc.id.goog[default/default]"
    template:
      apiVersion: iam.cnrm.cloud.google.com/v1beta1
      kind: IAMPartialPolicy
      metadata:
        name: ${schema.metadata.name}-workload-identity
        namespace: ${schema.metadata.namespace}
      spec:
        resourceRef:
          name: ${schema.metadata.name}-wi-jetstream
          kind: IAMServiceAccount
          apiVersion: iam.cnrm.cloud.google.com/v1beta1
        bindings:
          - role: roles/iam.workloadIdentityUser
            members:
              - member: serviceAccount:${project.spec.resourceID}.svc.id.goog[${schema.metadata.namespace}/default]
  - id: annotateDefaultServiceAccount
    # TODO KRO/BUG: Missing support for dont-delete lifecycle
    #  https://github.com/kubernetes-sigs/kro/issues/542
    template:
      apiVersion: v1
      kind: ServiceAccount
      metadata:
        name: default
        namespace: ${schema.metadata.namespace}
        annotations:
          iam.gke.io/gcp-service-account: ${serviceaccount.status.email}
  - id: bucket
    readyWhen:
      - ${bucket.status.conditions[0].status == "True"}
    template:
      apiVersion: storage.cnrm.cloud.google.com/v1beta1
      kind: StorageBucket
      metadata:
        #name: gemma7b-${schema.metadata.uid} # TODO KRO/BUG: kro 0.3 would have it
        #name: gemma7b-${schema.metadata.resourceVersion} # TODO KRO/BUG: resourceVersion keeps changing for Instance resulting in different bucket names resulting in several buckets being created in a loop
        name: gemma7b-${project.status.number} ## Use project number to make bucket name unique
        annotations:
          cnrm.cloud.google.com/force-destroy: "false"
      spec:
        storageClass: STANDARD
  # TODO Create GSA , KSA and bindings
  - id: downloadCheckpoint
    readyWhen:
      - ${downloadCheckpoint.status.completionTime != null}
    template:
      apiVersion: batch/v1
      kind: Job
      metadata:
        name: ${schema.metadata.name}-loader
      spec:
        # TODO KRO/BUG: will not work if we delete the job after it finishes. It will try to recreate the job.
        #ttlSecondsAfterFinished: 30
        template:
          spec:
            restartPolicy: Never
            containers:
              - name: inference-checkpoint
                image: us-docker.pkg.dev/cloud-tpu-images/inference/inference-checkpoint:v0.2.4
                args:
                  - -b=${bucket.metadata.name}
                  - -m=google/gemma/maxtext/7b-it/2
                volumeMounts:
                  - mountPath: "/kaggle/"
                    name: kaggle-credentials
                    readOnly: true
                resources:
                  requests:
                    google.com/tpu: "8"
                  limits:
                    google.com/tpu: "8"
            nodeSelector:
              cloud.google.com/gke-tpu-topology: 2x4
              cloud.google.com/gke-tpu-accelerator: tpu-v5-lite-podslice
            volumes:
              - name: kaggle-credentials
                secret:
                  defaultMode: 0400
                  secretName: ${schema.spec.kaggleSecret}
  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: ${schema.metadata.name}
      spec:
        replicas: 1
        selector:
          matchLabels:
            app: ${schema.metadata.name}
        template:
          metadata:
            labels:
              app: ${schema.metadata.name}
          spec:
            nodeSelector:
              cloud.google.com/gke-tpu-topology: 2x4
              cloud.google.com/gke-tpu-accelerator: tpu-v5-lite-podslice
            containers:
              - name: maxengine-server
                image: us-docker.pkg.dev/cloud-tpu-images/inference/maxengine-server:v0.2.2
                args:
                  - model_name=gemma-7b
                  - tokenizer_path=assets/tokenizer.gemma
                  - per_device_batch_size=4
                  - max_prefill_predict_length=1024
                  - max_target_length=2048
                  - async_checkpointing=false
                  - ici_fsdp_parallelism=1
                  - ici_autoregressive_parallelism=-1
                  - ici_tensor_parallelism=1
                  - scan_layers=false
                  - weight_dtype=bfloat16
                  - load_parameters_path=gs://${bucket.metadata.name}/final/unscanned/gemma_7b-it/0/checkpoints/0/items
                  - prometheus_port=9090
                ports:
                  - containerPort: 9000
                resources:
                  requests:
                    google.com/tpu: "8"
                  limits:
                    google.com/tpu: "8"
              - name: jetstream-http
                image: us-docker.pkg.dev/cloud-tpu-images/inference/jetstream-http:v0.2.2
                ports:
                  - containerPort: 8000
  - id: service
    template:
      apiVersion: v1
      kind: Service
      metadata:
        name: ${schema.metadata.name}
      spec:
        selector:
          app: ${schema.metadata.name}
        ports:
          - protocol: TCP
            name: jetstream-http
            port: 8000
            targetPort: 8000
          - protocol: TCP
            name: jetstream-grpc
            port: 9000
            targetPort: 9000
  ```
</details>
