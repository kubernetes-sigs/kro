# KREP-7: Cluster References for Multi-Cluster Resource Management

## Summary

This KREP introduces cluster references to KRO, enabling ResourceGraphDefinitions to read from and deploy resources to remote Kubernetes clusters. By adding a `cluster` field to external references, resource definitions, and the RGD spec, KRO can orchestrate resources across multiple clusters while maintaining its declarative model and dependency management capabilities.

## Motivation

Modern Kubernetes deployments increasingly span multiple clusters for high availability, geographic distribution, regulatory compliance, and workload isolation. Organizations need to:

- Deploy applications across multiple environments (dev, staging, production) in different clusters
- Reference shared configuration or infrastructure resources from central clusters
- Manage edge deployments from a central control plane
- Orchestrate multi-cluster architectures (hub-and-spoke, multi-region)

Today, users must either:
1. Run separate KRO instances in each cluster with manual coordination
2. Use external tools to replicate resources across clusters
3. Build custom operators for cross-cluster orchestration

None of these approaches leverage KRO's dependency management and CEL-based orchestration across cluster boundaries. Users lose the declarative, unified resource graph model when working with multiple clusters.

### Use Cases

**Shared Configuration**: A central platform cluster hosts shared ConfigMaps and Secrets that application clusters reference for consistent configuration.

**Multi-Region Deployment**: Deploy the same application stack to multiple regional clusters with region-specific parameters, all managed from a single RGD instance.

**Hub-and-Spoke Architecture**: A management cluster deploys and monitors workloads across dozens or hundreds of edge clusters.

**Cross-Cluster Dependencies**: An application in cluster A depends on a database provisioned in cluster B, with KRO managing the dependency relationship.

## Proposal

### Overview

This KREP introduces the `cluster` field at three levels:

1. **ExternalRef cluster** - Read resources from remote clusters
2. **Resource-level cluster** - Deploy individual resources to remote clusters
3. **RGD-level cluster** - Deploy all resources in an RGD to a remote cluster

The cluster reference specifies authentication (kubeconfig Secret) and synchronization behavior (watch by default, or poll with configurable interval and jitter). KRO extends its existing reconciliation model to manage resources across cluster boundaries while preserving dependency ordering and CEL expression evaluation.

### Core Concepts

**Cluster Reference Scope**

Cluster references operate at three levels with clear precedence:
- Resource-level cluster overrides RGD-level cluster
- RGD-level cluster applies to all resources unless overridden
- No cluster reference means local cluster (current behavior)

**Static vs Dynamic Cluster References**

Static cluster references (literal values) enable validation at RGD creation time. KRO can verify cluster connectivity and run static analysis on remote resources.

Dynamic cluster references (CEL expressions) defer cluster selection to instance creation time. KRO skips static analysis for these resources since the target cluster is unknown until runtime.

**Synchronization Strategies**

By default, cluster references use watch mode, establishing a watch connection to the remote cluster for real-time updates. For scenarios involving hundreds or thousands of clusters, users can specify a `pollConfig` to switch to polling mode with configurable interval and jitter.

**Dependency Constraints**

Resources deployed to remote clusters (RGD-level cluster) cannot depend on resources in the RGD. This prevents circular dependencies where remote resources would need to reference the local cluster. Resource-level cluster references have no such restriction since they remain part of the local dependency graph.

## API Changes

### ClusterReference Type

