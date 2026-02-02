# KREP-007: Cluster References for Cross-Cluster Resource Orchestration

## Summary

KRO currently orchestrates resources within a single Kubernetes cluster. This proposal introduces cluster references to enable cross-cluster resource orchestration, allowing ResourceGraphDefinitions to create, manage, and reference resources across multiple Kubernetes clusters. Cluster references are defined at the RGD level and use kubeconfig-based authentication stored in Kubernetes Secrets. By default, KRO watches resources in remote clusters for changes, with polling support deferred to future work.

## Motivation

Modern Kubernetes architectures increasingly span multiple clusters for high availability, geographic distribution, environment separation, and blast radius reduction. Organizations deploy hub-and-spoke topologies, multi-region setups, and hybrid cloud configurations where resources in one cluster depend on or complement resources in others.

Today, orchestrating resources across clusters requires external tooling, custom operators, or manual coordination. Users cannot express cross-cluster dependencies or lifecycle management within a single ResourceGraphDefinition. This forces them to:

- Maintain separate RGDs per cluster with manual coordination
- Build custom controllers to handle cross-cluster logic
- Use external orchestration tools that lack KRO's dependency management
- Accept operational complexity and reduced reliability

### Use Cases

**Multi-Region Application Deployment**: Deploy application components across regional clusters while maintaining centralized configuration and dependency management. A control plane cluster manages RGD instances that deploy workloads to edge clusters based on geographic requirements.

**Shared Infrastructure References**: Reference shared infrastructure resources (databases, message queues, configuration) from a central platform cluster while deploying application workloads to tenant clusters.

**Environment Promotion**: Define promotion workflows where resources in development clusters trigger creation of corresponding resources in staging and production clusters, with proper dependency ordering.

## Proposal

### Overview

This KREP introduces a `cluster` field to the ResourceGraphDefinition schema that specifies remote cluster access configuration. Cluster references enable KRO to:

- Create and manage resources in remote Kubernetes clusters
- Watch remote cluster resources for changes and reconcile accordingly
- Maintain dependency ordering across cluster boundaries
- Validate remote cluster accessibility during RGD reconciliation (for static cluster references)

### Core Concepts

**Cluster Reference Scope**: Cluster references apply to all resources within an RGD. When a cluster reference is specified, all resources defined in that RGD are created in the referenced cluster, not the cluster where the RGD instance exists.

**Watch-Based Reconciliation**: By default, KRO establishes watches on resources in remote clusters, receiving real-time notifications of changes. This maintains KRO's existing reconciliation model across cluster boundaries. (See Future Directions for more information)

**Static vs. Parameterized References**: All ClusterRef fields (Name, Namespace, Key) support CEL expressions that evaluate instance spec fields. Static references enable validation at RGD creation time, while parameterized references provide runtime flexibility. Fields can mix static and CEL values (e.g., static Name with CEL Namespace).

**Kubeconfig Storage**: Remote cluster credentials are stored as Kubernetes Secrets in the cluster where KRO runs. This leverages existing Kubernetes RBAC and secret management capabilities.

**Dependency Constraints**: Cluster references specified at the RGD level cannot depend on resources defined within that RGD. This prevents circular dependencies and ensures cluster selection happens before resource creation.

## API Changes

### ClusterRef Field

Add a new `ClusterRef` type and field to the ResourceGraphDefinition schema:

```go
// ClusterRef specifies a remote Kubernetes cluster where resources should be created.
// When specified, all resources in the RGD are created in the referenced cluster
// instead of the cluster where the RGD instance exists.
//
// All fields support CEL expressions using ${...} syntax to enable dynamic cluster
// selection based on instance spec fields.
type ClusterRef struct {
	// Name is the name of the Secret containing kubeconfig data.
	// Can be either:
	// - A static Secret name: "my-kubeconfig-secret"
	// - A CEL expression: "${schema.spec.targetCluster}"
	//
	// For static references, the Secret must exist before the RGD is created.
	// For CEL expressions, the Secret must exist when instances are created.
	//
	// +kubebuilder:validation:Required
	Name string `json:"name,omitempty"`

	// Namespace is the namespace of the Secret containing kubeconfig data.
	// Can be either:
	// - A static namespace: "kro-system"
	// - A CEL expression: "${schema.spec.clusterNamespace}"
	// - Empty: defaults to the instance's namespace
	//
	// +kubebuilder:validation:Optional
	Namespace string `json:"namespace,omitempty"`

	// Key within the Secret that contains the kubeconfig data.
	// Can be either:
	// - A static key: "kubeconfig"
	// - A CEL expression: "${schema.spec.kubeconfigKey}"
	// - Empty: defaults to "kubeconfig"
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:default="kubeconfig"
	Key string `json:"key,omitempty"`
}
```

