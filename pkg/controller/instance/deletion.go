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

package instance

import (
	"encoding/json"
	"fmt"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// reconcileDeletion drives deletion workflow for an instance.
func (c *Controller) reconcileDeletion(rcx *ReconcileContext) error {
	rcx.StateManager.State = v1alpha1.InstanceStateDeleting
	rcx.Mark.ResourcesUnderDeletion("deleting resources")

	candidates, applier, err := c.discoverDeletionInventory(rcx)
	if err != nil {
		return err
	}

	if len(candidates) == 0 {
		return c.removeFinalizer(rcx)
	}

	orders, highest, orderErr := parseDeletionOrders(candidates)
	if orderErr != nil {
		if nodeID := orderErr.nodeID; nodeID != "" {
			rcx.StateManager.SetNodeState(nodeID, errorState(orderErr))
		}
		return orderErr
	}

	c.updateDeletionNodeStates(rcx, candidates)

	conflict := false
	for i, candidate := range candidates {
		if orders[i] != highest || candidate.Object.GetDeletionTimestamp() != nil {
			continue
		}

		result, err := applier.DeleteOrphan(rcx.Ctx, candidate)
		if err != nil {
			if nodeID := candidate.Object.GetLabels()[metadata.NodeIDLabel]; nodeID != "" {
				rcx.StateManager.SetNodeState(nodeID, errorState(err))
			}
			return err
		}
		conflict = conflict || result.Conflict
	}

	if conflict {
		return rcx.delayedRequeue(fmt.Errorf("deletion encountered UID conflicts; retrying"))
	}
	return rcx.delayedRequeue(fmt.Errorf("deleting apply-order wave %d", highest))
}

// discoverDeletionInventory reconstructs the deletion search scope solely from
// the parent ApplySet metadata. It does not evaluate the current graph or CEL.
func (c *Controller) discoverDeletionInventory(
	rcx *ReconcileContext,
) ([]applyset.OrphanCandidate, *applyset.ApplySet, error) {
	applier := c.createApplySet(rcx)
	inventory, err := applier.Project(nil)
	if err != nil {
		return nil, nil, fmt.Errorf("project deletion inventory: %w", err)
	}
	candidates, err := applier.ListOrphans(rcx.Ctx, applyset.PruneOptions{
		KeepUIDs: sets.New[types.UID](),
		Scope:    inventory.PruneScope(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list deletion inventory: %w", err)
	}
	return candidates, applier, nil
}

type deletionOrderError struct {
	nodeID string
	err    error
}

func (e *deletionOrderError) Error() string { return e.err.Error() }
func (e *deletionOrderError) Unwrap() error { return e.err }

// parseDeletionOrders validates the complete inventory before any mutation.
func parseDeletionOrders(candidates []applyset.OrphanCandidate) ([]int, int, *deletionOrderError) {
	orders := make([]int, len(candidates))
	highest := 0
	for i, candidate := range candidates {
		obj := candidate.Object
		raw := obj.GetLabels()[metadata.ApplyOrderLabel]
		validDigits := raw != ""
		for _, digit := range raw {
			validDigits = validDigits && digit >= '0' && digit <= '9'
		}
		order, err := strconv.Atoi(raw)
		if !validDigits || err != nil || order <= 0 {
			gvk := obj.GroupVersionKind().String()
			if obj.GroupVersionKind().Empty() {
				gvk = candidate.GVR.String()
			}
			return nil, 0, &deletionOrderError{
				nodeID: obj.GetLabels()[metadata.NodeIDLabel],
				err: fmt.Errorf(
					"resource %s %s has invalid %s label %q: expected a positive base-10 integer",
					gvk, resourceRef(obj), metadata.ApplyOrderLabel, raw,
				),
			}
		}
		orders[i] = order
		if order > highest {
			highest = order
		}
	}
	return orders, highest, nil
}

func (c *Controller) updateDeletionNodeStates(rcx *ReconcileContext, candidates []applyset.OrphanCandidate) {
	if rcx.Runtime == nil {
		return
	}
	liveNodes := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if nodeID := candidate.Object.GetLabels()[metadata.NodeIDLabel]; nodeID != "" {
			liveNodes[nodeID] = struct{}{}
		}
	}
	for _, node := range rcx.Runtime.Nodes() {
		id := node.Spec.Meta.ID
		switch node.Spec.Meta.Type {
		case graph.NodeTypeExternal, graph.NodeTypeExternalCollection:
			rcx.StateManager.SetNodeState(id, skippedState())
		default:
			if _, exists := liveNodes[id]; exists {
				rcx.StateManager.SetNodeState(id, deletingState())
			} else {
				rcx.StateManager.SetNodeState(id, deletedState())
			}
		}
	}
}

// removeFinalizer clears managed state on the instance after deletions complete.
func (c *Controller) removeFinalizer(rcx *ReconcileContext) error {
	// Clean up coordinator watch requests before removing the finalizer.
	c.coordinator.RemoveInstance(c.gvr, types.NamespacedName{
		Name:      rcx.Instance.GetName(),
		Namespace: rcx.Instance.GetNamespace(),
	})

	patched, err := c.setUnmanaged(rcx, rcx.Instance)
	if err != nil {
		rcx.Mark.InstanceNotManaged("failed removing finalizer: %v", err)
		return err
	}
	rcx.Instance = patched
	if rcx.Runtime != nil {
		rcx.Runtime.Instance().SetObserved([]*unstructured.Unstructured{patched})
	}
	rcx.Mark = NewConditionsMarkerFor(rcx.Instance)
	rcx.Mark.ResourcesUnderDeletion("deleting resources")
	return nil
}

// resourceClientFor returns a client scoped to the node's namespace rules.
func resourceClientFor(
	rcx *ReconcileContext,
	desc graph.NodeMeta,
	namespace string,
) dynamic.ResourceInterface {
	if desc.Namespaced {
		return rcx.Client.Resource(desc.GVR).Namespace(namespace)
	}
	return rcx.Client.Resource(desc.GVR)
}

// setUnmanaged removes the instance finalizer using JSON merge patch with retry on conflict.
// Uses merge patch (not SSA) to avoid field manager ownership blocking finalizer removal.
func (c *Controller) setUnmanaged(rcx *ReconcileContext, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if exist := metadata.HasInstanceFinalizer(obj); !exist {
		return obj, nil
	}
	rcx.Log.Info("Removing managed state", "name", obj.GetName(), "namespace", obj.GetNamespace())

	var updated *unstructured.Unstructured
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Re-fetch fresh object on each retry attempt
		current, err := rcx.InstanceClient().Get(rcx.Ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}

		// Check if finalizer still exists after re-fetch
		if !metadata.HasInstanceFinalizer(current) {
			updated = current
			return nil
		}

		clone := current.DeepCopy()
		metadata.RemoveInstanceFinalizer(clone)

		patchData, err := json.Marshal(map[string]interface{}{
			"metadata": map[string]interface{}{
				"resourceVersion": current.GetResourceVersion(),
				"finalizers":      clone.GetFinalizers(),
			},
		})
		if err != nil {
			return fmt.Errorf("failed to marshal finalizer patch: %w", err)
		}

		updated, err = rcx.InstanceClient().Patch(
			rcx.Ctx,
			current.GetName(),
			types.MergePatchType,
			patchData,
			metav1.PatchOptions{},
		)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to update unmanaged state: %w", err)
	}
	return updated, nil
}
