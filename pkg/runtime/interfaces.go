// Copyright 2025 The Kube Resource Orchestrator Authors
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

	"github.com/kro-run/kro/pkg/graph/variable"
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

	// WantToCreateResource returns true if all the condition expressions return true
	// if not it will add itself to the ignored resources
	WantToCreateResource(resourceID string) (bool, error)

	// IgnoreResource ignores resource that has a condition expressison that evaluated
	// to false
	IgnoreResource(resourceID string)
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
}

// Resource extends `ResourceDescriptor` to include the actual resource data.
type Resource interface {
	ResourceDescriptor

	// Unstructured returns the resource data as an unstructured.Unstructured
	// object.
	Unstructured() *unstructured.Unstructured
}
