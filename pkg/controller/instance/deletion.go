package instance

import (
	"fmt"
	"slices"

	"github.com/kubernetes-sigs/kro/pkg/metadata"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
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
