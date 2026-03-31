# KREP-013: Multicluster Support via multicluster-runtime

## Summary

KRO operates on a single Kubernetes cluster. Users managing multiple clusters
must deploy and configure KRO separately on each, creating operational overhead
and preventing centralized management of ResourceGraphDefinitions across a
fleet.

This proposal introduces multicluster support using `multicluster-runtime`
(MCR) as a drop-in replacement for `controller-runtime`. A single KRO control
plane in a hub cluster manages ResourceGraphDefinitions, distributing CRDs and
reconciling instances across spoke clusters. The approach is transparent to the
RGD API - no schema changes are required.

## Motivation

Organizations run workloads across multiple clusters for geographic
distribution, environment separation, team isolation, or high availability.
KRO's current single-cluster model forces users to either:

- Deploy KRO N times for N clusters, duplicating RGDs and configuration
- Wrap KRO with GitOps tools like ArgoCD, adding complexity and losing unified
  status visibility
- Build custom multi-cluster orchestration outside KRO

Example: a platform team managing 50 edge clusters needs the same set of RGDs
available on all clusters. Today this requires 50 KRO installations, 50 copies
of each RGD, and no centralized way to observe instance health across the fleet.

The hub-spoke model solves this: define RGDs once in the hub, KRO automatically
distributes CRDs to spokes and reconciles instances wherever they are created.

## Proposal

### Overview

Use `sigs.k8s.io/multicluster-runtime` (MCR) to make KRO's existing
reconciliation loops cluster-aware. MCR wraps `controller-runtime` with
pluggable cluster discovery and per-cluster client management, allowing a single
controller to watch and reconcile resources across multiple clusters.

```
Hub Cluster
  - RGDs (definitions only)
  - KRO Controller (multicluster-aware)
  - Cluster discovery (kubeconfig Secrets)
        |
        +----------------------+----------------------+
        |                      |                      |
        v                      v                      v
  Spoke Cluster 1       Spoke Cluster 2       Spoke Cluster 3
  - CRDs (distributed)  - CRDs (distributed)  - CRDs (distributed)
  - Instances            - Instances            - Instances
  - Child Resources      - Child Resources      - Child Resources
```

Key design points:

- **Runtime-level integration**: MCR operates at the reconcile loop level, not
  the API level. No changes to RGD schema or CRD definitions.
- **Pluggable cluster discovery**: Clusters are discovered via providers
  (starting with kubeconfig Secrets, extensible to Cluster API and others).
- **Per-cluster controllers**: Each spoke gets its own `DynamicController`
  instance with cluster-specific clients and informers.
- **Opt-in**: Enabled via `--enable-multicluster` flag. Without the flag, KRO
  behaves exactly as it does today (single-cluster mode).

### Core Concepts

**Hub and Spoke Separation**

RGDs are authored and stored only in the hub cluster. The RGD controller runs
with `EngageWithLocalCluster(true)` and `EngageWithProviderClusters(false)` -
it only watches the hub. When an RGD is reconciled, KRO distributes the
generated CRD to all engaged spoke clusters and registers a
`MulticlusterDynamicController` for the instance GVR.

Instance controllers run with `EngageWithProviderClusters(true)` - they watch
all spoke clusters. When a user creates an instance in any spoke, the hub's
instance controller reconciles it, creating child resources in the same spoke
cluster as the instance.

**Same-Cluster Locality**

All child resources of an instance are created in the same spoke cluster as the
instance itself. There are no cross-cluster resource references within a single
instance. This preserves KRO's DAG model and avoids the complexity of
distributed transactions.

**Cluster Discovery**

MCR uses a provider interface to discover clusters at runtime:

```go
type Provider interface {
    Get(ctx context.Context, clusterName string) (Cluster, error)
    Watch(ctx context.Context, handler Handler) (Watcher, error)
}
```

The initial implementation uses `ProviderTypeKubeconfig`: Secrets in a
configured namespace with a specific label are treated as cluster registrations.
Each Secret contains a kubeconfig in a configured key. When a Secret is
created/updated/deleted, MCR engages/disengages the corresponding cluster.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: spoke-cluster-1
  namespace: kro-system
  labels:
    multicluster.runtime/cluster: "true"
