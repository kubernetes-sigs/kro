// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package instance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/aws-controllers-k8s/symphony/api/v1alpha1"
	"github.com/aws-controllers-k8s/symphony/internal/k8smetadata"
	"github.com/aws-controllers-k8s/symphony/internal/requeue"
	"github.com/aws-controllers-k8s/symphony/internal/resourcegroup"
)

type InstanceGraphReconciler struct {
	originalRequest             ctrl.Request
	log                         logr.Logger
	gvr                         schema.GroupVersionResource
	client                      dynamic.Interface
	rg                          *resourcegroup.ResourceGroup
	runtime                     *resourcegroup.RuntimeResourceGroup
	instanceLabeler             k8smetadata.Labeler
	instanceSubResourcesLabeler k8smetadata.Labeler
}

func (igr *InstanceGraphReconciler) Reconcile(ctx context.Context) error {
	instance := igr.runtime.Instance

	// Resolve static variables e.g all the ${spec.xxx} variables
	igr.log.Info("Resolving static variables (instance spec fields)")
	if err := igr.runtime.ResolveStaticVariables(); err != nil {
		return igr.patchStatusError(ctx, fmt.Errorf("failed to resolve static variables: %w", err), instance.GetGeneration())
	}

	// handle deletion case
	if !instance.GetDeletionTimestamp().IsZero() {
		igr.log.V(1).Info("Handling instance deletion", "deletionTimestamp", instance.GetDeletionTimestamp())
		return igr.handleInstanceDeletion(ctx)
	}

	igr.log.V(1).Info("Reconciling instance", "instance", instance)
	return igr.reconcile(ctx)
}

func (ge *InstanceGraphReconciler) reconcile(ctx context.Context) error {
	instanceUnstructured := ge.runtime.Instance

	patched, err := ge.setManaged(ctx, instanceUnstructured, instanceUnstructured.GetUID())
	if err != nil {
		return ge.patchStatusError(ctx, fmt.Errorf("failed to set managed: %w", err), instanceUnstructured.GetGeneration())
	}

	if patched != nil {
		ge.runtime.Instance.Object = patched.Object
	}

	ge.log.V(1).Info("Reconciling individual resources [following topological order]")
	instanceNamespace := instanceUnstructured.GetNamespace()

	for _, resourceID := range ge.runtime.ResourceGroup.TopologicalOrder {
		ge.log.V(1).Info("Reconciling resource", "resource", resourceID)

		resourceMeta := ge.rg.Resources[resourceID]
		if !ge.runtime.CanResolveResource(resourceID) {
			ge.log.V(1).Info("Resource dependencies not ready", "resource", resourceID)
			return requeue.NeededAfter(fmt.Errorf("resource dependencies not ready"), 3*time.Second)
		}

		if err := ge.runtime.ResolveResource(resourceID); err != nil {
			return ge.patchStatusError(ctx, fmt.Errorf("failed to resolve resource %s: %w", resourceID, err), instanceUnstructured.GetGeneration())
		}

		rUnstructured := ge.runtime.Resources[resourceID]

		rname := rUnstructured.GetName()
		namespace := rUnstructured.GetNamespace()
		// fallback to instance namespace
		if namespace == "" {
			namespace = instanceNamespace
		}

		gvr := k8smetadata.GVKtoGVR(resourceMeta.GroupVersionKind)
		rc := ge.client.Resource(gvr).Namespace(namespace)

		ge.log.V(1).Info("Checking if resource exists", "resource", resourceID)
		observed, err := rc.Get(ctx, rname, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				ge.log.V(1).Info("Resource not found, creating", "resource", resourceID)
				ge.instanceSubResourcesLabeler.ApplyLabels(rUnstructured)

				if _, err := rc.Create(ctx, rUnstructured, metav1.CreateOptions{}); err != nil {
					ge.log.Error(err, "Failed to create resource", "resource", resourceID)
					return ge.patchStatusError(ctx, fmt.Errorf("failed to create resource %s: %w", resourceID, err), instanceUnstructured.GetGeneration())
				}

				ge.log.V(1).Info("Resource created", "resource", resourceID)
				msg := "Reconciliation in progress"
				if err := ge.patchInstanceStatus(ctx, "IN PROGRESS", []*v1alpha1.Condition{
					{
						Type:               v1alpha1.ConditionType("AllResourcesSynced"),
						Status:             corev1.ConditionFalse,
						LastTransitionTime: &metav1.Time{Time: time.Now()},
						Reason:             &msg,
						Message:            &msg,
						ObservedGeneration: instanceUnstructured.GetGeneration(),
					},
				}, nil); err != nil {
					return fmt.Errorf("failed to patch instance status: %w", err)
				}
				return requeue.NeededAfter(fmt.Errorf("resource created"), 3*time.Second)
			} else {
				ge.log.Error(err, "Failed to get resource")
				return ge.patchStatusError(ctx, fmt.Errorf("failed to get resource %s: %w", resourceID, err), instanceUnstructured.GetGeneration())
			}
		}

		ge.log.V(1).Info("Resource exists", "resource", resourceID)
		ge.runtime.SetLatestResource(resourceID, observed)
		if err := ge.runtime.ResolveDynamicVariables(); err != nil {
			return ge.patchStatusError(ctx, fmt.Errorf("failed to resolve dynamic variables: %w", err), instanceUnstructured.GetGeneration())
		}
	}

	ge.log.V(1).Info("All resources are ready")
	ge.log.V(1).Info("Setting instance state to SUCCESS")
	msg := "All resources are ready"

	if err := ge.runtime.ResolveDynamicVariables(); err != nil {
		ge.log.Error(err, "Failed to resolve dynamic variables")
	}
	if err := ge.runtime.ResolveInstanceStatus(); err != nil {
		ge.log.Error(err, "Failed to resolve instance status")
	}

	extra := map[string]interface{}{}
	if ge.runtime.Instance.Object["status"] != nil {
		extra = ge.runtime.Instance.Object["status"].(map[string]interface{})
	}
	if err := ge.patchInstanceStatus(ctx, "ACTIVE", []*v1alpha1.Condition{
		{
			Type:               v1alpha1.ConditionType("AllResourcesSynced"),
			Status:             corev1.ConditionTrue,
			LastTransitionTime: &metav1.Time{Time: time.Now()},
			Reason:             &msg,
			Message:            &msg,
			ObservedGeneration: instanceUnstructured.GetGeneration(),
		},
	}, extra); err != nil {
		return fmt.Errorf("failed to patch instance status: %w", err)
	}

	return nil
}

