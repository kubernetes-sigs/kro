# KREP-012: Cross-Cluster Resource Orchestration via Target Field

## Summary

KRO creates resources in the same cluster where the RGD instance runs. This
means multi-cluster architectures require deploying KRO and duplicating RGDs in
every cluster. Sophisticated architectures like hub-and-spoke topologies,
multi-region deployments, edge computing and multi-environment workflows all
need centralized orchestration across distributed clusters.

This proposal introduces `Target`, enabling resources in an RGD to be created in
remote clusters. Users get centralized multi-cluster orchestration while KRO
maintains its dependency management and reconciliation guarantees.

## Motivation

KRO's current model creates all resources in the local cluster where the RGD
instance exists. If you need resources in 5 clusters, you deploy KRO 5 times
and duplicate your RGD 5 times. If you need to orchestrate dependencies across
clusters, you build custom solutions outside KRO.

Example: a multi-region application where each region runs in its own cluster,
or an edge computing platform managing hundreds of edge clusters from a central
hub, or a CI/CD pipeline deploying to dev/staging/prod clusters.

Users are forced to either duplicate infrastructure or build custom
multi-cluster orchestration - defeating KRO's purpose of simplifying complex
resource management.

## Proposal

Add a `Target` field to the RGD spec that specifies where all resources in the RGD should be created. The `Target` contains a `Cluster` field with kubeconfig credentials for accessing remote clusters.

### Key Features

1. **Cluster-scoped targeting**: Resources in an RGD can specify a target cluster
2. **Kubeconfig-based authentication**: Cluster credentials stored as Kubernetes Secrets
3. **Watch-based synchronization**: By default, KRO watches resources in remote clusters
4. **Static analysis support**: Statically defined cluster references enable validation
5. **Lifecycle management**: Full CRUD operations on remote cluster resources

### Design Principles

- **Security first**: Credentials stored as Secrets with RBAC controls
- **Kubernetes-native**: Uses standard kubeconfig format
- **Extensible**: Target structure allows future protocols beyond Cluster
- **Fail-safe**: Invalid or unreachable clusters prevent RGD from becoming ready

## Design Details

### API Changes

#### Target Field Structure

Add `Target` field to the `ResourceGraphDefinitionSpec` struct in `resourcegraphdefinition_types.go`:

```go
// ResourceGraphDefinitionSpec defines the desired state of ResourceGraphDefinition.
type ResourceGraphDefinitionSpec struct {
    // Schema defines the structure of instances created from this ResourceGraphDefinition.
    Schema *Schema `json:"schema,omitempty"`
    
    // Resources is the list of Kubernetes resources that will be created and managed.
    Resources []*Resource `json:"resources,omitempty"`
    
    // Target specifies where resources in this RGD should be created.
    // If not specified, resources are created in the same cluster as the RGD instance.
    //
    // +kubebuilder:validation:Optional
    Target *Target `json:"target,omitempty"`
}

// Target specifies the destination for resource creation.
// Currently supports Cluster targeting with future extensibility for other protocols.
type Target struct {
    // Cluster specifies a remote Kubernetes cluster as the target.
    // Exactly one target type must be specified.
    //
    // +kubebuilder:validation:Optional
    Cluster *ClusterProtocol `json:"cluster,omitempty"`
}

// ClusterProtocol defines a remote Kubernetes cluster target.
type ClusterProtocol struct {
    // KubeconfigSecretName is the name of the Secret containing kubeconfig data.
    // The Secret must exist in the namespace specified by KubeconfigSecretNamespace.
    //
    // +kubebuilder:validation:Required
    KubeconfigSecretName string `json:"kubeconfigSecretName"`
    
    // KubeconfigSecretNamespace is the namespace of the Secret.
    // If empty, defaults to kro-system for cluster-scoped resources.
    //
    // +kubebuilder:validation:Optional
    KubeconfigSecretNamespace string `json:"kubeconfigSecretNamespace,omitempty"`
}
```

### Behavior Specification