type: Opaque
stringData:
  kubeconfig: |
    apiVersion: v1
    kind: Config
    clusters:
    - cluster:
        server: https://spoke-1.example.com:6443
        certificate-authority-data: LS0tLS1...
      name: spoke-1
    contexts:
    - context:
        cluster: spoke-1
        user: kro-controller
      name: spoke-1
    current-context: spoke-1
    users:
    - name: kro-controller
      user:
        token: eyJhbGci...
```

Adding or removing a cluster is as simple as creating or deleting a Secret.
This is just one provider implementation - MCR's design allows for other discovery mechanisms 
(e.g. Cluster API, static config, cloud provider APIs) to be added in the future without changing KRO's core logic.

**Transparent to APIs**

Because MCR operates at the reconcile loop level, the RGD API remains
unchanged. Users author RGDs the same way regardless of whether KRO runs in
single-cluster or multicluster mode. The multicluster topology is an
operational concern, not an API concern.

## Design Details

### Manager Integration

The standard `ctrl.NewManager()` is replaced with `multicluster.NewManager()`:

```go
type Manager struct {
    mcmanager.Manager
}

func NewManager(cfg *rest.Config, provider mcprovider.Provider, opts ctrl.Options) (*Manager, error) {
    if provider == nil {
        // Single-cluster mode: use standard manager behavior
        provider = &noopProvider{} // Implements Provider but does nothing
    }
    mgr, err := mcmanager.New(cfg, provider, opts)
    // ...
}
```

When provider is nil (single-cluster mode), a no-op provider is used. The
manager behaves identically to standard `controller-runtime`. This ensures zero
behavioral change for users who do not opt into multicluster.

### MulticlusterDynamicController

The `MulticlusterDynamicController` wraps per-cluster `DynamicController`
instances and implements the `multicluster.Aware` interface:

```go
type MulticlusterDynamicController struct {
    controllers   map[string]*DynamicController  // clusterName -> controller
    handler       MulticlusterHandler
    localManager  ctrl.Manager
}

type MulticlusterHandler func(ctx context.Context, clusterName string, req ctrl.Request) error

// Called by MCR when a new cluster is discovered
func (mc *MulticlusterDynamicController) Engage(ctx context.Context, cluster multicluster.Cluster) error {
    // Create a new DynamicController for this cluster
    // Register all known GVRs
    // Start watching
}
```

When MCR discovers a new cluster (via the provider), it calls `Engage`. The
`MulticlusterDynamicController` creates a new `DynamicController` for that
cluster with cluster-specific clients and informers, and registers all GVRs
that have been registered so far. When a GVR is registered (from an RGD
reconcile), it is registered on all engaged clusters.

For remote clusters, `WaitForSync` is set to `false` because the CRD may not
have propagated yet. The informer will start watching once the CRD becomes
available.

### ClusterClientFactory

A `ClusterClientFactory` provides per-cluster dynamic clients and REST mappers:

```go
type ClusterClientFactory struct {
    clients      map[string]clientEntry  // clusterName -> {dynamic, mapper}
    localDynamic dynamic.Interface
    localMapper  meta.RESTMapper
}

// Called by MCR when a new cluster is discovered
func (f *ClusterClientFactory) Engage(ctx context.Context, cluster multicluster.Cluster) error {
    // Create dynamic.Interface and RESTMapper for this cluster
    // Cache them keyed by cluster name
}