func (ge *InstanceGraphReconciler) handleInstanceDeletion(ctx context.Context) error {
	ge.log.V(1).Info("Handling instance deletion")

	created := []string{}
	createdNamespacedNames := []struct{ name, namespace string }{}
	instanceNamespace := ge.runtime.Instance.GetNamespace()
	instanceUnstructured := ge.runtime.Instance

	ge.log.V(1).Info("Getting all resources created by Symphony")
	for _, resourceID := range ge.runtime.ResourceGroup.TopologicalOrder {
		_ = ge.runtime.ResolveDynamicVariables()

		resourceMeta := ge.rg.Resources[resourceID]
		if !ge.runtime.CanResolveResource(resourceID) {
			break
		}

		gvk := resourceMeta.GroupVersionKind
		gvr := k8smetadata.GVKtoGVR(gvk)
		if err := ge.runtime.ResolveResource(resourceID); err != nil {
			return ge.patchStatusError(ctx, fmt.Errorf("failed to resolve resource %s: %w", resourceID, err), instanceUnstructured.GetGeneration())
		}

		rUnstructured := ge.runtime.Resources[resourceID]

		rname := rUnstructured.GetName()
		namespace := rUnstructured.GetNamespace()
		if namespace == "" {
			namespace = instanceNamespace
		}

		rc := ge.client.Resource(gvr).Namespace(namespace)
		ge.log.V(1).Info("Checking if resource exists", "resource", resourceID)
		latest, err := rc.Get(ctx, rname, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				ge.log.V(1).Info("Resource not found", "resource", resourceID)
				continue
			}
			ge.log.Error(err, "Failed to get resource")
			return ge.patchStatusError(ctx, fmt.Errorf("failed to get resource %s: %w", resourceID, err), instanceUnstructured.GetGeneration())
		}

		created = append(created, resourceID)
		createdNamespacedNames = append(createdNamespacedNames, struct{ name, namespace string }{rname, namespace})
		ge.runtime.SetLatestResource(resourceID, latest)
		_ = ge.runtime.ResolveDynamicVariables()
	}

	if len(created) == 0 {
		ge.log.V(1).Info("No resources created by Symphony")
		instanceUnstructured := ge.runtime.Instance
		if _, err := ge.setUnmanaged(ctx, instanceUnstructured, instanceUnstructured.GetUID()); err != nil {
			return ge.patchStatusError(ctx, fmt.Errorf("failed to set unmanaged: %w", err), instanceUnstructured.GetGeneration())
		}
		return nil
	}

	ge.log.V(1).Info("Deleting resources in reverse topological order")
	for i := len(created) - 1; i >= 0; i-- {
		resourceID := created[i]
		resourceMeta := ge.rg.Resources[resourceID]

		gvk := resourceMeta.GroupVersionKind
		gvr := k8smetadata.GVKtoGVR(gvk)
		name := createdNamespacedNames[i].name
		namespace := createdNamespacedNames[i].namespace

		rc := ge.client.Resource(gvr).Namespace(namespace)

		ge.log.V(1).Info("Deleting resource", "resource", resourceID)
		if err := rc.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			ge.log.Error(err, "Failed to delete resource")
			return ge.patchStatusError(ctx, fmt.Errorf("failed to delete resource %s: %w", resourceID, err), instanceUnstructured.GetGeneration())
		}
		time.Sleep(3 * time.Second)
	}
	if _, err := ge.setUnmanaged(ctx, instanceUnstructured, instanceUnstructured.GetUID()); err != nil {
		return ge.patchStatusError(ctx, fmt.Errorf("failed to set unmanaged: %w", err), instanceUnstructured.GetGeneration())
	}
	return nil
}

