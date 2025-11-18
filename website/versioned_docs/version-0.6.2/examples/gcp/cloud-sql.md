---
sidebar_position: 406
---

# CloudSQL

This example show how you can use KRO to deploy GCP Cloud SQL instance in 2 regions as a primary and replica instances.


## End User: CloudSQL
The administrator needs to install the RGD first.
The end user creates a `CloudSQL` resource that looks like this:

```yaml
apiVersion: kro.run/v1alpha1
kind: CloudSQL
metadata:
  name: demo
  namespace: config-connector
spec:
  name: demo
  project: my-gcp-project
  primaryRegion: us-central1
  replicaRegion: us-west1
```

The status of the applied resource can be checked using:

```
kubectl get cloudsqls
kubectl get cloudsql demo -n config-connector -o yaml
```

Navigate to CloudSQL page in the GCP Console and verify the creation of primary and replica instances.

Once done, the user can delete the `CloudSQL` instance:

```
kubectl delete cloudsql demo -n config-connector
```

## Administrator: ResourceGraphDefinition
The administrator needs to install the RGD in the cluster first before the user can consume it:

```
kubectl apply -f rgd.yaml
```

Validate the RGD is installed correctly:

```
> kubectl get rgd cloudsql.kro.run
NAME               APIVERSION   KIND       STATE    AGE
cloudsql.kro.run   v1alpha1     CloudSQL   Active   44m
```

Once all user created instances are deleted, the administrator can choose to deleted the RGD.