#### Cluster Reference Resolution

**Static References**: When both `KubeconfigSecretName` and `KubeconfigSecretNamespace` (if specified) are literal strings without CEL expressions, KRO resolves the Secret at RGD validation time.

**Parameterized References**: When either `KubeconfigSecretName` or `KubeconfigSecretNamespace` contains CEL expressions (e.g., `${schema.spec.clusterName}`), KRO resolves the Secret at instance reconciliation time.

#### Static Analysis

**With Static References**: KRO performs static analysis on resources targeting remote clusters, validating:
- Resource schema compatibility
- API availability in target cluster
- RBAC permissions (if determinable)

**With Parameterized References**: KRO skips static analysis for cross-cluster resources since the target cluster is unknown at RGD validation time. Validation occurs at instance reconciliation.

#### RGD Readiness Conditions

An RGD with cluster targets will not become ready if:
- Referenced kubeconfig Secret does not exist
- Kubeconfig data is malformed or invalid
- Target cluster is unreachable
- Target cluster API server returns authentication/authorization errors

The RGD status will reflect these conditions with detailed error messages.

#### Resource Synchronization

**Watch Mode (Default)**: KRO establishes watches on resources in remote clusters, receiving real-time updates when resources change. This enables:
- Immediate detection of resource state changes
- Fast reconciliation loops
- Accurate readyWhen condition evaluation

**Event Handling**: KRO watches event streams for managed objects and performs server-side apply to restore desired state when drift is detected.

#### Dependency Constraints

**Cluster References Cannot Depend on RGD Resources**: A cluster reference in the RGD's `target` field cannot have a dependency on any resource defined in the RGD. This prevents circular dependencies and ensures cluster connectivity is established before resource creation.

Example of invalid configuration:
```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: invalid-example
spec:
  target:
    cluster:
      kubeconfigSecretName: ${clusterSecret.metadata.name}  # INVALID: target depends on RGD resource
  resources:
    - id: clusterSecret
      template:
        apiVersion: v1
        kind: Secret
        metadata:
          name: edge-cluster-kubeconfig
          namespace: kro-system
        stringData:
          kubeconfig: |
            # ... kubeconfig content
    
    - id: remoteResource
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: remote-config
```

The RGD's target references a Secret created by a resource in the same RGD, creating a circular dependency. The kubeconfig Secret must exist before the RGD is instantiated.

#### Deletion Behavior

**Graceful Deletion**: When an RGD instance is deleted, KRO deletes resources in reverse topological order, respecting cross-cluster dependencies.

**Cluster Unavailability**: If a target cluster becomes unreachable during deletion, the RGD deletion will hang until:
- Cluster access is restored, or
- Manual intervention removes finalizer on instance object

This prevents orphaned resources in remote clusters without manual intervention.

### Kubeconfig Secret Format

Kubeconfig credentials are stored as standard Kubernetes Secrets with the kubeconfig data in the `kubeconfig` key:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: edge-cluster-kubeconfig
  namespace: kro-system
type: Opaque
stringData:
  kubeconfig: |
    apiVersion: v1
    kind: Config
    clusters:
    - cluster:
        certificate-authority-data: LS0tLS1CRUdJTi...
        server: https://edge-cluster.example.com:6443
      name: edge-cluster
    contexts:
    - context:
        cluster: edge-cluster
        user: kro-controller
      name: edge-cluster
    current-context: edge-cluster
    users:
    - name: kro-controller
      user:
        token: eyJhbGciOiJSUzI1NiIsImtpZCI6...