```yaml
type ClusterReference struct {
    // Name is the identifier for this cluster reference.
    // Used in error messages and status reporting.
    //
    // +kubebuilder:validation:Required
    Name string `json:"name"`
    
    // KubeconfigSecret references a Secret containing the kubeconfig
    // for accessing the remote cluster.
    //
    // +kubebuilder:validation:Required
    KubeconfigSecret SecretReference `json:"kubeconfigSecret"`
    
    // PollConfig defines polling behavior for the remote cluster.
    // If not specified, the cluster reference uses watch mode (default).
    // When specified, switches to polling mode with the given configuration.
    //
    // +kubebuilder:validation:Optional
    PollConfig *PollConfig `json:"pollConfig,omitempty"`
}

type SecretReference struct {
    // Name of the Secret containing the kubeconfig.
    //
    // +kubebuilder:validation:Required
    Name string `json:"name"`
    
    // Namespace of the Secret.
    //
    // +kubebuilder:validation:Required
    Namespace string `json:"namespace"`
    
    // Key within the Secret that contains the kubeconfig data.
    // Defaults to "kubeconfig" if not specified.
    //
    // +kubebuilder:validation:Optional
    // +kubebuilder:default="kubeconfig"
    Key string `json:"key,omitempty"`
}

type PollConfig struct {
    // Interval is the base polling interval.
    // Example: "30s", "5m", "1h"
    //
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:Pattern=`^[0-9]+(s|m|h)$`
    Interval string `json:"interval"`
    
    // Jitter is the maximum random duration added to the interval
    // to prevent thundering herd. Example: "5s", "1m"
    //
    // +kubebuilder:validation:Optional
    // +kubebuilder:validation:Pattern=`^[0-9]+(s|m|h)$`
    Jitter string `json:"jitter,omitempty"`
}
```

### ExternalRef Changes

```yaml
type ExternalRef struct {
    // APIVersion is the API version of the external resource.
    //
    // +kubebuilder:validation:Required
    APIVersion string `json:"apiVersion"`
    
    // Kind is the kind of the external resource.
    //
    // +kubebuilder:validation:Required
    Kind string `json:"kind"`
    
    // Metadata contains the name and optional namespace of the external resource.
    //
    // +kubebuilder:validation:Required
    Metadata ExternalRefMetadata `json:"metadata"`
    
    // Cluster specifies a remote cluster to read this resource from.
    // If not specified, uses the local cluster.
    // Can be a static cluster reference or a CEL expression.
    // Uses watch mode by default; specify pollConfig to switch to polling.
    //
    // +kubebuilder:validation:Optional
    Cluster *ClusterReference `json:"cluster,omitempty"`
}
```

### Resource Changes

```yaml
type Resource struct {
    // ID is a unique identifier for this resource within the ResourceGraphDefinition.
    //
    // +kubebuilder:validation:Required
    ID string `json:"id,omitempty"`
    
    // Template is the Kubernetes resource manifest to create.
    //
    // +kubebuilder:validation:Optional
    Template runtime.RawExtension `json:"template,omitempty"`
    
    // ExternalRef references an existing resource in the cluster instead of creating one.
    //
    // +kubebuilder:validation:Optional
    ExternalRef *ExternalRef `json:"externalRef,omitempty"`
    
    // ReadyWhen is a list of CEL expressions that determine when this resource is considered ready.
    //
    // +kubebuilder:validation:Optional
    ReadyWhen []string `json:"readyWhen,omitempty"`
    
    // IncludeWhen is a list of CEL expressions that determine whether this resource should be created.
    //
    // +kubebuilder:validation:Optional
    IncludeWhen []string `json:"includeWhen,omitempty"`
    
    // Cluster specifies a remote cluster to deploy this resource to.
    // Overrides RGD-level cluster if both are specified.
    // Can be a static cluster reference or a CEL expression.
    // Uses watch mode by default; specify pollConfig to switch to polling.
    //
    // +kubebuilder:validation:Optional
    Cluster *ClusterReference `json:"cluster,omitempty"`
}
```

### ResourceGraphDefinitionSpec Changes

```yaml
type ResourceGraphDefinitionSpec struct {
    // Schema defines the structure of instances created from this ResourceGraphDefinition.
    //
    // +kubebuilder:validation:Required
    Schema *Schema `json:"schema,omitempty"`
    
    // Resources is the list of Kubernetes resources that will be created and managed
    // for each instance.
    //
    // +kubebuilder:validation:Optional
    Resources []*Resource `json:"resources,omitempty"`
    
    // Cluster specifies a remote cluster to deploy all resources to.
    // Individual resources can override this with their own cluster reference.
    // Resources deployed via RGD-level cluster cannot depend on other
    // resources in the RGD.
    // Uses watch mode by default; specify pollConfig to switch to polling.
    //
    // +kubebuilder:validation:Optional
    // +kubebuilder:validation:XValidation:rule="!has(self.cluster) || self.resources.all(r, !has(r.template) || size(extractDependencies(r.template)) == 0)",message="resources with RGD-level cluster cannot have dependencies"
    Cluster *ClusterReference `json:"cluster,omitempty"`
}
```