func (f *ClusterClientFactory) GetClients(clusterName string) (dynamic.Interface, meta.RESTMapper, error) {
    if clusterName == "" {
        return f.localDynamic, f.localMapper, nil  // Hub cluster
    }
    // Return cached remote cluster clients
}
```

The local cluster (empty string name) is always available. Remote cluster
clients are created when `Engage` is called and cleaned up when the cluster's
context is cancelled.

### Instance Controller Changes

The instance controller becomes cluster-aware:

```go
func (c *Controller) Reconcile(ctx context.Context, clusterName string, req ctrl.Request) error {
    // Get cluster-specific clients
    dynClient, mapper, err := c.clientFactory.GetClients(clusterName)

    // Fetch instance from the correct cluster
    instance, err := dynClient.Resource(c.gvr).Namespace(req.Namespace).Get(ctx, req.Name, ...)

    // All child resources are created in the same cluster
    rcx := &ReconcileContext{
        ClusterName: clusterName,
        Client:      dynClient,
        // ...
    }
}
```

The reconcile signature gains a `clusterName` parameter. All operations within
a single reconcile use the same cluster's clients, maintaining same-cluster
locality.

### RGD Controller Changes

The RGD controller remains hub-only but gains multicluster awareness for CRD
distribution and instance controller setup:

- Uses `mcbuilder.ControllerManagedBy()` with `EngageWithLocalCluster(true)`
  and `EngageWithProviderClusters(false)` - only watches RGDs on the hub.
- When registering a GVR with the `MulticlusterDynamicController`, the GVR is
  automatically registered on all engaged clusters (current and future).
- CRD distribution to spoke clusters is handled through the
  `ClusterClientFactory`.

### CRD Distribution

When an RGD is reconciled and a CRD is generated, KRO distributes the CRD to
all engaged spoke clusters:

1. RGD controller generates CRD (existing behavior)
2. CRD is applied to the hub cluster (existing behavior)
3. CRD is applied to all engaged spoke clusters via `ClusterClientFactory`
4. When a new cluster is engaged, all existing CRDs are distributed to it

CRD distribution uses server-side apply to handle conflicts gracefully. If a
spoke cluster is temporarily unreachable, distribution is retried on the next
reconcile.

### CLI Flags

```
--enable-multicluster           Enable multicluster mode (default: false)
--cluster-secrets-namespace     Namespace to watch for cluster Secrets (default: kro-system)
--cluster-secrets-label         Label selector for cluster Secrets (default: multicluster.runtime/cluster=true)
--cluster-secrets-key           Key in Secret containing kubeconfig (default: kubeconfig)
```

## Comparison with KREP-012 (Cluster Targets)

KREP-012 proposes a `Target` field on the RGD spec for cross-cluster resource
orchestration. The two proposals address overlapping but distinct use cases
with fundamentally different approaches.

### Architectural Difference

| Aspect | KREP-012 (Target Field) | KREP-013 (MCR) |
|--------|------------------------|-----------------|
| Integration layer | API-level (RGD schema) | Runtime-level (reconcile loop) |
| RGD changes | New `Target` field | None |
| Cluster selection | Per-RGD, in spec | Operational (Secrets) |
| Instance location | Hub cluster only | Spoke clusters |
| Child resource location | Target cluster | Same cluster as instance |
| Cross-cluster in one RGD | Yes (via Target) | No (same-cluster locality) |
| Cluster discovery | Manual (Secret ref in RGD) | Automatic (provider) |

### When to Use Which

**KREP-012** is suited for cases where the RGD author needs explicit control
over which cluster receives resources. The cluster target is part of the
application definition - for example, "this database goes to the production
cluster" or "deploy to the cluster specified by the user in spec.targetCluster".

**KREP-013** is suited for fleet management where the same RGDs should be
available across many clusters. The cluster topology is an operational concern
decoupled from the RGD definition - for example, "make all our platform RGDs
available on every cluster in the fleet".

### Complementary, Not Competing

The two proposals can coexist. KREP-013 provides the multicluster runtime
foundation (cluster discovery, per-cluster clients, cluster-aware reconciliation).
KREP-012's `Target` field could be implemented on top of this foundation,
using the `ClusterClientFactory` to access target clusters instead of managing
its own kubeconfig resolution.

A combined architecture:

```
Hub Cluster (KREP-013: MCR manages fleet)
  - RGDs with optional Target field (KREP-012)
  - KRO Controller
  - Cluster Secrets
        |
        +---> Spoke Cluster 1: instances + child resources (KREP-013)
        |       |
        |       +---> Target Cluster X: specific resources (KREP-012)
        |
        +---> Spoke Cluster 2: instances + child resources (KREP-013)