```

### Error Handling

Error handling for cross-cluster operations is complex and will be addressed in follow-up work. Initial implementation will:
- Surface errors in RGD and Instance status conditions
- Include cluster name and resource identifier in error messages
- Retry failed operations with exponential backoff

Customizing error handling logic (e.g., ignore errors from specific clusters, continue on partial failure) is deferred to future work.

## Examples

### Example 1: Multi-Region Application Deployment

Deploy an application to multiple regional clusters:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: multi-region-app
spec:
  target:
    cluster:
      kubeconfigSecretName: ${schema.spec.targetCluster + '-kubeconfig'}
  schema:
    apiVersion: v1alpha1
    kind: MultiRegionApp
    spec:
      appName: string
      targetCluster: string
  resources:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.spec.appName}
          namespace: default
        spec:
          replicas: 3
          selector:
            matchLabels:
              app: ${schema.spec.appName}
          template:
            metadata:
              labels:
                app: ${schema.spec.appName}
            spec:
              containers:
              - name: app
                image: myapp:latest
```

Instance:
```yaml
apiVersion: v1alpha1
kind: MultiRegionApp
metadata:
  name: my-app-us-east
spec:
  appName: web-service
  targetCluster: us-east
---
apiVersion: v1alpha1
kind: MultiRegionApp
metadata:
  name: my-app-us-west
spec:
  appName: web-service
  targetCluster: us-west
```

Required Secrets:
```yaml
---
apiVersion: v1
kind: Secret
metadata:
  name: us-east-cluster-kubeconfig
  namespace: kro-system
type: Opaque
stringData:
  kubeconfig: |
    # kubeconfig for us-east cluster
---
apiVersion: v1
kind: Secret
metadata:
  name: us-west-cluster-kubeconfig
  namespace: kro-system
type: Opaque
stringData:
  kubeconfig: |
    # kubeconfig for us-west cluster
---
apiVersion: v1
kind: Secret
metadata:
  name: eu-central-cluster-kubeconfig
  namespace: kro-system
type: Opaque
stringData:
  kubeconfig: |
    # kubeconfig for eu-central cluster
```

### Example 2: Static Cluster Reference

Deploy to a known remote cluster with static reference:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: production-database
spec:
  target:
    cluster:
      kubeconfigSecretName: production-cluster-kubeconfig  # Static reference
  schema:
    apiVersion: v1alpha1
    kind: ProductionDatabase
    spec:
      databaseName: string
  resources:
    - id: database
      template:
        apiVersion: databases.example.com/v1
        kind: PostgreSQL
        metadata:
          name: ${schema.spec.databaseName}
        spec:
          version: "15"
          storage: 100Gi
```

In this example, KRO performs static analysis on the PostgreSQL resource against the production cluster during RGD validation.

### Example 3: Delegated Cluster Provisioning

Outer RGD provisions a cluster and creates kubeconfig, then delegates to inner RGD to deploy workloads:

```yaml
# Outer RGD: Provisions cluster and delegates deployment
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: cluster-provisioner
spec:
  schema:
    apiVersion: v1alpha1
    kind: ProvisionedCluster
    spec:
      clusterName: string
      workloadImage: string
  resources:
    - id: provisionJob
      template:
        apiVersion: batch/v1
        kind: Job
        metadata:
          name: ${schema.spec.clusterName + '-provision'}
          namespace: kro-system
        spec:
          template:
            spec:
              serviceAccountName: cluster-provisioner
              containers:
              - name: provision
                image: cluster-provisioner:latest
                env:
                - name: CLUSTER_NAME
                  value: ${schema.spec.clusterName}
                - name: SECRET_NAME
                  value: ${schema.spec.clusterName + '-kubeconfig'}
              restartPolicy: OnFailure
      readyWhen:
        - ${provisionJob.status.succeeded == 1}
    
    - id: workloadDeployment
      template:
        apiVersion: example.com/v1alpha1
        kind: RemoteWorkload
        metadata:
          name: ${schema.spec.clusterName + '-workload'}
        spec:
          targetCluster: ${schema.spec.clusterName}
          image: ${schema.spec.workloadImage}