## Examples

### Example 1: Reading Shared Configuration from Central Cluster

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: central-cluster-kubeconfig
  namespace: default
type: Opaque
stringData:
  kubeconfig: |
    apiVersion: v1
    kind: Config
    clusters:
    - cluster:
        certificate-authority-data: LS0tLS1CRUdJTi...
        server: https://central-cluster.example.com:6443
      name: central-cluster
    contexts:
    - context:
        cluster: central-cluster
        user: kro-reader
      name: central-cluster
    current-context: central-cluster
    users:
    - name: kro-reader
      user:
        token: eyJhbGciOiJSUzI1NiIsImtpZCI6...
---
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: app-with-shared-config
spec:
  schema:
    apiVersion: v1alpha1
    kind: Application
    spec:
      name: "string"
      replicas: "integer | default=1"
  resources:
    - id: sharedConfig
      externalRef:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: platform-defaults
          namespace: shared-config
        cluster:
          name: central-cluster
          kubeconfigSecret:
            name: central-cluster-kubeconfig
            namespace: default
          # Uses watch mode by default (no pollConfig specified)
    
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.spec.name}
        spec:
          replicas: ${schema.spec.replicas}
          template:
            spec:
              containers:
              - name: app
                image: ${sharedConfig.data.defaultImage}
                env:
                - name: LOG_LEVEL
                  value: ${sharedConfig.data.logLevel}
```

### Example 2: Multi-Region Deployment with Resource-Level Cluster

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: multi-region-app
spec:
  schema:
    apiVersion: v1alpha1
    kind: MultiRegionApp
    spec:
      regions: "[]string"
      image: "string"
  resources:
    - id: regionalDeployments
      forEach:
        - region: ${schema.spec.regions}
      cluster:
        name: ${region + '-cluster'}
        kubeconfigSecret:
          name: ${region + '-kubeconfig'}
          namespace: default
        pollConfig:
          interval: "1m"
          jitter: "10s"
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.metadata.name}
          namespace: default
        spec:
          replicas: 3
          template:
            spec:
              containers:
              - name: app
                image: ${schema.spec.image}
```

### Example 3: Hub-and-Spoke with RGD-Level Cluster

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: edge-workload
spec:
  schema:
    apiVersion: v1alpha1
    kind: EdgeWorkload
    spec:
      targetCluster: "string"
      workloadConfig: "object"
  cluster:
    name: ${schema.spec.targetCluster}
    kubeconfigSecret:
      name: edge-clusters-kubeconfig
      namespace: default
    pollConfig:
      interval: "5m"
      jitter: "30s"
  resources:
    - id: namespace
      template:
        apiVersion: v1
        kind: Namespace
        metadata:
          name: edge-workloads
    
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.metadata.name}
          namespace: edge-workloads
        spec:
          replicas: ${schema.spec.workloadConfig.replicas}
          template:
            spec:
              containers:
              - name: workload
                image: ${schema.spec.workloadConfig.image}
```

### Example 4: Static Cluster Reference with Watch

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: cross-cluster-app
spec:
  schema:
    apiVersion: v1alpha1
    kind: CrossClusterApp
    spec:
      name: "string"
  resources:
    - id: database
      cluster:
        name: data-cluster
        kubeconfigSecret:
          name: data-cluster-kubeconfig
          namespace: default
        # Uses watch mode by default
      template:
        apiVersion: db.example.com/v1
        kind: Database
        metadata:
          name: ${schema.spec.name}-db
        spec:
          size: large
    
    - id: application
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.spec.name}
        spec:
          template:
            spec:
              containers:
              - name: app
                env:
                - name: DB_HOST
                  value: ${database.status.endpoint}
```

## Design Details

### Static Analysis and Validation

**Static Cluster References**

When a cluster reference uses literal values (no CEL expressions), KRO performs validation at RGD creation time:

1. Verify the kubeconfig Secret exists and is accessible
2. Attempt to connect to the remote cluster
3. Validate API server reachability
4. Run static analysis on resources targeting that cluster (schema validation, RBAC checks)
5. Mark RGD as not ready if cluster is unreachable