```

In this model, KREP-013 handles the "where do instances live" question, and
KREP-012 handles the "where do specific resources within an instance go"
question.

## Code Restructuring for Pluggable Runtime

Adopting MCR should not create a hard dependency that prevents kro from running
with standard `controller-runtime`. The goal is a codebase where the runtime
backend (single-cluster vs multicluster) is a startup-time choice, not a
compile-time fork. This requires restructuring several coupling points in the
current code.

### Current State

The codebase has **19 files** importing `controller-runtime` directly. Some
areas already have good abstractions (`kroclient.SetInterface` for clients),
but others embed controller-runtime types directly (the RGD controller embeds
`client.Client`). The key coupling points fall into three categories:

**High coupling** - Manager creation/lifecycle, RGD controller's embedded
client and builder pattern, health probe system.

**Medium coupling** - Logging (`logr`), metrics registry
(`controller-runtime/pkg/metrics`).

**Low coupling** - Instance controller (already uses custom abstractions),
`DynamicController` (only needs `Runnable` interface and `ctrl.Request` type).

### Required Changes

#### 1. Manager Interface Extraction

Currently `cmd/controller/main.go` calls `ctrl.NewManager()` directly and
passes the concrete `ctrl.Manager` into `SetupWithManager()`. This needs a
thin wrapper interface:

```go
// pkg/manager/manager.go
type Manager interface {
    GetClient() client.Client
    GetRESTMapper() meta.RESTMapper
    GetLogger() logr.Logger
    Add(runnable manager.Runnable) error
    AddHealthzCheck(name string, check healthz.Checker) error
    AddReadyzCheck(name string, check healthz.Checker) error
    Start(ctx context.Context) error
}
```

Both `ctrl.Manager` and `mcmanager.Manager` already satisfy this interface -
the extraction is purely about declaring the interface in kro's own package so
that downstream code depends on the interface, not the concrete type. The
factory function in `main.go` selects the implementation based on the
`--enable-multicluster` flag.

#### 2. RGD Controller Decoupling

The `ResourceGraphDefinitionReconciler` currently:
- Embeds `client.Client` from controller-runtime
- Uses `ctrl.NewControllerManagedBy()` builder pattern in `SetupWithManager()`
- References `mcbuilder` or `ctrl` builder depending on runtime

The restructuring:
- Replace embedded `client.Client` with an explicit field set during setup
- Extract the builder/watch configuration into a setup function that accepts
  the manager interface, allowing the builder pattern to differ between
  single-cluster (`ctrl.NewControllerManagedBy`) and multicluster
  (`mcbuilder.ControllerManagedBy`) without polluting the reconciler struct
- Move `SetupWithManager` to accept the kro manager interface instead of
  `ctrl.Manager`

#### 3. DynamicController Abstraction

The `DynamicController` currently implements controller-runtime's `Runnable`
interface and uses `ctrl.Request`. For multicluster, the
`MulticlusterDynamicController` wraps per-cluster instances. Both need to
satisfy a common interface:

```go
// pkg/dynamiccontroller/interface.go
type Interface interface {
    Register(ctx context.Context, parentGVR schema.GroupVersionResource,
        handler Handler, childGVRs ...schema.GroupVersionResource) error
    RegisterWithGVK(ctx context.Context, parentGVR schema.GroupVersionResource,
        parentGVK schema.GroupVersionKind, handler Handler,
        childGVRs ...schema.GroupVersionResource) error
    Deregister(ctx context.Context, gvr schema.GroupVersionResource) error
}
```

The RGD controller depends on this interface, not the concrete type. At startup,
`main.go` creates either a `DynamicController` or
`MulticlusterDynamicController` and passes it through the interface.

#### 4. Client Factory Pattern

The instance controller currently takes `kroclient.SetInterface` which is a
single-cluster client set. For multicluster, `ClusterClientFactory` wraps
per-cluster client sets. The restructuring:

```go
// pkg/client/factory.go
type Factory interface {
    // GetClients returns clients for the named cluster.
    // Empty string returns local (hub) cluster clients.
    GetClients(clusterName string) (SetInterface, error)
}

