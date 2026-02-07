# KREP: Support for Cluster-Scoped Instance CRDs

## Summary

This proposal adds support for generating cluster-scoped CustomResourceDefinitions (CRDs) from ResourceGraphDefinitions (RGDs). Currently, kro hardcodes all generated instance CRDs to be namespaced-scoped. This enhancement allows users to define cluster-scoped instance CRDs when needed, enabling RGDs that manage cluster-level resources or resources that don't fit the namespace model.

## Motivation

### Problem Statement

Currently, kro hardcodes all generated instance CRDs to be namespaced-scoped. This limitation prevents users from creating RGDs that:
- Manage cluster-scoped resources (e.g., ClusterRole, ClusterRoleBinding, ClusterIssuer)
- Define infrastructure components that are inherently cluster-wide
- Create resources that need to exist at the cluster level for operational reasons

Users have expressed the need for this capability, particularly for RGDs that create a mixture of cluster and namespace-scoped resources (see [issue #806](https://github.com/kubernetes-sigs/kro/issues/806)).

### Use Cases

1. **Cluster Infrastructure Management**: Define RGDs that manage cluster-level infrastructure components like cluster-wide policies, cluster issuers, or cluster-scoped storage classes.

2. **Mixed Scope Resources**: Create RGDs that manage both cluster-scoped and namespaced resources together, where the instance itself needs to be cluster-scoped to properly represent the cluster-level component.

3. **Operator Patterns**: Support operator-like patterns where the instance CRD represents a cluster-wide configuration that manages multiple namespaced resources.

## Proposal

### Overview

Add a `scope` field to `ResourceGraphDefinition.spec.schema` that allows users to specify whether the generated instance CRD should be `Namespaced` or `Cluster` scoped. The field is:
- Optional (defaults to `Namespaced` to preserve backward compatibility)
- Immutable (cannot be changed after RGD creation, matching Kubernetes CRD scope immutability rules)
- Enum-constrained (`Namespaced` or `Cluster`)

### Design Details

#### API Changes

**New Field in `Schema` struct:**
```go
// Scope determines whether the generated instance CRD is Namespaced or Cluster scoped.
// Default is Namespaced to preserve current behavior. This field is immutable.
//
// +kubebuilder:validation:Optional
// +kubebuilder:default="Namespaced"
// +kubebuilder:validation:Enum=Namespaced;Cluster
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="scope is immutable"
Scope ResourceScope `json:"scope,omitempty"`
```

**New Type:**
```go
type ResourceScope string

const (
	ScopeNamespaced ResourceScope = "Namespaced"
	ScopeCluster    ResourceScope = "Cluster"
)
```

#### Implementation Changes

1. **CRD Generation** (`pkg/graph/crd/crd.go`):
   - Update `SynthesizeCRD` to accept `scope` parameter
   - Set `crd.Spec.Scope` based on the provided scope value

2. **Builder** (`pkg/graph/builder.go`):
   - Extract scope from `rgd.Spec.Schema.Scope`
   - Pass scope to CRD synthesis
   - Set instance node's `Namespaced` field based on scope

3. **Controller** (`pkg/controller/instance/status.go`):
   - Handle cluster-scoped instances in status updates (skip `.Namespace()` call when namespace is empty)

4. **Dynamic Controller**:
   - Already handles both scopes correctly via `resourceClientFor` function

#### Example Usage

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: cluster-policy
spec:
  schema:
    apiVersion: v1
    kind: ClusterPolicy
    scope: Cluster  # <-- New field
    spec:
      policyName: string
      rules: list<map>
  resources:
    - id: clusterrole
      template:
        apiVersion: rbac.authorization.k8s.io/v1
        kind: ClusterRole
        metadata:
          name: ${schema.spec.policyName}
        rules: ${schema.spec.rules}
```

### Backward Compatibility

- **Default Behavior**: When `scope` is omitted, it defaults to `Namespaced`, preserving existing behavior
- **Existing RGDs**: All existing RGDs continue to work without modification
- **Immutable Field**: Once set, scope cannot be changed, preventing migration issues

### Validation

- **Enum Validation**: Only `Namespaced` or `Cluster` values are accepted
- **Immutability**: XValidation rule ensures scope cannot be changed after RGD creation
- **CRD Scope Rules**: Follows Kubernetes CRD scope immutability rules

## Other Solutions Considered

### Alternative 1: Separate ResourceGraphDefinition Type

**Approach**: Create a separate `ClusterResourceGraphDefinition` type.

**Rejected Because**:
- Duplicates API surface unnecessarily
- Requires maintaining two parallel code paths
- More complex for users to understand

### Alternative 2: Auto-detect from Resources

**Approach**: Automatically determine scope based on the resources in the RGD.

**Rejected Because**:
- Ambiguous when RGD contains both cluster and namespaced resources
- Instance scope is independent of resource scope
- Users should explicitly declare intent

### Alternative 3: Boolean `namespaced` Field

**Approach**: Use a boolean `namespaced` field instead of enum.

**Rejected Because**:
- Less explicit and self-documenting
- Enum provides better validation and future extensibility
- Matches Kubernetes CRD API pattern (`scope` field)

## Scoping

#### What is in scope for this proposal?

- Adding `scope` field to RGD schema
- CRD generation with correct scope
- Controller handling of cluster-scoped instances
- Status updates for cluster-scoped instances
- Unit tests for scope functionality
- Documentation updates

#### What is not in scope?

- Migration tooling for existing RGDs (not needed - defaults preserve behavior)
- Scope conversion/migration (scope is immutable by design)
- Per-resource scope control (resources maintain their own scope)
- Webhook validation for scope changes (handled by XValidation)

## Testing Strategy

#### Requirements

- Unit tests for CRD scope generation
- Unit tests for scope validation and immutability
- Integration tests for cluster-scoped instance creation
- Controller tests for cluster-scoped status updates

#### Test Plan

1. **Unit Tests**:
   - Verify CRD scope is set correctly for both `Namespaced` and `Cluster`
   - Verify default is `Namespaced` when scope is omitted
   - Verify immutability validation works
   - Verify enum validation rejects invalid values

2. **Integration Tests**:
   - Create cluster-scoped RGD and verify generated CRD is cluster-scoped
   - Create instance of cluster-scoped CRD and verify reconciliation works
   - Verify status updates work for cluster-scoped instances

3. **Backward Compatibility Tests**:
   - Verify existing namespaced RGDs continue to work
   - Verify default behavior when scope is not specified

## Benefits

1. **Flexibility**: Enables RGDs for cluster-level infrastructure and policies
2. **Kubernetes Alignment**: Matches Kubernetes CRD scope model
3. **Backward Compatible**: Defaults preserve existing behavior
4. **Explicit Intent**: Users explicitly declare scope, making intent clear

## Tradeoffs

1. **API Surface**: Adds a new field to the Schema API (minimal impact)
2. **Complexity**: Slightly increases code complexity in CRD generation and controller
3. **Validation**: Requires immutability validation (handled by XValidation)

## Potential Impact

### Positive Impact

- Enables new use cases for cluster-scoped resources
- Aligns with Kubernetes patterns and user expectations
- Minimal disruption to existing users (backward compatible)

### Risks and Mitigations

1. **Risk**: Users might accidentally create cluster-scoped CRDs
   - **Mitigation**: Default to `Namespaced`, requires explicit opt-in

2. **Risk**: Scope immutability might be confusing
   - **Mitigation**: Clear documentation and validation error messages

3. **Risk**: Controller bugs with cluster-scoped instances
   - **Mitigation**: Comprehensive testing, especially for status updates

## Discussion and Notes

- This proposal addresses [issue #806](https://github.com/kubernetes-sigs/kro/issues/806)
- Maintainer feedback requested explicit scope field (enum) rather than boolean
- Scope immutability matches Kubernetes CRD behavior
- Default to `Namespaced` ensures zero breaking changes
