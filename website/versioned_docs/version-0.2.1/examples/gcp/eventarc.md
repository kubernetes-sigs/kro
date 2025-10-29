---
sidebar_position: 407
---

# GCSBucketWithFinalizerTrigger

A **Platform Administrator** wants to give end users in their organization self-service access to creating GCS Buckets that triggers a Cloud Workflow when any object in it is finalized. The platform administrator creates a kro ResourceGraphDefinition called *gcsbucketwithfinalizertrigger.kro.run* that defines the required Kubernetes resources and a CRD called *GCSBucketWithFinalizertrigger* that exposes only the options they want to be configurable by end users.

The following KCC objects are created by this RGD:
* IAMServiceAccount, IAMPolicyMember: Service Account with necessary permissions for Eventarc and Pub/Sub.
* StorageBucket
* PubSubTopic
* EventArcTrigger
* StorageNotification: To publish events from the GCS bucket to a Pub/Sub topic.

Pre-requisites:
* Workflow: The workflow to be triggered on Finalizer event.

Everything related to these resources would be hidden from the end user, simplifying their experience.  

![GCS EventArc Stack](gcsbucket-with-finalizer-trigger.png)

<!--
meta {
  title "GCS Bucket Finalizer Event Processing"
}

elements {
  gcp {
      card iam {
         name "EventArc, Workflow"
      }
      group  storageA {
        name ""
        card gcs {
        name "bucket"
        }
        card pubsub {
        name "finalizer topic"
        }
        card eventarc {
        name "trigger"
        }
        card workflows {
        name "finalizer workflow"
        }
      }
  }
}

paths {
  gcs \-\-> pubsub
  pubsub \-\-> eventarc
  eventarc \-\-> workflows
}
-->



## End User: GCSBucketWithFinalizerTrigger

The administrator needs to install the RGD first.
The end user creates a `GCSBucketWithFinalizerTrigger` resource something like this:

```yaml
apiVersion: kro.run/v1alpha1
kind: GCSBucketWithFinalizerTrigger
metadata:
  name: gcsevent-test
  namespace: config-connector
spec:
  name: demo-gcs               # used as name or prefix for KCC objects
  workflowName: gcs-finalizer-workflow   # Replace with your workflow path
  location: us-central1        # desired location
  project: my-project-name     # Replace with your project name
```

They can then check the status of the applied resource:

```
kubectl get gcsbucketwithfinalizertrigger -n config-connector
kubectl get gcsbucketwithfinalizertrigger gcsevent-test -n config-connector -o yaml
```

Navigate to GCS page in the GCP Console and verify the bucket creation. Also verify that the Triggers are setup correctly in the EventArc page.

Once done, the user can delete the `GCSBucketWithFinalizerTrigger` instance:

```
kubectl delete gcsbucketwithfinalizertrigger gcsevent-test -n config-connector
```

## Administrator: ResourceGraphDefinition
The administrator needs to install the RGD in the cluster first before the user can consume it:

```
kubectl apply -f rgd.yaml
```

Validate the RGD is installed correctly:

```
kubectl get rgd gcsbucketwithfinalizertrigger.kro.run
```

Once all user created instances are deleted, the administrator can choose to deleted the RGD.

