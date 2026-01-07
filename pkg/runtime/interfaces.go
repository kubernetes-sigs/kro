// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtime

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

// Interface defines the main runtime interface for managing and synchronizing
// resources.
//
// Note: The use of interfaces here is to allow for easy testing and mocking of
// the runtime the instance controller uses. e.g we can create a fake runtime
// that returns a specific set of resources for testing purposes.
//
// The other reason for the interface is to allow for different implementations
type Interface interface {
	// Synchronize attempts to resolve as many resources as possible.
	// It returns true if the user should call Synchronize again, and false if all
	// resources are resolved. An error is returned if the synchronization process
	// encounters any issues.
	Synchronize() (bool, error)

	// TopologicalOrder returns the topological order of resources.
	TopologicalOrder() []string

	// ResourceDescriptor returns the descriptor for a given resource ID.
	// The descriptor provides metadata about the resource.
	ResourceDescriptor(resourceID string) ResourceDescriptor

	// GetResource retrieves a resource by its ID. It returns the resource object
	// and its current state. If the resource is not found or not yet resolved,
	// it returns nil and the appropriate ResourceState.
	GetResource(resourceID string) (*unstructured.Unstructured, ResourceState)

	// SetResource updates or sets a resource in the runtime. This is typically
	// called after a resource has been created or updated in the cluster.
	SetResource(resourceID string, obj *unstructured.Unstructured)

	// GetInstance returns the main instance object managed by this runtime.
	GetInstance() *unstructured.Unstructured

	// SetInstance updates the main instance object.
	// This is typically called after the instance has been updated in the cluster.
	SetInstance(obj *unstructured.Unstructured)

	// IsResourceReady returns true if the resource is ready, and false otherwise.
	IsResourceReady(resourceID string) (bool, string, error)

	// ReadyToProcessResource returns true if all the condition expressions return true
	// if not it will add itself to the ignored resources
	ReadyToProcessResource(resourceID string) (bool, error)

	// AreDependenciesReady returns true if all the dependencies of the resource are ready
	AreDependenciesReady(resourceID string) bool

	// IgnoreResource ignores resource that has a condition expressison that evaluated
	// to false
	IgnoreResource(resourceID string)

	// ExpandCollection evaluates the forEach expressions for a collection resource
	// and returns the expanded resources. Each returned resource has its template
	// expressions resolved with the iterator values for that combination.
	// Returns an error if the resource is not a collection or if expansion fails.
	ExpandCollection(resourceID string) ([]*unstructured.Unstructured, error)

	// GetCollectionResources returns all expanded resources for a collection in order.
	// The returned slice is guaranteed to be in the same order as originally stored
	// via SetCollectionResources (index 0, 1, 2, ...).
	//
	// Returns:
	//   - (items, ResourceStateResolved) if applied (items may be empty slice for zero-item collections)
	//   - (nil, ResourceStateResolved) if ready to expand but not yet applied
	//   - (nil, ResourceStateWaitingOnDependencies) if dependencies not ready
	//
	// IMPORTANT: Use `items != nil` (not `len(items) > 0`) to check if applied.
	GetCollectionResources(resourceID string) ([]*unstructured.Unstructured, ResourceState)

	// SetCollectionResources stores all expanded resources for a collection at once.
	// The input slice MUST be in the same order as returned by ExpandCollection.
	// The slice index becomes the storage index (0, 1, 2, ...).
	// This should be called after all items have been applied to K8s.
	SetCollectionResources(resourceID string, resources []*unstructured.Unstructured)

	// IsCollectionReady checks if all expanded resources in a collection are ready.
	// Returns true only if all resources satisfy their readyWhen expressions.
	IsCollectionReady(resourceID string) (bool, string, error)
}

// ResourceDescriptor provides metadata about a resource.
//
// Note: the reason why we do not import resourcegraphdefinition/graph.Resource here is
// to avoid a circular dependency between the runtime and the graph packages.
// Had to scratch my head for a while to figure this out. But here is the
// quick overview:
//
//  1. The runtime package depends on how resources are defined in the graph
//     package.
//
//  2. The graph package needs to instantiate a runtime instance during
//     the reconciliation process.
//
//  3. The graph package needs to classify the variables and dependencies of
//     a resource to build the graph. The runtime package needs to know about
//     these variables and dependencies to resolve the resources.
//     This utility is moved to the `types` package. (Thinking about moving it
//     to a new package called "internal/typesystem/variables")
type ResourceDescriptor interface {
	// GetGroupVersionResource returns the k8s GVR for this resource. Note that
	// we don't use the GVK (GroupVersionKind) because the dynamic client needs
	// the GVR to interact with the API server. Yep, it's a bit unfortunate.
	GetGroupVersionResource() schema.GroupVersionResource

	// GetVariables returns the list of variables associated with this resource.
	GetVariables() []*variable.ResourceField

	// GetDependencies returns the list of resource IDs that this resource
	// depends on.
	GetDependencies() []string

	// GetReadyWhenExpressions returns the list of expressions that need to be
	// evaluated before the resource is considered ready.
	GetReadyWhenExpressions() []string

	// GetIncludeWhenExpressions returns the list of expressions that need to
	// be evaluated before deciding whether to create a resource
	GetIncludeWhenExpressions() []string

	// IsNamespaced returns true if the resource is namespaced, and false if it's
	// cluster-scoped.
	IsNamespaced() bool

	// IsExternalRef returns true if the resource is marked as an external reference
	// This is used for external references
	IsExternalRef() bool

	// IsCollection returns true if the resource has forEach iterators,
	// meaning it will expand into multiple resources at runtime.
	IsCollection() bool
}

// Resource extends `ResourceDescriptor` to include the actual resource data.
type Resource interface {
	ResourceDescriptor

	// Unstructured returns the resource data as an unstructured.Unstructured
	// object.
	Unstructured() *unstructured.Unstructured
}

// ForEachDimensionInfo provides the information needed to expand a collection.
// This is the runtime view of a forEach dimension (api/v1alpha1.ForEachDimension).
type ForEachDimensionInfo struct {
	// Name is the variable name used in templates (e.g., "region")
	Name string
	// Expression is the CEL expression that evaluates to a collection
	Expression string
}

// CollectionResource extends Resource with collection-specific methods.
// Resources that have forEach dimensions implement this interface.
type CollectionResource interface {
	Resource

	// GetForEachDimensionInfo returns the forEach dimension information.
	// This allows the runtime to access dimension data without importing graph.
	GetForEachDimensionInfo() []ForEachDimensionInfo
}