**Dynamic Cluster References**

When cluster references contain CEL expressions (e.g., `${schema.spec.targetCluster}`), KRO:

1. Skips cluster connectivity validation at RGD creation
2. Skips static analysis for resources targeting dynamic clusters
3. Defers all validation to instance creation time
4. Evaluates CEL expressions when processing each instance

### Dependency Management

**RGD-Level Cluster Constraint**

Resources deployed via RGD-level cluster cannot reference other resources in the RGD. This is enforced via CEL validation:

```yaml
+kubebuilder:validation:XValidation:rule="!has(self.cluster) || self.resources.all(r, !has(r.template) || size(extractDependencies(r.template)) == 0)",message="resources with RGD-level cluster cannot have dependencies"
```

This prevents scenarios where:
- Remote resources try to reference local resources (cross-cluster CEL evaluation)
- Circular dependencies between local and remote clusters
- Ambiguous dependency ordering across cluster boundaries

**Resource-Level Cluster Dependencies**

Resources with resource-level cluster references remain part of the local dependency graph. They can:
- Depend on local resources
- Depend on externalRef resources from remote clusters
- Depend on other remote resources (same or different clusters)
- Be referenced by local resources via CEL expressions

KRO evaluates dependencies normally, fetching remote resource state as needed for CEL expressions.

### Synchronization Behavior

**Watch Mode (Default)**

When no `pollConfig` is specified, KRO establishes a watch connection to the remote cluster's API server:
- Real-time updates when resources change
- Automatic reconnection on connection loss
- Suitable for small to medium numbers of clusters
- Lower latency for detecting changes

**Poll Mode**

When `pollConfig` is specified, KRO periodically fetches resource state:
- Configurable interval with jitter to prevent thundering herd
- Scales to thousands of clusters
- Higher latency for detecting changes
- Reduced load on remote API servers
- Essential for hub-and-spoke architectures managing large numbers of edge clusters

### Error Handling

**Cluster Unreachable at RGD Creation**

For static cluster references, if the cluster is unreachable:
- RGD validation fails
- RGD status shows "Inactive" state
- Condition indicates cluster connectivity issue
- User must fix connectivity before RGD becomes ready

**Cluster Unreachable During Reconciliation**

If a cluster becomes unreachable after RGD is active:
- Instance reconciliation retries with exponential backoff
- Instance status reflects connectivity error
- Other instances continue reconciling normally
- Resources in unreachable clusters remain in last known state

**Cluster Unreachable During Deletion**

If a cluster is unreachable during instance deletion:
- Deletion hangs until cluster access is restored
- Finalizers prevent instance removal
- Status indicates waiting for cluster access
- User must restore connectivity or manually remove finalizers

**Future Enhancement**: More sophisticated error handling and lifecycle control (timeouts, orphan policies, force deletion, graceful degradation) will be addressed in a follow-up KREP.

### Kubeconfig Secret Format

KRO expects kubeconfig Secrets in standard Kubernetes Secret format:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: remote-cluster-kubeconfig
  namespace: default
type: Opaque
stringData:
  kubeconfig: |
    apiVersion: v1
    kind: Config
    clusters:
    - cluster:
        certificate-authority-data: <base64-encoded-ca-cert>
        server: https://remote-cluster.example.com:6443
      name: remote-cluster
    contexts:
    - context:
        cluster: remote-cluster
        user: kro-user
      name: remote-cluster
    current-context: remote-cluster
    users:
    - name: kro-user
      user:
        token: <service-account-token>
```

The Secret must contain a valid kubeconfig with sufficient permissions to perform required operations (read for externalRef, full CRUD for resource deployment).

### Cluster Reference Precedence

When multiple cluster references are specified:

1. Resource-level `cluster` takes highest precedence
2. RGD-level `cluster` applies if no resource-level cluster
3. No cluster reference means local cluster (current behavior)

Example:

```yaml
spec:
  cluster:
    name: default-remote  # Applies to all resources
  resources:
    - id: resource1
      template: {...}  # Deployed to default-remote
    
    - id: resource2
      cluster:
        name: specific-remote  # Overrides default-remote
      template: {...}  # Deployed to specific-remote
    
    - id: resource3
      template: {...}  # Deployed to default-remote
