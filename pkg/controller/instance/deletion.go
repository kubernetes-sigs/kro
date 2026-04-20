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

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

// reconcileDeletion drives deletion workflow for an instance.
func (c *Controller) reconcileDeletion(rcx *ReconcileContext) error {
	rcx.StateManager.State = v1alpha1.InstanceStateDeleting
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

		isExternal := nodeMeta.Type == graph.NodeTypeExternal || nodeMeta.Type == graph.NodeTypeExternalCollection

		// Resolve identity without readiness gating. External nodes use this to
		// locate the resource for observation; managed nodes use it as the deletion target.
		desired, err := node.GetDesiredIdentity()
		if err != nil {
			if !isExternal && runtime.IsDataPending(err) {
				// Identity depends on a resource that lost its data. Treat as deleted —
				// there is no better mechanism today for tracking identity across data loss.
				state.SetDeleted()
				continue
			}
			state.SetError(err)
			return nil, err
		}

		// External nodes are never deleted by the controller. Observe them so
		// downstream managed nodes can resolve their identity from the CEL context.
		if isExternal {
			if len(desired) > 0 {
				if err := c.observeExternal(rcx, node, desired[0]); err != nil {
					state.SetError(err)
					return nil, err
				}
			}
			state.SetSkipped()
			continue
		}

		if len(desired) == 0 {
			state.SetDeleted()
			continue
		}

		// At this point, identity is resolvable and we can safely observe (GET/LIST)
		// to find the next deletable node.
		switch nodeMeta.Type {

		case graph.NodeTypeInstance:
			panic(fmt.Sprintf("unexpected instance node in deletion: %s", rid))

		case graph.NodeTypeCollection:
			// Collections are label-selected and can span namespaces; LIST once and
			// set observed so runtime can compute delete targets in desired order.
			//
			// Differently from single resources, we do not do GETs per-item here because
			// that would be inefficient and cause many API calls during deletion.
			// listCollectionItems already filters by instance ID + node ID labels,
			// so orphaned resources won't be returned.
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

	// Check if the resource should be retained based on its lifecycle policy
	policy, err := node.Policy()
	if err != nil {
		state.SetError(err)
		return err
	}

	// If policy says to retain, orphan all targets and mark node as deleted.
	if policy.ShouldRetain() {
		for _, target := range targets {
			if err := applyset.RemoveKroLabelsToRetainResource(rcx.Ctx, rcx.Client, node.Spec.Meta.GVR, target.GetNamespace(), target.GetName()); err != nil {
				state.SetError(err)
				return err
			}
			rcx.Log.Info("Orphaned resource due to lifecycle policy", "resource", node.Spec.Meta.ID, "name", target.GetName())
		}
		// All resources orphaned - mark as deleted (from KRO's perspective)
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

// setUnmanaged removes the instance finalizer using SSA.
func (c *Controller) setUnmanaged(rcx *ReconcileContext, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if exist := metadata.HasInstanceFinalizer(obj); !exist {
		return obj, nil
	}
	rcx.Log.Info("Removing managed state", "name", obj.GetName(), "namespace", obj.GetNamespace())
	instancePatch := instanceSSAPatch(obj)
	instancePatch.SetFinalizers(obj.GetFinalizers())
	metadata.RemoveInstanceFinalizer(instancePatch)
	updated, err := rcx.InstanceClient().Apply(rcx.Ctx, instancePatch.GetName(), instancePatch, metav1.ApplyOptions{FieldManager: FieldManagerForLabeler, Force: true})
	if err != nil {
		return nil, fmt.Errorf("failed to update unmanaged state: %w", err)
	}
	return updated, nil
}