```

```yaml
# Inner RGD: Deploys workloads to provisioned cluster
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: remote-workload
spec:
  target:
    cluster:
      kubeconfigSecretName: ${schema.spec.targetCluster + '-kubeconfig'}
      kubeconfigSecretNamespace: kro-system
  schema:
    apiVersion: v1alpha1
    kind: RemoteWorkload
    spec:
      targetCluster: string
      image: string
  resources:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: workload
          namespace: default
        spec:
          replicas: 2
          selector:
            matchLabels:
              app: workload
          template:
            metadata:
              labels:
                app: workload
            spec:
              containers:
              - name: app
                image: ${schema.spec.image}
    
    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: workload
          namespace: default
        spec:
          selector:
            app: workload
          ports:
          - port: 80
            targetPort: 8080
```

Instance:
```yaml
apiVersion: v1alpha1
kind: ProvisionedCluster
metadata:
  name: dev-cluster-1
spec:
  clusterName: dev-cluster-1
  workloadImage: myapp:v1.0
```

This example demonstrates:
- Outer RGD creates a Job that provisions a cluster and generates a kubeconfig Secret
- Outer RGD waits for Job completion before creating inner RGD instance
- Inner RGD references the dynamically created kubeconfig to deploy resources
- Separation of concerns: cluster provisioning vs workload deployment

## Security Considerations

### Credential Storage

**Secrets**: Kubeconfig credentials are stored as Kubernetes Secrets, leveraging existing Secret encryption and RBAC mechanisms.

**RBAC**: Access to kubeconfig Secrets should be restricted using RBAC policies. Only the KRO controller service account should have read access.

**Secret Rotation**: Kubeconfig Secrets can be updated independently of RGDs. KRO will detect Secret changes and re-establish cluster connections.

### Cluster Access Permissions

**Least Privilege**: Kubeconfigs should grant only the minimum permissions required for KRO to manage resources in target clusters.

**Service Accounts**: Prefer service account tokens over user credentials in kubeconfigs.

**Certificate Validation**: KRO validates TLS certificates when connecting to remote clusters.

### Audit Logging

Cross-cluster operations should be auditable:
- Log all cluster connection attempts
- Record resource creation/update/deletion in remote clusters
- Include cluster identity in audit logs

## Backward Compatibility

This change is fully backward compatible:
- The `Target` field is optional
- Existing RGDs without `Target` fields continue to work unchanged
- Resources without `Target` are created in the local cluster (current behavior)

## Scope

### In Scope

- Add `Target` field to RGD spec (ResourceGraphDefinitionSpec)
- Implement `Cluster` target type with kubeconfig Secret references
- Watch-based synchronization for remote cluster resources
- Static analysis for statically defined cluster references
- RGD validation for cluster connectivity
- Deletion handling with cluster unavailability detection
- Documentation and examples

### Not in Scope

- `Target` field on ExternalRef schema (Future Directions)
- `Target` field on individual Resource objects (Future Directions)
- Poll-based synchronization (Future Directions)
- Custom error handling logic (follow-up work)
- Automatic Secret rotation
- Cross-cluster networking setup

## Required Dependencies

### 1. Enhanced Instance Status Conditions

**Motivation**: Cross-cluster orchestration requires detailed per-resource status tracking to enable effective error handling and lifecycle controls.

**Requirements**:
- Instance objects must track status for each managed resource individually
- Status must include:
  - Last update time
  - Last observed generation
  - Current state (Ready, Pending, Error, etc.)
  - Detailed error messages when KRO cannot read or write objects
- Error messages must be detailed enough for use in CEL expressions
- Error messages must identify which resource failed and the failure reason

**Impact on Cross-Cluster**: Without enhanced status, users cannot:
- Determine which cluster or resource is causing failures
- Implement conditional logic based on resource-specific errors
- Track resource state across multiple clusters effectively

### 2. Lifecycle Controls

**Motivation**: Cross-cluster orchestration creates more numerous and complicated error cases that require dynamic behavior modification.

**Requirements**:
- RGD authors can change RGD behavior dynamically based on Instance status
- Lifecycle controls work with enhanced status conditions (Dependency #1)
- Supported lifecycle events:
  - Change resource properties (e.g., replica count) based on error classification
  - Skip deletion of specific managed resources when Instance is deleted
  - Force deletion of resources after timeout
  - Change deletion order based on runtime conditions
  - Create cleanup resources when Instance is deleted (e.g., backup jobs)

**Impact on Cross-Cluster**: Lifecycle controls enable:
- Graceful degradation when clusters become unavailable
- Conditional resource creation based on cluster health
- Cleanup operations that span multiple clusters
- Recovery strategies for partial failures

**Example Use Case**: When a regional cluster becomes unavailable, lifecycle controls can:
1. Detect the error from enhanced status
2. Skip resources targeting that cluster
3. Continue managing resources in healthy clusters
4. Create backup resources in alternative clusters

## Future Directions

These items are explicitly excluded from this KREP and will be addressed in future work:

### 1. Poll-Based Synchronization (PollConfig)

**Motivation**: Watch-based synchronization can lead to scaling issues when managing 50+ clusters.

**Proposal**: Add `PollConfig` parameter to ClusterProtocol schema:

```go
type ClusterProtocol struct {
    KubeconfigSecretName string `json:"kubeconfigSecretName"`
    KubeconfigSecretNamespace string `json:"kubeconfigSecretNamespace,omitempty"`
    
    // PollConfig enables polling instead of watching for this cluster.
    // +kubebuilder:validation:Optional
    PollConfig *PollConfig `json:"pollConfig,omitempty"`
}