<details>
  <summary>ResourceGraphDefinition</summary>
  ```yaml title="rgd.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: gcsbucketwithfinalizertrigger.kro.run
spec:
  schema:
    apiVersion: v1alpha1
    kind: GCSBucketWithFinalizerTrigger
    spec:
      name: string
      workflowName: string
      location: string
      project: string
    status:
      url: ${bucket.status.url}
  resources:
  - id: storageEnable
    template:
      apiVersion: serviceusage.cnrm.cloud.google.com/v1beta1
      kind: Service
      metadata:
        annotations:
          cnrm.cloud.google.com/deletion-policy: "abandon"
          cnrm.cloud.google.com/disable-dependent-services: "false"
        name: storage-enablement
      spec:
        resourceID: storage.googleapis.com
  - id: iamEnable
    template:
      apiVersion: serviceusage.cnrm.cloud.google.com/v1beta1
      kind: Service
      metadata:
        annotations:
          cnrm.cloud.google.com/deletion-policy: "abandon"
          cnrm.cloud.google.com/disable-dependent-services: "false"
        name: iam-enablement
      spec:
        resourceID: iam.googleapis.com
  - id: pubsubEnable
    template:
      apiVersion: serviceusage.cnrm.cloud.google.com/v1beta1
      kind: Service
      metadata:
        annotations:
          cnrm.cloud.google.com/deletion-policy: "abandon"
          cnrm.cloud.google.com/disable-dependent-services: "false"
        name: pubsub-enablement
      spec:
        resourceID: pubsub.googleapis.com
  - id: eventarcEnable
    template:
      apiVersion: serviceusage.cnrm.cloud.google.com/v1beta1
      kind: Service
      metadata:
        annotations:
          cnrm.cloud.google.com/deletion-policy: "abandon"
          cnrm.cloud.google.com/disable-dependent-services: "false"
        name: eventarc-enablement
      spec:
        resourceID: eventarc.googleapis.com
  - id: iamsa
    template:
      apiVersion: iam.cnrm.cloud.google.com/v1beta1
      kind: IAMServiceAccount
      metadata:
        labels:
          enabled-service: ${iamEnable.metadata.name}
        #annotations:
        #  cnrm.cloud.google.com/project-id: ${schema.spec.project}
        name: ${schema.spec.name}
      spec:
        displayName: ${schema.spec.name}-eventarc-workflow
  - id: iampmEventarc
    template:
      apiVersion: iam.cnrm.cloud.google.com/v1beta1
      kind: IAMPolicyMember
      metadata:
        labels:
          enabled-service: ${iamEnable.metadata.name}
        name: ${schema.spec.name}-eventarc
      spec:
        memberFrom:
          serviceAccountRef:
            name: ${iamsa.metadata.name}
        role: roles/eventarc.admin
        resourceRef:
          kind: Project
          external: ${schema.spec.project}
  - id: iampmWorkflow
    template:
      apiVersion: iam.cnrm.cloud.google.com/v1beta1
      kind: IAMPolicyMember
      metadata:
        labels:
          enabled-service: ${iamEnable.metadata.name}
        name: ${schema.spec.name}-workflow
      spec:
        memberFrom:
          serviceAccountRef:
            name: ${iamsa.metadata.name}
        role: roles/workflows.admin
        resourceRef:
          kind: Project
          external: ${schema.spec.project}
  - id: topic
    template:
      apiVersion: pubsub.cnrm.cloud.google.com/v1beta1
      kind: PubSubTopic
      metadata:
        labels:
          enabled-service: ${pubsubEnable.metadata.name}
        name: ${schema.spec.name}-gcs-finalizer-topic
  - id: bucket
    template:
      apiVersion: storage.cnrm.cloud.google.com/v1beta1
      kind: StorageBucket
      metadata:
        labels:
          enabled-service: ${storageEnable.metadata.name}
        name: ${schema.spec.name}-${schema.spec.project}
      spec:
        uniformBucketLevelAccess: true
  - id: eventTrigger
    template:
      apiVersion: eventarc.cnrm.cloud.google.com/v1beta1
      kind: EventarcTrigger
      metadata:
        labels:
          enabled-service: ${eventarcEnable.metadata.name}
        name: ${schema.spec.name}-gcsfinalizer
      spec:
        destination:
          workflowRef:
            external: "projects/${schema.spec.project}/locations/${schema.spec.location}/workflows/${schema.spec.workflowName}"
        location: ${schema.spec.location}
        serviceAccountRef:
          name: ${iamsa.metadata.name}
        transport:
          pubsub:
            topicRef:
              name: ${topic.metadata.name}
              namespace: config-connector
        matchingCriteria:
        - attribute: "type"
          value: "google.cloud.pubsub.topic.v1.messagePublished"
        projectRef:
          external: "projects/${schema.spec.project}"
  - id: storageNotification
    template:
      apiVersion: storage.cnrm.cloud.google.com/v1beta1
      kind: StorageNotification
      metadata:
        name: ${schema.spec.name}-gcs
      spec:
        bucketRef:
          name: ${bucket.metadata.name}
        topicRef:
          name: ${topic.metadata.name}
        eventTypes:
        - "OBJECT_FINALIZE"
        payloadFormat: JSON_API_V1
  ```
</details>