```

## Scope

### In Scope

- `cluster` field for ExternalRef (reading from remote clusters)
- `cluster` field for Resource (deploying individual resources to remote clusters)
- `cluster` field for ResourceGraphDefinitionSpec (deploying all resources to remote cluster)
- Static and dynamic cluster references (literal values and CEL expressions)
- Watch mode (default) and poll mode synchronization
- Kubeconfig Secret-based authentication
- Static analysis for static cluster references
- Dependency constraint enforcement for RGD-level cluster
- Basic error handling (unreachable clusters, invalid kubeconfigs)

### Out of Scope

The following are explicitly out of scope for this KREP and will be addressed in future work:

- **ExternalRef selectors** - Collection watching via label selectors (KREP-3). Cluster references are designed to be compatible with this feature when accepted.
- **Advanced error handling** - Timeout policies, orphan handling, force deletion, graceful degradation and lifecyle controls
- **Instance objects in remote clusters** - Deploying the RGD instance itself to a remote cluster
- **Alternative authentication methods** - Service account projection, OIDC, certificate-based auth
- **Cluster discovery** - Automatic discovery of clusters via labels or external registries
- **Cluster health monitoring** - Proactive health checks and alerting for remote clusters
- **Bandwidth optimization** - Compression, delta updates, or other optimizations for poll mode

## Alternatives Considered

### Delegate to External Multi-Cluster Operator

**Approach**: Build a separate Kubernetes operator that handles cross-cluster resource management, with KRO delegating to it.

**Trade-offs**:

This approach requires either:

1. **Monkey-patching existing types**: Modify standard Kubernetes resources (Deployment, Service, etc.) to add cluster targeting metadata. This is infeasible because:
   - Violates Kubernetes API conventions
   - Breaks compatibility with standard tooling
   - Requires API server modifications or admission webhooks
   - Creates maintenance burden across Kubernetes versions

2. **Wrapper types**: Create custom resources that wrap standard Kubernetes types (e.g., `RemoteDeployment` wrapping `Deployment`). This has narrow utility because:
   - Requires a powerful orchestrator to introspect wrappers and understand their contents
   - Adds indirection and complexity for users
   - Breaks compatibility with existing Kubernetes tooling
   - Since KRO is already that powerful orchestrator, combining functionality is more natural

**Why rejected**: KRO already has the introspection, dependency management, and CEL evaluation capabilities needed for cross-cluster orchestration. Adding cluster references to KRO's existing model is simpler and more powerful than building a separate operator with wrapper types.

### Cluster-Scoped RGD Instances

**Approach**: Allow RGD instances to specify a target cluster, deploying all resources to that cluster.

**Trade-offs**:
- Simpler API (single cluster per instance)
- Cannot mix local and remote resources in one instance
- Cannot reference resources across clusters within a single RGD
- Less flexible for hybrid architectures

**Why rejected**: Resource-level and RGD-level cluster references provide more flexibility while still supporting the single-cluster-per-instance use case.

### Implicit Cluster Discovery

**Approach**: Automatically discover clusters via labels, annotations, or external registries instead of explicit kubeconfig references.

**Trade-offs**:
- Reduces configuration verbosity
- Adds complexity to cluster authentication and authorization
- Makes security model less explicit
- Harder to audit and troubleshoot

**Why rejected**: Explicit kubeconfig references provide clear security boundaries and make cluster access auditable. Discovery can be added later as an optional enhancement.

## Backward Compatibility

The `cluster` field is purely additive. Existing RGDs continue to function unchanged:
- Resources without `cluster` field deploy to local cluster (current behavior)
- ExternalRefs without `cluster` field read from local cluster (current behavior)
- No API version bump required
- Users can adopt cluster references incrementally

## Testing Strategy

### Requirements

- Kubernetes 1.30+
- Multiple test clusters (local + remote)
- Integration test suite with cross-cluster scenarios

### Test Plan

1. **Static Cluster References**
   - Verify RGD validation with reachable clusters
   - Verify RGD fails validation with unreachable clusters
   - Test static analysis runs on remote resources
   - Confirm kubeconfig Secret changes trigger revalidation

2. **Dynamic Cluster References**
   - Verify static analysis is skipped for dynamic clusters
   - Test CEL expression evaluation for cluster selection
   - Confirm validation occurs at instance creation time

3. **ExternalRef with Cluster**
   - Read single resources from remote clusters
   - Verify watch mode (default) receives real-time updates
   - Verify poll mode fetches at correct intervals when pollConfig specified

4. **Resource-Level Cluster**
   - Deploy resources to remote clusters
   - Verify dependencies work across clusters
   - Test resource-level overrides RGD-level cluster
   - Confirm CEL expressions can reference remote resources

5. **RGD-Level Cluster**
   - Deploy all resources to remote cluster
   - Verify dependency constraint enforcement (no internal dependencies)
   - Test that resource-level cluster overrides RGD-level

6. **Synchronization Strategies**
   - Watch mode (default): verify real-time updates and reconnection
   - Poll mode: verify interval and jitter behavior when pollConfig specified
   - Test that omitting pollConfig uses watch mode

7. **Error Handling**
   - Unreachable cluster at RGD creation (static)
   - Cluster becomes unreachable during reconciliation
   - Cluster unreachable during deletion (verify hang)
   - Invalid kubeconfig Secret

8. **Security**
   - Verify RBAC enforcement for Secret access
   - Test with minimal-permission kubeconfigs
   - Confirm audit logging of cross-cluster operations

## Integration with ExternalRef Selectors

This KREP focuses on cluster references for single named resources. If KREP-3 (Decorators/Collection Watching) is accepted, cluster references would naturally extend to collection-based externalRefs.

**ExternalRef with Selectors and Cluster**

When KREP-3 adds `Selector` and `NamespaceSelector` to ExternalRef, cluster references would work seamlessly:

```yaml
resources:
  - id: remoteDeployments
    externalRef:
      apiVersion: apps/v1
      kind: DeploymentList
      selector:
        matchLabels:
          app: backend
      namespaceSelector:
        matchLabels:
          env: production
      cluster:
        name: production-cluster
        kubeconfigSecret:
          name: prod-kubeconfig
          namespace: default
        # Uses watch mode by default
