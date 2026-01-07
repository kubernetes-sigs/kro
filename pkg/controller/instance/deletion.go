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

	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

func (c *Controller) reconcileDeletion(rcx *ReconcileContext) error {
	rcx.StateManager.State = InstanceStateDeleting
	rcx.Mark.ResourcesUnderDeletion("deleting resources")

	order := rcx.Runtime.TopologicalOrder()
	slices.Reverse(order)
	for _, resource := range order {
		if err := c.deleteOne(rcx, resource); err != nil {
			return err
		}
	}

	return c.removeFinalizer(rcx)
}

func (c *Controller) deleteOne(rcx *ReconcileContext, rid string) error {
	desc := rcx.Runtime.ResourceDescriptor(rid)

	if desc.IsExternalRef() {
		rcx.StateManager.ResourceStates[rid] = &ResourceState{State: ResourceStateSkipped}
		return nil
	}

	// Handle collection resources - delete all expanded items
	if desc.IsCollection() {
		return c.deleteCollection(rcx, rid, desc)
	}

	resource, _ := rcx.Runtime.GetResource(rid)
	if resource == nil {
		rcx.StateManager.ResourceStates[rid] = &ResourceState{State: ResourceStateDeleted}
		return nil
	}

	rc := c.getClientFor(rcx, rid)
	err := rc.Delete(rcx.Ctx, resource.GetName(), metav1.DeleteOptions{})
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

func (c *Controller) deleteCollection(rcx *ReconcileContext, rid string, desc runtime.ResourceDescriptor) error {
	gvr := desc.GetGroupVersionResource()
	instance := rcx.Runtime.GetInstance()

	// Find all collection items by label selector
	selector := fmt.Sprintf("%s=%s,%s=%s",
		metadata.NodeIDLabel, rid,
		metadata.InstanceIDLabel, string(instance.GetUID()),
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
		if ns == "" && desc.IsNamespaced() {
			ns = instance.GetNamespace()
		}

		var rc dynamic.ResourceInterface
		base := rcx.Client.Resource(gvr)
		if desc.IsNamespaced() {
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
	inst := rcx.Runtime.GetInstance()
	patched, err := c.setUnmanaged(rcx, inst)
	if err != nil {
		rcx.Mark.InstanceNotManaged("failed removing finalizer: %v", err)
		return err
	}
	rcx.Runtime.SetInstance(patched)
	return nil
}

func (c *Controller) getClientFor(rcx *ReconcileContext, rid string) dynamic.ResourceInterface {
	desc := rcx.Runtime.ResourceDescriptor(rid)
	gvr := desc.GetGroupVersionResource()
	ns := c.getNamespaceFor(rcx, rid)

	if desc.IsNamespaced() {
		return rcx.Client.Resource(gvr).Namespace(ns)
	}
	return rcx.Client.Resource(gvr)
}

func (c *Controller) getNamespaceFor(rcx *ReconcileContext, rid string) string {
	inst := rcx.Runtime.GetInstance()
	resource, _ := rcx.Runtime.GetResource(rid)

	if ns := resource.GetNamespace(); ns != "" {
		return ns
	}
	if ns := inst.GetNamespace(); ns != "" {
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
