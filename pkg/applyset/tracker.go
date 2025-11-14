// Copyright 2025 The Kube Resource Orchestrator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package applyset

import (
	"encoding/json"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// ApplyableObject is implemented by objects that can be applied to the cluster.
// We don't need much, so this might allow for more efficient implementations in future.
type Applyable interface {
	// GroupVersionKind returns the GroupVersionKind structure describing the type of the object
	GroupVersionKind() schema.GroupVersionKind
	// GetNamespace returns the namespace of the object
	GetNamespace() string
	// GetName returns the name of the object
	GetName() string

	// GetLabels returns the labels of the object
	GetLabels() map[string]string
	// SetLabels sets the labels of the object
	SetLabels(labels map[string]string)

	// The object should implement json marshalling
	json.Marshaler
}
type ApplyableObject struct {
	*unstructured.Unstructured

	// Optional
	// User provided unique identifier for the object.
	// If present a uniqeness check is done when adding
	// It is opaque and is passed in the callbacks as is
	ID string

	// lastReadRevision is the revision of the object that was last read from the cluster.
	lastReadRevision string
}

func (a *ApplyableObject) String() string {
	return fmt.Sprintf("[%s:%s/%s]", a.GroupVersionKind(), a.GetNamespace(), a.GetName())
}

type k8sObjectKey struct {
	schema.GroupVersionKind
	types.NamespacedName
}

// tracker manages a collection of resources to be applied.
//
// CONCURRENCY SAFETY:
// This type is safe for concurrent use. The Add() and Len() methods can be called
// from multiple goroutines simultaneously. This is critical because the parallel
// DAG walker in controller_reconcile.go calls Add() concurrently as resources are
// resolved.
//
// All exported methods MUST acquire the mutex before accessing any fields.
// The mutex protects:
//   - objects slice (concurrent appends)
//   - serverIDs map (concurrent reads/writes)
//   - clientIDs map (concurrent reads/writes)
//
// When adding new methods or modifying existing ones, ensure proper locking:
//
//	func (t *tracker) NewMethod() {
//	    t.mu.Lock()
//	    defer t.mu.Unlock()
//	    // ... safe to access fields here
//	}
type tracker struct {
	// mu protects all fields below from concurrent access.
	// MUST be held when reading or writing any field.
	mu sync.Mutex

	// objects is a list of objects we are applying.
	// Protected by mu.
	objects []ApplyableObject

	// serverIDs is a map of object key to object.
	// Protected by mu.
	serverIDs map[k8sObjectKey]bool

	// clientIDs is a map of object key to object.
	// Protected by mu.
	clientIDs map[string]bool
}

func NewTracker() *tracker {
	return &tracker{
		serverIDs: make(map[k8sObjectKey]bool),
		clientIDs: make(map[string]bool),
	}
}

func (t *tracker) Add(obj ApplyableObject) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	gvk := obj.GroupVersionKind()

	// Server side uniqueness check
	objectKey := k8sObjectKey{
		GroupVersionKind: gvk,
		NamespacedName: types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      obj.GetName(),
		},
	}

	// detect duplicates in the objects list
	if _, found := t.serverIDs[objectKey]; found {
		return fmt.Errorf("duplicate object %v", objectKey)
	}
	t.serverIDs[objectKey] = true

	// TODO(barney-s): Do we need to care about client side uniqueness?
	// We could just not take the ID (opaque string) and let user deal with mapping
	// GVKNN to their ID. Adding a todo here to revisit this.
	if obj.ID != "" {
		if _, found := t.clientIDs[obj.ID]; found {
			return fmt.Errorf("duplicate object ID %v", obj.ID)
		}
		t.clientIDs[obj.ID] = true
	}

	// Ensure the object is marshallable
	if _, err := json.Marshal(obj); err != nil {
		return fmt.Errorf("object %v is not json marshallable: %w", objectKey, err)
	}

	// Add the object to the tracker
	t.objects = append(t.objects, obj)
	return nil
}

func (t *tracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.objects)
}