// SingleClusterFactory wraps a single SetInterface for backward compatibility.
type SingleClusterFactory struct {
    client SetInterface
}

func (f *SingleClusterFactory) GetClients(_ string) (SetInterface, error) {
    return f.client, nil
}
```

In single-cluster mode, the factory always returns the same client set.
In multicluster mode, it returns cluster-specific clients. The instance
controller uses the factory interface, making it cluster-aware without
importing MCR.

#### 5. Reconcile Signature Alignment

The instance controller's `Reconcile(ctx, req) error` and the RGD controller's
`Reconcile(ctx, obj) (Result, error)` differ. For multicluster, both need a
cluster name in the reconcile path. Rather than changing signatures, the cluster
name is carried in the context:

```go
// pkg/multicluster/context.go
type clusterNameKey struct{}

func WithClusterName(ctx context.Context, name string) context.Context {
    return context.WithValue(ctx, clusterNameKey{}, name)
}

func ClusterNameFrom(ctx context.Context) string {
    v, _ := ctx.Value(clusterNameKey{}).(string)
    return v  // empty string = local cluster
}
```

This avoids breaking existing signatures. In single-cluster mode, the context
carries an empty string. In multicluster mode, MCR populates it automatically.

### Migration Path

The restructuring can be done incrementally without changing behavior:

1. **Extract interfaces** (`Manager`, `DynamicController.Interface`,
   `client.Factory`) - pure refactor, no behavioral change
2. **Remove embedded `client.Client`** from RGD controller - use explicit field
3. **Introduce `client.Factory`** with `SingleClusterFactory` - existing tests
   pass unchanged
4. **Add context-based cluster name** - no-op in single-cluster mode
5. **Add MCR integration** behind `--enable-multicluster` flag

Each step is independently mergeable and testable. Steps 1-4 benefit kro's
code quality regardless of whether multicluster ships, by reducing concrete
type dependencies and improving testability.

## Implementing KREP-012 Target Functionality Within KREP-013

KREP-012's `Target` field enables cross-cluster resource placement within a
single instance - "put this Deployment in cluster A and that Service in
cluster B". This is orthogonal to KREP-013's fleet management but can be
built on top of the same runtime foundation rather than as a separate
implementation.

### What KREP-012 Requires

KREP-012 needs three capabilities:
1. Resolve a cluster reference (kubeconfig Secret) to a REST client
2. Apply/watch/delete resources using that client instead of the local client
3. Track per-resource cluster targeting in the reconcile loop

### How KREP-013 Already Provides the Foundation

The `ClusterClientFactory` from KREP-013 already solves (1) and (2). It
maintains per-cluster dynamic clients and REST mappers, created when a cluster
is engaged. The difference is that KREP-013 engages clusters via the MCR
provider (automatic discovery), while KREP-012 engages them via explicit
Secret references in the RGD spec.

### Extending the ClusterClientFactory

To support KREP-012's use case, the `ClusterClientFactory` gains an on-demand
cluster engagement path:

```go
// GetOrCreate returns clients for a cluster, creating them on-demand from
// a kubeconfig Secret if the cluster is not already engaged via the provider.
func (f *ClusterClientFactory) GetOrCreate(ctx context.Context,
    secretName, secretNamespace string) (SetInterface, error) {
    // Check if already engaged (via MCR provider or previous on-demand creation)
    // If not, read the Secret, parse kubeconfig, create clients, cache them
}
```

Clusters discovered by MCR's provider and clusters referenced by Target fields
share the same client cache. If a spoke cluster is both MCR-managed and
Target-referenced, a single set of clients serves both purposes.

### Per-Resource Target Resolution in the Instance Controller

The instance controller's reconcile loop currently creates all child resources
using a single client (the instance's cluster). With Target support, each
resource node in the DAG can specify a different cluster:

```go
// During resource reconciliation in the instance controller
func (c *Controller) reconcileResource(rcx *ReconcileContext, node *graph.Node) error {
    client := rcx.Client  // Default: same cluster as instance

    if node.Target != nil {
        // Resolve the Target's cluster reference
        targetClient, err := c.clientFactory.GetOrCreate(rcx.Ctx,
            node.Target.Cluster.KubeconfigSecretName,
            node.Target.Cluster.KubeconfigSecretNamespace)
        if err != nil {
            return err
        }
        client = targetClient
    }

    // Apply resource using the resolved client
    return c.applyResource(rcx, node, client)
}
```

### API Addition

This requires adding the `Target` field from KREP-012 to the `Resource`
struct (per-resource targeting), not just the `ResourceGraphDefinitionSpec`
(RGD-level targeting):

```go
type Resource struct {
    ID             string   `json:"id"`
    Template       runtime.RawExtension `json:"template"`
    // ...existing fields...

    // Target specifies where this resource should be created.
    // If not specified, the resource is created in the same cluster as the instance.
    // +kubebuilder:validation:Optional
    Target *Target `json:"target,omitempty"`
}
```

RGD-level targeting (all resources go to one cluster) can be syntactic sugar
that sets the Target on every resource that doesn't already have one.

### What This Means in Practice

With both KREPs implemented, users get the full spectrum:

```yaml
# Fleet management (KREP-013 only): same RGD available everywhere
# No Target field needed - instances are created in spokes, child resources
# follow the instance.