<details>
  <summary>ResourceGraphDefinition</summary>
  ```yaml title="rgd.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: cloudsql.kro.run
spec:
  schema:
    apiVersion: v1alpha1
    kind: CloudSQL
    spec:
      name: string
      project: string
      primaryRegion: string
      replicaRegion: string
    status:
      connectionName: ${sqlPrimary.status.connectionName}
      ipAddress: ${sqlPrimary.status.firstIpAddress}
  resources:
  - id: cloudkmsEnable
    template:
      apiVersion: serviceusage.cnrm.cloud.google.com/v1beta1
      kind: Service
      metadata:
        annotations:
          cnrm.cloud.google.com/deletion-policy: "abandon"
          cnrm.cloud.google.com/disable-dependent-services: "false"
        name: cloudkms-enablement
      spec:
        resourceID: cloudkms.googleapis.com
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
  - id: serviceUsageEnable
    template:
      apiVersion: serviceusage.cnrm.cloud.google.com/v1beta1
      kind: Service
      metadata:
        annotations:
          cnrm.cloud.google.com/deletion-policy: "abandon"
          cnrm.cloud.google.com/disable-dependent-services: "false"
        name: serviceusage-enablement
      spec:
        resourceID: serviceusage.googleapis.com
  - id: sqlAdminEnable
    template:
      apiVersion: serviceusage.cnrm.cloud.google.com/v1beta1
      kind: Service
      metadata:
        annotations:
          cnrm.cloud.google.com/deletion-policy: "abandon"
          cnrm.cloud.google.com/disable-dependent-services: "false"
        name: sqladmin-enablement
      spec:
        resourceID: sqladmin.googleapis.com
  - id: serviceidentity
    template:
      apiVersion: serviceusage.cnrm.cloud.google.com/v1beta1
      kind: ServiceIdentity
      metadata:
        labels:
          enabled-service: ${serviceUsageEnable.metadata.name}
        name: sqladmin.googleapis.com
      spec:
        projectRef:
          external: ${schema.spec.project}
  - id: keyringPrimary
    template:
      apiVersion: kms.cnrm.cloud.google.com/v1beta1
      kind: KMSKeyRing
      metadata:
        labels:
          enabled-service: ${cloudkmsEnable.metadata.name}
        name: ${schema.spec.name}-primary
      spec:
        location: ${schema.spec.primaryRegion}
  - id: keyringReplica
    template:
      apiVersion: kms.cnrm.cloud.google.com/v1beta1
      kind: KMSKeyRing
      metadata:
        labels:
          enabled-service: ${cloudkmsEnable.metadata.name}
        name: ${schema.spec.name}-replica
      spec:
        location: ${schema.spec.replicaRegion}
  - id: kmskeyPrimary
    template:
      apiVersion: kms.cnrm.cloud.google.com/v1beta1
      kind: KMSCryptoKey
      metadata:
        labels:
          enabled-service: ${cloudkmsEnable.metadata.name}
          failure-zone: ${schema.spec.primaryRegion}
        name: ${schema.spec.name}-primary
      spec:
        keyRingRef:
          name: ${keyringPrimary.metadata.name}
          #namespace: {{ cloudsqls.metadata.namespace }}
        purpose: ENCRYPT_DECRYPT
        versionTemplate:
          algorithm: GOOGLE_SYMMETRIC_ENCRYPTION
          protectionLevel: SOFTWARE
        importOnly: false
  - id: kmskeyReplica
    template:
      apiVersion: kms.cnrm.cloud.google.com/v1beta1
      kind: KMSCryptoKey
      metadata:
        labels:
          enabled-service: ${cloudkmsEnable.metadata.name}
          failure-zone: ${schema.spec.replicaRegion}
        name: ${schema.spec.name}-replica
      spec:
        keyRingRef:
          name: ${keyringReplica.metadata.name}
          #namespace: {{ cloudsqls.metadata.namespace }}
        purpose: ENCRYPT_DECRYPT
        versionTemplate:
          algorithm: GOOGLE_SYMMETRIC_ENCRYPTION
          protectionLevel: SOFTWARE
        importOnly: false
  - id: iampolicymemberPrimary
    template:
      apiVersion: iam.cnrm.cloud.google.com/v1beta1
      kind: IAMPolicyMember
      metadata:
        labels:
          enabled-service: ${iamEnable.metadata.name}
        name: sql-kms-${schema.spec.primaryRegion}-policybinding
      spec:
        member: serviceAccount:${serviceidentity.status.email}
        role: roles/cloudkms.cryptoKeyEncrypterDecrypter
        resourceRef:
          kind: KMSCryptoKey
          name: ${kmskeyPrimary.metadata.name}-primary
          #namespace: {{ cloudsqls.metadata.namespace }}
  - id: iampolicymemberReplica
    template:
      apiVersion: iam.cnrm.cloud.google.com/v1beta1
      kind: IAMPolicyMember
      metadata:
        name: sql-kms-${schema.spec.replicaRegion}-policybinding
        labels:
          enabled-service: ${iamEnable.metadata.name}
      spec:
        member: serviceAccount:${serviceidentity.status.email}
        role: roles/cloudkms.cryptoKeyEncrypterDecrypter
        resourceRef:
          kind: KMSCryptoKey
          name: ${kmskeyReplica.metadata.name}-replica
          #namespace: {{ cloudsqls.metadata.namespace }}
  - id: sqlPrimary
    template:
      apiVersion: sql.cnrm.cloud.google.com/v1beta1
      kind: SQLInstance
      metadata:
        annotations:
          cnrm.cloud.google.com/deletion-policy: abandon
        labels:
          failure-zone: ${schema.spec.primaryRegion}
          enabled-service: ${sqlAdminEnable.metadata.name}
        name: ${schema.spec.name}-primary
      spec:
        databaseVersion: POSTGRES_13
        encryptionKMSCryptoKeyRef:
          external: projects/${schema.spec.project}/locations/${schema.spec.primaryRegion}/keyRings/${keyringPrimary.metadata.name}/cryptoKeys/${kmskeyPrimary.metadata.name}
        region: ${schema.spec.primaryRegion}
        settings:
          availabilityType: REGIONAL
          backupConfiguration:
            backupRetentionSettings:
              retainedBackups: 6
            enabled: true
            location: us
          diskSize: 50
          diskType: PD_SSD
          maintenanceWindow:
            day: 7
            hour: 3
          tier: db-custom-8-30720
  - id: sqlReplica
    template:
      apiVersion: sql.cnrm.cloud.google.com/v1beta1
      kind: SQLInstance
      metadata:
        annotations:
          cnrm.cloud.google.com/deletion-policy: abandon
        labels:
          failure-zone: ${schema.spec.replicaRegion}
          enabled-service: ${sqlAdminEnable.metadata.name}
        name: ${schema.spec.name}-replica
      spec:
        databaseVersion: POSTGRES_13
        encryptionKMSCryptoKeyRef:
          external: projects/${schema.spec.project}/locations/${schema.spec.replicaRegion}/keyRings/${keyringReplica.metadata.name}/cryptoKeys/${kmskeyReplica.metadata.name}
        masterInstanceRef:
          name: ${schema.spec.name}-primary
          #namespace: {{ cloudsqls.metadata.namespace }}
        region: ${schema.spec.replicaRegion}
        settings:
          availabilityType: REGIONAL
          diskSize: 50
          diskType: PD_SSD
          tier: db-custom-8-30720
  ```
</details>