Add the `Cluster` field to ResourceGraphDefinitionSpec:

```go
type ResourceGraphDefinitionSpec struct {
	// Schema defines the structure of instances created from this ResourceGraphDefinition.
	Schema *Schema `json:"schema,omitempty"`

	// Resources is the list of Kubernetes resources that will be created and managed.
	Resources []*Resource `json:"resources,omitempty"`

	// Cluster specifies a remote Kubernetes cluster where all resources in this RGD
	// should be created. When specified, KRO connects to the remote cluster using
	// the provided kubeconfig and manages resources there instead of the local cluster.
	//
	// Cluster references cannot depend on resources defined in this RGD. For static
	// cluster references, KRO validates cluster accessibility when the RGD is created.
	// For parameterized references (using CEL), validation occurs when instances are
	// created.
	//
	// +kubebuilder:validation:Optional
	Cluster *ClusterRef `json:"cluster,omitempty"`
}
```

### Validation Rules

The Name field is required and validated at the API level. All fields support CEL expressions, detected at RGD creation time by checking for `${...}` syntax.

## Examples

### Example 1: Static Cluster Reference

Deploy resources to a fixed remote cluster using a static kubeconfig Secret:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: edge-application
spec:
  cluster:
    name: us-west-edge-kubeconfig
    namespace: kro-system
  schema:
    apiVersion: v1alpha1
    kind: EdgeApp
    spec:
      replicas: "integer | default=3"
      image: "string"
  resources:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.metadata.name}
          namespace: ${schema.metadata.namespace}
        spec:
          replicas: ${schema.spec.replicas}
          selector:
            matchLabels:
              app: ${schema.metadata.name}
          template:
            metadata:
              labels:
                app: ${schema.metadata.name}
            spec:
              containers:
                - name: app
                  image: ${schema.spec.image}
    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${schema.metadata.name}
          namespace: ${schema.metadata.namespace}
        spec:
          selector:
            app: ${schema.metadata.name}
          ports:
            - port: 80
              targetPort: 8080
```

The kubeconfig Secret must exist before the RGD is created.

### Example 2: Parameterized Cluster Reference

Allow instance creators to specify the target cluster dynamically:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: multi-region-app
spec:
  cluster:
    name: ${schema.spec.targetCluster}
    namespace: ${schema.spec.clusterNamespace}
  schema:
    apiVersion: v1alpha1
    kind: MultiRegionApp
    spec:
      targetCluster: "string"
      clusterNamespace: "string"
      replicas: "integer | default=3"
      image: "string"
  resources:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.metadata.name}
        spec:
          replicas: ${schema.spec.replicas}
          selector:
            matchLabels:
              app: ${schema.metadata.name}
          template:
            metadata:
              labels:
                app: ${schema.metadata.name}
            spec:
              containers:
                - name: app
                  image: ${schema.spec.image}
```

Users create instances specifying the target cluster:

```yaml
apiVersion: example.com/v1alpha1
kind: MultiRegionApp
metadata:
  name: my-app-us-east
spec:
  targetCluster: us-east-cluster-kubeconfig
  clusterNamespace: kro-system
  replicas: 5
  image: myapp:v1.2.3
---
apiVersion: example.com/v1alpha1
kind: MultiRegionApp
metadata:
  name: my-app-eu-west
spec:
  targetCluster: eu-west-cluster-kubeconfig
  clusterNamespace: kro-system
  replicas: 3
  image: myapp:v1.2.3
```

### Example 3: Mixed Static and Dynamic Fields