```

**With Polling for Large-Scale Collection Watching**

```yaml
resources:
  - id: edgeDeployments
    externalRef:
      apiVersion: apps/v1
      kind: DeploymentList
      selector:
        matchLabels:
          tier: edge
      cluster:
        name: ${schema.spec.edgeCluster}
        kubeconfigSecret:
          name: edge-kubeconfig
          namespace: default
        pollConfig:
          interval: "2m"
          jitter: "20s"
```

This would enable:
- Watching collections of resources in remote clusters
- Decorating resources across cluster boundaries
- Building multi-cluster aggregation patterns
- Scaling collection watching to thousands of clusters via polling

**Design Considerations**

The cluster reference design is intentionally compatible with collection watching:
- Watch mode supports watching multiple resources efficiently
- Poll mode scales to large collections across many clusters
- CEL expressions can iterate over remote collections using forEach
- Dependency management works identically for single resources and collections

No API changes would be needed to support this integration - cluster references would simply work with whatever ExternalRef capabilities exist.

## Future Work

### Instance Objects in Remote Clusters

Allow the RGD instance itself to be created in a remote cluster, with resources deployed relative to that cluster.

### Alternative Authentication Methods

- Service account token projection
- OIDC-based authentication
- Certificate-based authentication
- Cloud provider IAM integration

### Cluster Discovery and Management

- Automatic cluster discovery via labels or external registries
- Cluster health monitoring and alerting
- Dynamic cluster pool management

### Multi-Cluster Status Aggregation

- Unified status view across all cluster deployments
- Cross-cluster resource health dashboards
- Aggregated metrics and events

### Performance Optimizations

- Compression for poll mode
- Delta updates to reduce bandwidth
- Connection pooling for multiple resources in same cluster
- Batch operations for collections

## Migration Path

Users can adopt cluster references incrementally:

1. **Phase 1**: Add cluster references to externalRef for reading shared configuration
2. **Phase 2**: Deploy individual resources to remote clusters using resource-level cluster
3. **Phase 3**: Deploy entire RGDs to remote clusters using RGD-level cluster

No migration of existing RGDs is required. The feature is opt-in via the `cluster` field.
