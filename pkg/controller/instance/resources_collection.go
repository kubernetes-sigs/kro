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

package instance

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

// CollectionInfo holds collection item metadata for decorator.
type CollectionInfo struct {
	Index int
	Size  int
}

// processCollectionNode builds applyset inputs for a collection node and
// aligns observed items to desired items. Returns resources, node state, and error.
func (c *Controller) processCollectionNode(
	rcx *ReconcileContext,
	node *runtime.Node,
	expandedResources []*unstructured.Unstructured,
) ([]applyset.Resource, NodeState, error) {
	id := node.Spec.Meta.ID
	nodeMeta := node.Spec.Meta
	gvr := nodeMeta.GVR

	collectionSize := len(expandedResources)

	// LIST all existing collection items with single call (more efficient than N GETs)
	existingItems, err := c.listCollectionItems(rcx, gvr, id)
	if err != nil {
		listErr := fmt.Errorf("failed to list collection items: %w", err)
		return nil, errorState(listErr), listErr
	}

	// Empty collection: observed is set (possibly with orphans to prune), mark ready.
	if collectionSize == 0 {
		node.SetObserved(existingItems)
		return nil, readyState(), nil
	}

	// Build lookup map for current items keyed by namespace/name.
	existingByKey := make(map[string]*unstructured.Unstructured, len(existingItems))
	for _, current := range existingItems {
		key := current.GetNamespace() + "/" + current.GetName()
		existingByKey[key] = current
	}

	// Register a single collection watch for the forEach node. The selector
	// matches all items owned by this instance + node, which is the same
	// selector listCollectionItems uses to LIST them.
	selector := labels.SelectorFromSet(labels.Set{
		metadata.InstanceIDLabel: string(rcx.Instance.GetUID()),
		metadata.NodeIDLabel:     id,
	})
	requestCollectionWatch(rcx, id, gvr, rcx.Instance.GetNamespace(), selector)

	for _, expandedResource := range expandedResources {
		key := expandedResource.GetNamespace() + "/" + expandedResource.GetName()
		current := existingByKey[key]
		if current != nil && current.GetDeletionTimestamp() != nil {
			rcx.Log.V(1).Info("Collection resource is terminating; waiting for deletion to complete",
				"id", id,
				"namespace", current.GetNamespace(),
				"name", current.GetName(),
			)
			return nil, deletingState(), newResourceDeletingError(id, current)
		}
	}

	// Pass unordered observed items to runtime; it will align them to desired
	// order by identity.
	node.SetObserved(existingItems)

	// Evaluate lifecycle policy once for the whole collection
	shouldRetain, err := node.ShouldRetain()
	if err != nil {
		shouldRetain = false
	}

	// Build resources list for apply
	resources := make([]applyset.Resource, 0, collectionSize)
	for i, expandedResource := range expandedResources {
		// Apply decorator labels and lifecycle annotation with collection info
		collectionInfo := &CollectionInfo{Index: i, Size: collectionSize}
		c.applyDecoratorLabels(rcx, node, expandedResource, id, collectionInfo, shouldRetain)

		// Look up current revision from LIST results
		key := expandedResource.GetNamespace() + "/" + expandedResource.GetName()
		current := existingByKey[key]

		expandedID := fmt.Sprintf("%s-%d", id, i)
		resources = append(resources, applyset.Resource{
			ID:      expandedID,
			Object:  expandedResource,
			Current: current,
		})
	}

	return resources, inProgressState(), nil
}

// listCollectionItems returns existing collection items.
// Uses a single LIST with label selector instead of N individual GETs.
func (c *Controller) listCollectionItems(
	rcx *ReconcileContext,
	gvr schema.GroupVersionResource,
	nodeID string,
) ([]*unstructured.Unstructured, error) {
	// Filter by both instance UID and node ID for precise matching
	instanceUID := string(rcx.Instance.GetUID())
	selector := fmt.Sprintf("%s=%s,%s=%s",
		metadata.InstanceIDLabel, instanceUID,
		metadata.NodeIDLabel, nodeID,
	)

	// List across all namespaces - collection items may span namespaces
	list, err := rcx.Client.Resource(gvr).List(rcx.Ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, err
	}

	items := make([]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		items[i] = &list.Items[i]
	}
	return items, nil
}

// updateCollectionFromApplyResults maps per-item apply results back to the
// collection node and refreshes the observed list in runtime.
// This operates on already-registered node states from processApplyResults.
func (c *Controller) updateCollectionFromApplyResults(
	_ *ReconcileContext,
	node *runtime.Node,
	state *NodeState,
	byID map[string]applyset.ApplyResultItem,
) error {
	nodeID := node.Spec.Meta.ID
	desiredItems, err := node.GetDesired()
	if err != nil {
		if runtime.IsDataPending(err) {
			return nil
		}
		state.SetError(err)
		return err
	}
	if len(desiredItems) == 0 {
		state.SetReady()
		return nil
	}

	observedItems := make([]*unstructured.Unstructured, 0, len(desiredItems))

	for i := range desiredItems {
		expandedID := fmt.Sprintf("%s-%d", nodeID, i)
		if item, ok := byID[expandedID]; ok {
			if item.Error != nil {
				state.SetError(fmt.Errorf("collection item %d: %w", i, item.Error))
				return nil
			}
			if item.Observed != nil {
				observedItems = append(observedItems, item.Observed)
			}
		}
	}

	node.SetObserved(observedItems)
	setStateFromReadiness(node, state)
	return nil
}