func (ge *InstanceGraphReconciler) setManaged(ctx context.Context, uObj *unstructured.Unstructured, uid types.UID) (*unstructured.Unstructured, error) {
	ge.log.V(1).Info("Setting managed", "resource", uObj.GetName(), "namespace", uObj.GetNamespace())

	dc := uObj.DeepCopy()
	if err := k8smetadata.SetInstanceFinalizerUnstructured(dc, uid); err != nil {
		return nil, fmt.Errorf("failed to set instance finalizer: %w", err)
	}

	ge.instanceLabeler.ApplyLabels(dc)

	if len(dc.GetFinalizers()) != len(uObj.GetFinalizers()) {
		b, err := json.Marshal(dc)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal object: %w", err)
		}

		patched, err := ge.client.Resource(ge.gvr).Namespace(uObj.GetNamespace()).Patch(ctx, uObj.GetName(), types.MergePatchType, b, metav1.PatchOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to patch object: %w", err)
		}
		return patched, nil
	}
	return uObj, nil
}

func (ge *InstanceGraphReconciler) setUnmanaged(ctx context.Context, uObj *unstructured.Unstructured, uid types.UID) (*unstructured.Unstructured, error) {
	ge.log.V(1).Info("Setting unmanaged", "resource", uObj.GetName(), "namespace", uObj.GetNamespace())

	dc := uObj.DeepCopy()
	if err := k8smetadata.RemoveInstanceFinalizerUnstructured(dc, uid); err != nil {
		return nil, fmt.Errorf("failed to remove instance finalizer: %w", err)
	}

	b, err := json.Marshal(dc)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal object: %w", err)
	}

	patched, err := ge.client.Resource(ge.gvr).Namespace(uObj.GetNamespace()).Patch(ctx, uObj.GetName(), types.MergePatchType, b, metav1.PatchOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to patch object: %w", err)
	}
	return patched, nil
}

func (ge *InstanceGraphReconciler) patchInstanceStatus(ctx context.Context, state string, conditions []*v1alpha1.Condition, extra map[string]interface{}) error {
	instanceUnstructuredCopy := ge.runtime.Instance.DeepCopy()
	s := map[string]interface{}{}
	if state != "" {
		s["state"] = state
	}
	if conditions != nil {
		s["conditions"] = conditions
	}
	for k, v := range extra {
		if k != "state" && k != "conditions" {
			s[k] = v
		}
	}

	instanceUnstructuredCopy.Object["status"] = s
	client := ge.client.Resource(ge.gvr)

	_, err := client.Namespace(instanceUnstructuredCopy.GetNamespace()).UpdateStatus(ctx, instanceUnstructuredCopy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to patch instance status: %w", err)
	}
	return nil
}

func (ge *InstanceGraphReconciler) patchStatusError(ctx context.Context, err error, observedGeneration int64) error {
	ge.log.Error(err, "Patching status error")
	msg := err.Error()
	return ge.patchInstanceStatus(ctx, "ERROR", []*v1alpha1.Condition{&v1alpha1.Condition{
		Type:               v1alpha1.ConditionType("ResourceSynced"),
		Status:             corev1.ConditionFalse,
		LastTransitionTime: &metav1.Time{Time: time.Now()},
		Reason:             &msg,
		Message:            &msg,
		ObservedGeneration: observedGeneration,
	}}, nil)
}

func getNamespaceName(req ctrl.Request) (string, string) {
	parts := strings.Split(req.Name, "/")
	name := parts[len(parts)-1]
	namespace := parts[0]
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}
	return namespace, name
}