type PollConfig struct {
    // Interval is the polling interval.
    Interval metav1.Duration `json:"interval"`
}
```

**Requirements**: Depends on Enhanced Instance Status (Required Dependency #1) to track object Generation values and determine which objects have changed.

### 2. Target Field on ExternalRef Schema

**Motivation**: Enable cross-cluster watch semantics for external references.

**Proposal**: Add `Target` field to ExternalRef:

```go
type ExternalRef struct {
    APIVersion string `json:"apiVersion"`
    Kind string `json:"kind"`
    Metadata ExternalRefMetadata `json:"metadata"`
    
    // Target specifies a remote cluster to read from.
    // +kubebuilder:validation:Optional
    Target *Target `json:"target,omitempty"`
}
```

**Use Case**: Reference ConfigMaps or Secrets from a central configuration cluster.

### 3. Target Field on Individual Resource Objects

**Motivation**: Allow fine-grained control where different resources in the same RGD can target different clusters.

**Proposal**: Add `Target` field to individual Resource objects in addition to RGD-level target.

**Use Case**: Mixed deployments where some resources go to one cluster and others to different clusters within the same RGD.

### 4. Additional Target Protocols

**Motivation**: Support non-Kubernetes targets for resource orchestration.

**Examples**:
- HTTP/REST APIs
- Cloud provider APIs (AWS, GCP, Azure)
- Database connections
- Message queues

**Design**: The `Target` struct is designed to be extensible with additional protocol types beyond `Cluster`.

## Alternatives Considered

### Delegate to External Operator

**Description**: KRO could delegate cross-cluster access to a different Kubernetes operator specialized in multi-cluster management.

**Rejection Rationale**:

Such an operator would require either:

1. **Monkey Patching**: Modifying existing Kubernetes types to add cross-cluster awareness. This is infeasible due to API compatibility constraints.

2. **Wrapper Types**: Creating wrapper types that encapsulate existing Kubernetes resources with cross-cluster metadata.

The wrapper approach has very narrow utility because:
- Using wrapper types requires a powerful object orchestrator capable of introspecting wrappers to understand their contents
- KRO is precisely that powerful object orchestrator
- Combining cross-cluster functionality with KRO's resource orchestration capabilities is more efficient than maintaining two separate systems

**Conclusion**: Since KRO already has the orchestration capabilities needed to manage cross-cluster resources effectively, integrating cross-cluster functionality directly into KRO is the superior approach.

**Note**: Deploying Instance objects themselves to remote clusters (as opposed to the resources they manage) is deferred to future work. This KREP focuses on deploying resources defined in RGDs to remote clusters while keeping Instance objects in the control plane cluster.
