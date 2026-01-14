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
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

func (c *Controller) reconcileDeletion(rcx *ReconcileContext) error {
	rcx.StateManager.State = InstanceStateDeleting
	rcx.Mark.ResourcesUnderDeletion("deleting resources")

	// Get nodes and reverse for deletion order (dependents first)
	nodes := rcx.Runtime.Nodes()
	slices.Reverse(nodes)
	for _, node := range nodes {
		if err := c.deleteOne(rcx, node); err != nil {
			return err
		}
	}

	return c.removeFinalizer(rcx)
}

func (c *Controller) deleteOne(rcx *ReconcileContext, node *runtime.Node) error {
	rid := node.Spec.Meta.ID
	desc := node.Spec.Meta

	if desc.Type == graph.NodeTypeExternal {
		rcx.StateManager.ResourceStates[rid] = &ResourceState{State: ResourceStateSkipped}
		return nil
	}

	// Handle collection resources - delete all expanded items
	if desc.Type == graph.NodeTypeCollection {
		return c.deleteCollection(rcx, node)
	}

	desired, err := node.GetDesired()
	if err != nil || len(desired) == 0 {
		// If we can't resolve desired, consider it already deleted or skip.
		rcx.StateManager.ResourceStates[rid] = &ResourceState{State: ResourceStateDeleted}
		return nil
	}
	resource := desired[0]

	rc := c.getClientFor(rcx, node)
	err = rc.Delete(rcx.Ctx, resource.GetName(), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		rcx.StateManager.ResourceStates[rid] = &ResourceState{State: ResourceStateDeleted}
		return nil
	}
	if err != nil {
		rcx.StateManager.ResourceStates[rid] = &ResourceState{State: ResourceStateError, Err: err}
		return err
	}

	rcx.StateManager.ResourceStates[rid] = &ResourceState{State: ResourceStateDeleting}
	return rcx.delayedRequeue(fmt.Errorf("deleting resource %s", rid))
}

func (c *Controller) deleteCollection(rcx *ReconcileContext, node *runtime.Node) error {
	rid := node.Spec.Meta.ID
	desc := node.Spec.Meta
	gvr := desc.GVR

	// Find all collection items by label selector
	selector := fmt.Sprintf("%s=%s,%s=%s",
		metadata.NodeIDLabel, rid,
		metadata.InstanceIDLabel, string(rcx.Instance.GetUID()),
	)

	list, err := rcx.Client.Resource(gvr).List(rcx.Ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		rcx.StateManager.ResourceStates[rid] = &ResourceState{State: ResourceStateError, Err: err}
		return fmt.Errorf("failed to list collection items for %s: %w", rid, err)
	}

	if len(list.Items) == 0 {
		rcx.StateManager.ResourceStates[rid] = &ResourceState{State: ResourceStateDeleted}
		return nil
	}

	// Delete each item
	allDeleted := true
	for _, item := range list.Items {
		ns := item.GetNamespace()
		if ns == "" && desc.Namespaced {
			ns = rcx.Instance.GetNamespace()
		}

		var rc dynamic.ResourceInterface
		base := rcx.Client.Resource(gvr)
		if desc.Namespaced {
			rc = base.Namespace(ns)
		} else {
			rc = base
		}

		err := rc.Delete(rcx.Ctx, item.GetName(), metav1.DeleteOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			rcx.StateManager.ResourceStates[rid] = &ResourceState{
				State: ResourceStateError,
				Err:   fmt.Errorf("failed to delete collection item %s: %w", item.GetName(), err),
			}
			return rcx.StateManager.ResourceStates[rid].Err
		}
		allDeleted = false
	}

	if allDeleted {
		rcx.StateManager.ResourceStates[rid] = &ResourceState{State: ResourceStateDeleted}
		return nil
	}

	rcx.StateManager.ResourceStates[rid] = &ResourceState{State: ResourceStateDeleting}
	return rcx.delayedRequeue(fmt.Errorf("collection deletion in progress for %s", rid))
}

func (c *Controller) removeFinalizer(rcx *ReconcileContext) error {
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

func (c *Controller) getClientFor(rcx *ReconcileContext, node *runtime.Node) dynamic.ResourceInterface {
	desc := node.Spec.Meta
	gvr := desc.GVR

	if desc.Namespaced {
		ns := c.getNamespaceFor(rcx, node)
		return rcx.Client.Resource(gvr).Namespace(ns)
	}
	return rcx.Client.Resource(gvr)
}

func (c *Controller) getNamespaceFor(rcx *ReconcileContext, node *runtime.Node) string {
	desired, err := node.GetDesired()
	if err == nil && len(desired) > 0 {
		if ns := desired[0].GetNamespace(); ns != "" {
			return ns
		}
	}
	if ns := rcx.Instance.GetNamespace(); ns != "" {
		return ns
	}
	return metav1.NamespaceDefault
}

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