# Cross-cluster orchestration (KREP-012 on KREP-013 foundation):
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: multi-cluster-app
spec:
  schema:
    apiVersion: v1alpha1
    kind: MultiClusterApp
    spec:
      appName: string
  resources:
    - id: database
      target:
        cluster:
          kubeconfigSecretName: db-cluster-kubeconfig
          kubeconfigSecretNamespace: kro-system
      template:
        apiVersion: databases.example.com/v1
        kind: PostgreSQL
        metadata:
          name: ${schema.spec.appName + '-db'}

    - id: app
      # No target - created in same cluster as instance
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.spec.appName}
        spec:
          template:
            spec:
              containers:
              - name: app
                env:
                - name: DB_HOST
                  value: ${database.status.host}
```

### Implementation Sequence

1. **KREP-013 first**: Ship MCR integration with `ClusterClientFactory` and
   multicluster reconciliation. This is the foundation.
2. **KREP-012 on top**: Add `Target` field to `Resource` struct, extend
   `ClusterClientFactory` with on-demand engagement, update instance
   controller to resolve per-resource targets. The runtime plumbing is
   already in place.

This ordering avoids duplicate work. KREP-012 implemented standalone would
need its own cluster client management, Secret watching, and connection
lifecycle - all of which KREP-013 already provides.

## Addressing Concerns

The following concerns were raised in
[issue #1060](https://github.com/kubernetes-sigs/kro/issues/1060):

### Stability Guarantees

MCR is a thin wrapper around `controller-runtime`. It does not replace or
reimplement controller-runtime internals - it extends them with cluster-aware
routing. The stability guarantees are inherited from controller-runtime:

- Cache behavior, informer lifecycle, and reconcile semantics are unchanged
- Per-cluster controllers use standard `DynamicController` instances
- The only new failure mode is cluster connectivity, which is handled by
  MCR's provider interface (unhealthy clusters are disengaged)

MCR is maintained under `sigs.k8s.io` and follows Kubernetes SIG conventions.

### Testing Setup and Infrastructure

**Unit tests**: Existing unit tests pass without modification in single-cluster
mode (nil provider). Multicluster-specific logic is tested by mocking the
`multicluster.Cluster` interface.

**Integration tests**: The existing integration test environment is updated to
use `multicluster.NewManager` with nil provider, ensuring no behavioral change
for single-cluster tests.

**E2E tests**: A kind-based test harness creates multiple clusters:
- 1 hub cluster running KRO with `--enable-multicluster`
- 2+ spoke clusters registered via kubeconfig Secrets
- Tests verify: CRD distribution, instance creation in spokes, child resource
  locality, cluster add/remove, RGD updates propagating to all spokes

The E2E setup uses `kind` which is already used by KRO's existing test
infrastructure.

### Version Compatibility Across Differing API Server Versions

This is explicitly out of scope for the initial implementation. Users are
responsible for ensuring spoke clusters are compatible with the RGDs they will
run. Mitigation strategies for future work:

- Minimum API server version check at cluster engagement time
- CEL expression compatibility is bounded by KRO's embedded CEL library, not
  the spoke API server version
- RGD validation happens on the hub; spoke clusters only need to support the
  resource GVKs referenced in templates

### Performance Considerations Around Dynamic Controller Scaling

Each spoke cluster adds:
- One `DynamicController` instance per registered GVR
- One informer per watched GVR per cluster
- One dynamic client and REST mapper

For N clusters and M RGDs, this is O(N * M) informers. Initial benchmarks
should establish the practical ceiling. Mitigation strategies:

- MCR's provider can rate-limit cluster engagement
- Informers use shared caches within a cluster
- `WaitForSync: false` prevents blocking on unavailable CRDs
- Future work: selective RGD-to-cluster targeting to reduce the fan-out

## Scope

### In Scope

- MCR integration as drop-in replacement for `controller-runtime`
- Kubeconfig Secret provider for cluster discovery
- `MulticlusterDynamicController` with per-cluster controller instances
- `ClusterClientFactory` for per-cluster clients
- Cluster-aware instance reconciliation with same-cluster locality
- CRD distribution from hub to spoke clusters
- `--enable-multicluster` opt-in flag and related CLI configuration
- E2E test infrastructure using kind

### Out of Scope

The following are out of scope for this KREP but may be addressed in future:

- Cross-cluster resource dependencies within a single instance
- Per-cluster RGD targeting (selectively distributing RGDs to specific clusters)
- Cluster API provider for cluster discovery
- API server version compatibility checking
- Performance benchmarks and optimization for large fleet sizes (50+ clusters)

The following are out of scope and not planned:

- Cross-cluster networking or service mesh integration
- Cluster lifecycle management (creating/deleting clusters)
- Running multiple KRO controllers that coordinate with each other
- Federation or replication of RGDs across clusters (RGDs are hub-only)

### Non-Goals

- Change the RGD API or schema
- Require users to modify existing RGDs for multicluster support
- Replace specialized multi-cluster tools (Admiralty, Liqo, etc.)
- Provide cluster fleet management beyond resource orchestration

## Backward Compatibility

This change is fully backward compatible:

- Multicluster is opt-in via `--enable-multicluster` flag
- Without the flag, KRO uses a no-op provider and behaves identically to today
- No RGD API changes - existing RGDs work without modification
- The `multicluster.NewManager` with nil provider passes all existing tests
- Single-cluster mode remains the default and recommended starting point

## Testing Strategy

### Requirements

- `kind` for creating multi-cluster test environments
- Existing integration test framework (updated to use `multicluster.NewManager`)

### Test Plan

**Unit tests**:
- `MulticlusterDynamicController`: Engage/disengage clusters, GVR registration
  propagation, handler routing by cluster name
- `ClusterClientFactory`: Client creation, caching, cleanup on disengage
- Provider: Secret parsing, label filtering, watch events

**Integration tests**:
- Single-cluster mode: all existing tests pass with `multicluster.NewManager`
  and nil provider (regression gate)
- Multicluster mode: RGD reconcile distributes CRDs, instance reconcile uses
  correct cluster clients

**E2E tests**:
- Hub + 2 spokes: create RGD on hub, verify CRDs on spokes, create instance on
  spoke, verify child resources on same spoke
- Cluster add: add a spoke Secret, verify CRDs are distributed and instances
  can be created
- Cluster remove: delete a spoke Secret, verify KRO handles disengagement
  gracefully
- RGD update: modify RGD on hub, verify CRD updates propagate to all spokes

## POC

A proof-of-concept implementation is available at
[mjudeikis/kro@mcr.poc](https://github.com/mjudeikis/kro/tree/mcr.poc).

The POC demonstrates the full integration: MCR manager setup, multicluster
dynamic controller, cluster client factory, cluster-aware instance
reconciliation, and e2e test infrastructure with kind clusters.
