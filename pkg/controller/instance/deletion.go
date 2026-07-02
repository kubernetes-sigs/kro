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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// liveResource pairs a cluster object with its GVR so deletion can issue
// the DELETE call without CEL-based identity resolution.
type liveResource struct {
	Object *unstructured.Unstructured
	GVR    schema.GroupVersionResource
}

// reconcileDeletion drives deletion workflow for an instance.
//
// Instead of resolving resource identities via CEL (which fails when the
// identity depends on another resource that might already be gone), it
// discovers managed resources by their kro ownership labels and deletes
// them directly.
func (c *Controller) reconcileDeletion(rcx *ReconcileContext) error {
	rcx.StateManager.State = v1alpha1.InstanceStateDeleting
	rcx.Mark.ResourcesUnderDeletion("deleting resources")

	live, err := c.discoverLiveResources(rcx)
	if err != nil {
		return err
	}

	if len(live) == 0 {
		return c.removeFinalizer(rcx)
	}

	// nodesDeleting tracks which nodes had at least one resource deleted or
	// already terminating. Absent means no live resources found for that node.
	// Delete errors set state inline and return early, so this set only
	// contains successful outcomes.
	nodesDeleting := map[string]struct{}{}

	for _, res := range live {
		nodeID := res.Object.GetLabels()[metadata.NodeIDLabel]

		if res.Object.GetDeletionTimestamp() != nil {
			nodesDeleting[nodeID] = struct{}{}
			continue
		}

		var rc dynamic.ResourceInterface = rcx.Client.Resource(res.GVR)
		if ns := res.Object.GetNamespace(); ns != "" {
			rc = rcx.Client.Resource(res.GVR).Namespace(ns)
		}
		err := rc.Delete(rcx.Ctx, res.Object.GetName(), metav1.DeleteOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			if nodeID != "" {
				rcx.StateManager.SetNodeState(nodeID, errorState(err))
			}
			return err
		}
		nodesDeleting[nodeID] = struct{}{}
	}

	for _, node := range rcx.Runtime.Nodes() {
		id := node.Spec.Meta.ID
		t := node.Spec.Meta.Type
		if t == graph.NodeTypeExternal || t == graph.NodeTypeExternalCollection {
			rcx.StateManager.SetNodeState(id, skippedState())
		} else if _, deleting := nodesDeleting[id]; deleting {
			rcx.StateManager.SetNodeState(id, deletingState())
		} else {
			rcx.StateManager.SetNodeState(id, deletedState())
		}
	}

	return rcx.delayedRequeue(fmt.Errorf("deleting resources"))
}

// discoverLiveResources finds all managed resources owned by this instance
// using label selectors. One LIST call per unique GVR, across all namespaces.
func (c *Controller) discoverLiveResources(
	rcx *ReconcileContext,
) ([]liveResource, error) {
	gvrs := map[schema.GroupVersionResource]struct{}{}
	for _, node := range rcx.Runtime.Nodes() {
		switch node.Spec.Meta.Type {
		case graph.NodeTypeExternal, graph.NodeTypeExternalCollection, graph.NodeTypeInstance:
			continue
		}
		gvrs[node.Spec.Meta.GVR] = struct{}{}
	}

	instanceUID := string(rcx.Instance.GetUID())
	selector := fmt.Sprintf("%s=%s", metadata.InstanceIDLabel, instanceUID)

	var result []liveResource
	for gvr := range gvrs {
		list, err := rcx.Client.Resource(gvr).List(rcx.Ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list %s for deletion: %w", gvr.Resource, err)
		}
		for i := range list.Items {
			result = append(result, liveResource{
				Object: &list.Items[i],
				GVR:    gvr,
			})
		}
	}

	return result, nil
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
	rcx.Runtime.Instance().SetObserved([]*unstructured.Unstructured{patched})
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