Use static namespace with dynamic Secret name and key:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: flexible-app
spec:
  cluster:
    name: ${schema.spec.secretName}
    namespace: kro-system
    key: ${schema.spec.kubeconfigKey}
  schema:
    apiVersion: v1alpha1
    kind: FlexibleApp
    spec:
      secretName: "string"
      kubeconfigKey: "string | default=kubeconfig"
      image: "string"
  resources:
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
              containers:
                - name: app
                  image: ${schema.spec.image}
```

### Kubeconfig Secret Structure

Kubeconfig Secrets must contain a `kubeconfig` key with valid kubeconfig YAML. The kubeconfig should specify a single context that KRO will use:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: production-cluster-kubeconfig
  namespace: kro-system
type: Opaque
stringData:
  kubeconfig: |
    apiVersion: v1
    kind: Config
    clusters:
      - name: production
        cluster:
          server: https://prod-api.example.com:6443
          certificate-authority-data: LS0tLS1CRUdJTi...
    users:
      - name: kro-service-account
        user:
          token: eyJhbGciOiJSUzI1NiIsImtpZCI6Ii...
    contexts:
      - name: production
        context:
          cluster: production
          user: kro-service-account
          namespace: default
    current-context: production
```

KRO supports multiple authentication methods in kubeconfig:
- Client certificates (`client-certificate-data`, `client-key-data`)
- Bearer tokens (`token`)
- Exec-based authentication (`exec`)
- Username/password (not recommended)

## Design Details

### RGD Validation and Static Analysis

When an RGD with a cluster reference is created or updated:

1. **CEL Expression Detection**: Check each ClusterRef field for `${...}` syntax:
   - Fields with `${...}` are treated as CEL expressions (parameterized)
   - Fields without `${...}` are treated as static values
   - A ClusterRef is fully static only if all fields are static

2. **Static Reference Validation**: For fully static references (no CEL in any field):
   - Verify the referenced Secret exists in the specified namespace
   - Verify the Secret contains the specified Key (default: "kubeconfig")
   - Parse the kubeconfig and validate its structure
   - Attempt to connect to the remote cluster and verify accessibility
   - If the cluster is unreachable or the kubeconfig is invalid, mark the RGD as not ready with a descriptive error condition

3. **Parameterized Reference Handling**: For any field containing CEL expressions:
   - Validate all CEL expression syntax
   - Verify expressions reference only instance spec fields (not resources in the RGD)
   - Skip cluster connectivity validation (cannot be performed until instance creation)
   - Mark the RGD as ready if all CEL expressions are valid

4. **Resource Static Analysis**: For fully static cluster references:
   - Perform static analysis on resources using the remote cluster's API server
   - Validate resource schemas against the remote cluster's CRDs and API versions
   - Report validation errors in RGD status conditions

5. For any parameterized cluster references, skip static analysis for resources that will be created in the remote cluster

### Instance Reconciliation

When an instance of an RGD with a cluster reference is created:

1. **Cluster Resolution**: 
   - Evaluate all ClusterRef fields, resolving any CEL expressions
   - For fully static references, use the pre-validated cluster connection
   - For parameterized fields, evaluate CEL expressions to determine values
   - Retrieve the kubeconfig Secret from the resolved namespace using the resolved Key
   - Parse the kubeconfig and establish a connection to the remote cluster
   - If the cluster is unreachable, update instance status with an error condition and requeue

2. **Resource Creation**:
   - Create a Kubernetes client for the remote cluster using the kubeconfig
   - Process resources in dependency order (existing topological sorting)
   - Apply resources to the remote cluster instead of the local cluster
   - Establish watches on remote cluster resources for change notifications

3. **Watch Management**:
   - Create informers for each resource type in the remote cluster
   - Register event handlers to trigger reconciliation when remote resources change
   - Maintain watch connections with automatic reconnection on failure
   - Update instance status based on remote resource states

4. **Status Synchronization**:
   - Read resource status from the remote cluster
   - Evaluate `readyWhen` conditions against remote resource state
   - Project status fields from remote resources to instance status
   - Update instance status conditions to reflect remote cluster state

### Deletion Handling

When an instance with cluster references is deleted:

1. **Resource Deletion**:
   - Connect to the remote cluster using the kubeconfig
   - Delete resources in reverse dependency order (existing deletion logic)
   - Wait for each resource to be fully deleted before proceeding

2. **Cluster Unreachability**:
   - If the remote cluster is unreachable during deletion, the instance deletion will hang
   - KRO will continuously retry connecting to the cluster
   - Instance finalizer will not be removed until all remote resources are confirmed deleted
   - This prevents orphaned resources in remote clusters
   - Users must restore cluster connectivity to complete deletion

3. **Kubeconfig Secret Deletion**:
   - If the kubeconfig Secret is deleted while instances exist, mark instances as degraded
   - Continue attempting reconciliation with exponential backoff
   - Do not remove instance finalizers until remote resources are confirmed deleted

### Error Handling and Status Conditions

Cluster reference operations introduce new error scenarios that must be surfaced to users:

**RGD Status Conditions**:
- `ClusterAccessible`: Indicates whether the remote cluster is reachable (static references only)
- `ClusterValidated`: Indicates whether the kubeconfig is valid and authentication succeeds
- `StaticAnalysisComplete`: Indicates whether resource validation against the remote cluster succeeded

**Instance Status Conditions**:
- `ClusterResolved`: Indicates whether the cluster reference was successfully resolved (parameterized references)
- `RemoteResourcesReady`: Indicates whether all resources in the remote cluster are ready
- `RemoteClusterConnected`: Indicates current connectivity status to the remote cluster

Error messages must include:
- The cluster reference namespace/name
- The specific operation that failed (connect, create, update, delete, watch)
- The underlying error from the Kubernetes client
- Guidance on resolution (e.g., "verify kubeconfig Secret exists and contains valid credentials")

### Security Considerations

**Kubeconfig Secret Access**: KRO requires RBAC permissions to read Secrets in namespaces where cluster references are defined. This should be limited to Secrets with a specific label or naming convention to prevent unauthorized access to unrelated Secrets. The Namespace field allows flexibility in Secret placement while maintaining security boundaries.

**Remote Cluster Permissions**: The credentials in kubeconfig Secrets must have appropriate RBAC permissions in remote clusters. KRO should document recommended RBAC configurations for remote cluster service accounts.

**Secret Rotation**: When kubeconfig Secrets are updated (e.g., certificate rotation), KRO must detect the change and re-establish connections. This requires watching the kubeconfig Secrets for changes.

**Audit Logging**: All cross-cluster operations should be logged with sufficient detail for security auditing, including cluster names, operations performed, and authentication methods used.

**Network Security**: Cross-cluster communication should use TLS with certificate validation. KRO should reject kubeconfig files that disable certificate verification.

## Scope

### In Scope

- `cluster` field on ResourceGraphDefinition schema
- Static cluster references using `kubeconfigSecret`
- Parameterized cluster references using `kubeconfigSecretRef` with CEL expressions
- Watch-based reconciliation for remote cluster resources
- Kubeconfig storage in Kubernetes Secrets
- Static analysis for statically defined cluster references
- Validation of cluster accessibility for static references
- Error handling and status conditions for cluster operations
- Deletion handling with cluster unreachability protection
- Documentation and examples

### Out of Scope

The following are explicitly out of scope for this KREP and are addressed in the Future Directions section:

- **Polling-based reconciliation**: Alternative to watches for high-scale scenarios (50+ clusters)
- **Cluster references on individual resources**: Allows simplified RGD structure for cross cluster deployments
- **Cluster references on ExternalRef**: Reading and watching external resources from remote clusters
- **Custom error handling logic**: Advanced error recovery and retry strategies
- **Lifecycle controls**: Dynamic behavior modification based on error conditions

### Non-Goals

- Replace dedicated multi-cluster management tools (Cluster API, Fleet, ArgoCD)
- Provide cluster provisioning or lifecycle management
- Implement custom service mesh or networking solutions
- Support non-Kubernetes target systems

## Required Dependencies

This KREP has two required dependencies that must be implemented before or alongside cluster references:

### Dependency 1: Enhanced Status Tracking

**Requirement**: KRO must track detailed status for each managed resource, including last update time, last observed generation, and error messages.

