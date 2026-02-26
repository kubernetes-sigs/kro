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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

// reconcileDeletion drives deletion workflow for an instance.
func (c *Controller) reconcileDeletion(rcx *ReconcileContext) error {
	rcx.StateManager.State = InstanceStateDeleting
	rcx.Mark.ResourcesUnderDeletion("deleting resources")

	deletionNode, err := c.planNodesForDeletion(rcx)
	if err != nil {
		return err
	}

	if deletionNode != nil {
		state := rcx.StateManager.NodeStates[deletionNode.Spec.Meta.ID]
		if err := c.deleteTarget(rcx, deletionNode, state); err != nil {
			return err
		}
		// Deletion is in progress; requeue.
		return rcx.delayedRequeue(fmt.Errorf("deleting resource %s", deletionNode.Spec.Meta.ID))
	}

	return c.removeFinalizer(rcx)
}

// planNodesForDeletion resolves identities and observes existing objects to
// select the last deletable node (topologically).
func (c *Controller) planNodesForDeletion(
	rcx *ReconcileContext,
) (*runtime.Node, error) {
	var deletionNode *runtime.Node

	// Loop through nodes in topological order and try to observe their state.
	// stop at the first node that can't be observed (e.g. due to pending data).
	for _, node := range rcx.Runtime.Nodes() {
		rid := node.Spec.Meta.ID
		nodeMeta := node.Spec.Meta

		state := rcx.StateManager.NewNodeState(rid)

		// 1/ check if the node is ignored.
		ignored, err := node.IsIgnored()
		if err != nil {
			state.SetError(err)
			return nil, err
		}
		if ignored {
			state.SetSkipped()
			continue
		}

		// Resolve identity up front so deletion never blocks on readiness or full template
		// resolution. If we can't get a stable identity, we treat the node as deleted.
		desired, err := node.GetDesiredIdentity()
		if err != nil {
			if runtime.IsDataPending(err) {
				// If identity can't be resolved during deletion, treat it as deleted. There is
				// a case where identity depends on another resource that lost some data and we
				// can't resolve it anymore. For that case we need a better deletion/versioning/tracking
				// mechanism - as of today it is unsolved.
				state.SetDeleted()
				continue
			}
			state.SetError(err)
			return nil, err
		}
		if len(desired) == 0 {
			state.SetDeleted()
			continue
		}

		// At this point, identity is resolvable and we can safely observe (GET/LIST)
		// to find the next deletable node.
		switch nodeMeta.Type {
		case graph.NodeTypeExternal, graph.NodeTypeExternalCollection:
			state.SetSkipped()
			continue

		case graph.NodeTypeInstance:
			panic(fmt.Sprintf("unexpected instance node in deletion: %s", rid))

		case graph.NodeTypeCollection:
			// Collections are label-selected and can span namespaces; LIST once and
			// set observed so runtime can compute delete targets in desired order.
			//
			// Differently from single resources, we do not do GETs per-item here because
			// that would be inefficient and cause many API calls during deletion.
			items, err := c.listCollectionItems(rcx, nodeMeta.GVR, rid)
			if err != nil {
				state.SetError(err)
				return nil, fmt.Errorf("failed to list collection items for %s: %w", rid, err)
			}
			if len(items) == 0 {
				state.SetDeleted()
				continue
			}
			node.SetObserved(items)
			state.SetInProgress()
			deletionNode = node

		case graph.NodeTypeResource:
			// Single resources delete by identity; GET the object to mark observed and
			// allow DeleteTargets to return the correct target.
			obj := desired[0]
			rc := resourceClientFor(rcx, nodeMeta, obj.GetNamespace())
			observed, err := rc.Get(rcx.Ctx, obj.GetName(), metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					state.SetDeleted()
					continue
				}
				state.SetError(err)
				return nil, err
			}
			node.SetObserved([]*unstructured.Unstructured{observed})
			state.SetInProgress()
			deletionNode = node

		default:
			panic(fmt.Sprintf("unknown node type: %v", nodeMeta.Type))
		}
	}

	return deletionNode, nil
}

// deleteTarget issues delete requests for the node's delete targets and updates state.
func (c *Controller) deleteTarget(
	rcx *ReconcileContext,
	node *runtime.Node,
	state *NodeState,
) error {
	targets, err := node.DeleteTargets()
	if err != nil {
		state.SetError(err)
		return err
	}
	if len(targets) == 0 {
		state.SetDeleted()
		return nil
	}

	// Track whether any delete request was accepted. a successful Delete does NOT
	// mean the object is gone yet, just that deletion is in progress.
	anyDeleted := false
	for _, target := range targets {
		rc := resourceClientFor(rcx, node.Spec.Meta, target.GetNamespace())
		err := rc.Delete(rcx.Ctx, target.GetName(), metav1.DeleteOptions{})
		if apierrors.IsNotFound(err) {
			// Already gone: leave anyDeleted as is and keep checking others.
			continue
		}
		if err != nil {
			state.SetError(err)
			return err
		}

		// at least one delete call was accepted by the API server.
		anyDeleted = true
	}

	if !anyDeleted {
		// All targets were NotFound, so the node is fully deleted.
		state.SetDeleted()
		return nil
	}

	// At least one delete call succeeded; resources may still be terminating.
	state.SetDeleting()
	return nil
}

// removeFinalizer clears managed state on the instance after deletions complete.
func (c *Controller) removeFinalizer(rcx *ReconcileContext) error {
	// Clean up coordinator watch requests before removing the finalizer.
	if c.coordinator != nil {
		c.coordinator.RemoveInstance(c.gvr, types.NamespacedName{
			Name:      rcx.Instance.GetName(),
			Namespace: rcx.Instance.GetNamespace(),
		})
	}

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

// setUnmanaged removes the instance finalizer using SSA.
func (c *Controller) setUnmanaged(rcx *ReconcileContext, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if exist := metadata.HasInstanceFinalizer(obj); !exist {
		return obj, nil
	}
	rcx.Log.Info("Removing managed state", "name", obj.GetName(), "namespace", obj.GetNamespace())
	instancePatch := &unstructured.Unstructured{}
	instancePatch.SetUnstructuredContent(map[string]interface{}{"apiVersion": obj.GetAPIVersion(), "kind": obj.GetKind(), "metadata": map[string]interface{}{"name": obj.GetName(), "namespace": obj.GetNamespace()}})
	instancePatch.SetFinalizers(obj.GetFinalizers())
	metadata.RemoveInstanceFinalizer(instancePatch)
	updated, err := rcx.InstanceClient().Apply(rcx.Ctx, instancePatch.GetName(), instancePatch, metav1.ApplyOptions{FieldManager: FieldManagerForLabeler, Force: true})
	if err != nil {
		return nil, fmt.Errorf("failed to update unmanaged state: %w", err)
	}
	return updated, nil
}