**Rationale**: Cross-cluster orchestration introduces more complex error scenarios. When a remote cluster becomes unreachable or a resource fails to apply, users need detailed information about which specific resource failed and why. Current status tracking is insufficient for debugging cross-cluster issues.

**Required Enhancements**:
- Per-resource status conditions on Instance objects
- Last update timestamp for each resource
- Last observed generation to detect changes
- Detailed error messages including resource identity (GVK, namespace, name)
- Error messages must be accessible via CEL expressions for lifecycle controls

**Impact on Cluster References**: 
- Enables users to identify which resources in which clusters are failing
- Supports future lifecycle controls that react to specific error conditions
- Provides observability for cross-cluster operations

**Implementation Note**: KRO currently watches event streams and performs server-side apply without comparing object generations. For cross-cluster polling (future work), KRO will need to list resources and events as well as compare generation values to detect changes. Enhanced status tracking is a prerequisite for this capability.

### Dependency 2: Lifecycle Controls

**Requirement**: RGD authors must be able to define dynamic behavior based on resource status and error conditions.

**Rationale**: Cross-cluster orchestration creates more numerous and complex error cases. A single unreachable cluster may not justify blocking other operations. RGD authors need control over how errors are handled, how resources are deleted, and how changes propagate.

**Required Capabilities**:
- Conditional resource scaling based on error classification (e.g., reduce replicas when a dependency fails)
- Selective resource deletion (e.g., skip deleting certain resources when instance is deleted)
- Forced deletion after timeout (e.g., remove finalizer after 5 minutes if cluster unreachable)
- Custom deletion ordering based on runtime conditions
- Temporary resource creation during deletion (e.g., backup jobs before cleanup)

**Impact on Cluster References**:
- Allows graceful degradation when remote clusters are unreachable
- Enables sophisticated deletion strategies for cross-cluster resources
- Supports backup and cleanup workflows during instance deletion
- Provides flexibility for handling partial failures

**Integration with Enhanced Status**: Lifecycle controls use CEL expressions that reference the enhanced status conditions from Dependency 1. For example, a lifecycle control might check `${database.status.error.contains('connection refused')}` to determine whether to skip deletion.

## Future Directions

The following enhancements are explicitly deferred to future work and must not be included in the initial implementation:

### 1. Polling-Based Reconciliation

**Motivation**: Watch-based reconciliation can create scaling challenges when managing 50+ remote clusters. Each watch maintains a persistent connection, consuming memory and network resources.

**Proposal**: Add a `pollConfig` field to ClusterRef:

```go
type ClusterRef struct {
	Name                string            `json:"name"`
	KubeconfigSecret    *SecretReference  `json:"kubeconfigSecret,omitempty"`
	KubeconfigSecretRef string            `json:"kubeconfigSecretRef,omitempty"`
	
	// PollConfig enables polling instead of watching for resource changes.
	// When specified, KRO periodically lists resources and compares generation
	// values to detect changes instead of maintaining persistent watch connections.
	//
	// +kubebuilder:validation:Optional
	PollConfig *PollConfig `json:"pollConfig,omitempty"`
}

type PollConfig struct {
	// Interval specifies how frequently to poll the remote cluster.
	// Minimum value is 10s to prevent excessive API server load.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Format=duration
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(s|m|h))+$`
	Interval metav1.Duration `json:"interval"`
}
```

**Requirements**: Depends on Enhanced Status Tracking (Dependency 1) to track resource generations and detect changes.

### 2. Cluster References on ExternalRef

**Motivation**: Enable reading existing resources from remote clusters without managing them.

**Proposal**: Add `cluster` field to ExternalRef:

```go
type ExternalRef struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Metadata   ExternalRefMetadata `json:"metadata"`
	
	// Cluster specifies a remote cluster to read this resource from.
	// When specified, KRO reads the resource from the remote cluster
	// instead of the local cluster.
	//
	// +kubebuilder:validation:Optional
	Cluster *ClusterRef `json:"cluster,omitempty"`
}
```

**Use Case**: Reference shared configuration or infrastructure from a central platform cluster while deploying application resources to tenant clusters.

### 3. Cluster References on Resource Schema

**Motivation**: Simplify RGD structure when only some resources need to be created in remote clusters.

**Proposal**: Add `cluster` field to Resource:

```go
type Resource struct {
	ID          string               `json:"id,omitempty"`
	Template    runtime.RawExtension `json:"template,omitempty"`
	ExternalRef *ExternalRef         `json:"externalRef,omitempty"`
	ReadyWhen   []string             `json:"readyWhen,omitempty"`
	IncludeWhen []string             `json:"includeWhen,omitempty"`
	
	// Cluster specifies a remote cluster where this specific resource
	// should be created. Overrides the RGD-level cluster reference.
	//
	// +kubebuilder:validation:Optional
	Cluster *ClusterRef `json:"cluster,omitempty"`
}
```

**Use Case**: Create most resources locally but deploy specific resources (e.g., edge workloads) to remote clusters.


### 4. External References on Schema Schema

**Motivation**: Allows existing types to be used as Instance objects. Allows a more natural implementation of decorator pattern for use with [KREP-003](https://github.com/kubernetes-sigs/kro/pull/738)

**Proposal**: Add 'externalRef' to `schema`

```go
type Schema struct {
	Kind string `json:"kind,omitempty"`
	APIVersion string `json:"apiVersion,omitempty"`
	Group string `json:"group,omitempty"`
	Spec runtime.RawExtension `json:"spec,omitempty"`
	Types runtime.RawExtension `json:"types,omitempty"`
	Status runtime.RawExtension `json:"status,omitempty"`
	AdditionalPrinterColumns []extv1.CustomResourceColumnDefinition `json:"additionalPrinterColumns,omitempty"`

    // ExternalRef references an existing resource instead of creating one.
	// Exactly one of spec or externalRef must be provided.
	//
	// +kubebuilder:validation:Optional
	ExternalRef *ExternalRef `json:"externalRef,omitempty"`
}
```


## Alternatives Considered

### Alternative 1: Delegate to External Operator

**Approach**: KRO could delegate cross-cluster access to a separate Kubernetes operator that specializes in multi-cluster management.

**Trade-offs**:
- **Rejected**: This approach requires either monkey-patching existing Kubernetes types or wrapping them in a custom type
- Monkey-patching is infeasible and violates Kubernetes API conventions
- A wrapper type has limited utility because consumers need to understand the wrapped content
- Since KRO already introspects and orchestrates resources, combining cross-cluster functionality with KRO's orchestration capabilities is more natural
- Reduces operational complexity by avoiding an additional operator dependency

### Alternative 2: Instance-Level Cluster References

**Approach**: Allow instances to specify target clusters instead of defining them in the RGD.

**Trade-offs**:
- **Rejected**: Prevents static analysis and validation at RGD creation time
- Cannot verify cluster accessibility or resource schemas until instance creation
- Reduces safety guarantees and increases likelihood of runtime errors
- Makes RGD behavior less predictable and harder to reason about
- The parameterized cluster reference approach provides sufficient flexibility while maintaining validation capabilities

### Alternative 3: Implicit Cluster Discovery

**Approach**: Automatically discover clusters using labels or annotations instead of explicit kubeconfig references.

**Trade-offs**:
- **Rejected**: Adds complexity and reduces explicitness
- Requires additional CRDs or conventions for cluster registration
- Makes it harder to understand which clusters an RGD will interact with
- Complicates RBAC and security auditing
- Explicit kubeconfig references are clearer and more secure

### Alternative 4: Built-in Cluster API Integration

**Approach**: Integrate directly with Cluster API to discover and access clusters.

**Trade-offs**:
- **Rejected**: Creates a hard dependency on Cluster API
- Many organizations use other cluster management tools (Rancher, GKE Hub, EKS Anywhere)
- Kubeconfig-based approach is vendor-agnostic and works with any cluster
- Users can integrate with Cluster API by creating Secrets from Cluster API resources if desired

## Backward Compatibility

The `cluster` field is purely additive. Existing RGDs continue to function unchanged, creating resources in the local cluster as before. No API version bump is required. Users can adopt cluster references incrementally.

RGDs without a `cluster` field behave identically to current behavior. The feature is opt-in and does not affect existing functionality.
