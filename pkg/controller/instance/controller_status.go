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
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/apis"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

const (
	Ready           = "Ready"
	InstanceManaged = "InstanceManaged"
	GraphResolved   = "GraphResolved"
	ResourcesReady  = "ResourcesReady"
)

var instanceConditionTypes = apis.NewReadyConditions(InstanceManaged, GraphResolved, ResourcesReady)

// unstructuredConditionAdapter adapts an unstructured.Unstructured to implement apis.Object
type unstructuredConditionAdapter struct {
	*unstructured.Unstructured
}

// GetConditions implements apis.Object interface
func (u *unstructuredConditionAdapter) GetConditions() []v1alpha1.Condition {
	if conditions, found, err := unstructured.NestedSlice(u.Object, "status", "conditions"); err == nil && found {
		// Marshal the conditions slice to JSON and then unmarshal to []v1alpha1.Condition
		conditionsJSON, err := json.Marshal(conditions)
		if err != nil {
			return []v1alpha1.Condition{}
		}

		var result []v1alpha1.Condition
		if err := json.Unmarshal(conditionsJSON, &result); err != nil {
			return []v1alpha1.Condition{}
		}
		return result
	}
	return []v1alpha1.Condition{}
}

// SetConditions implements apis.Object interface
func (u *unstructuredConditionAdapter) SetConditions(conditions []v1alpha1.Condition) {
	// Marshal the conditions to JSON and then unmarshal to interface{} slice
	conditionsJSON, err := json.Marshal(conditions)
	if err != nil {
		return // Fail silently - could log this in the future
	}

	var conditionsInterface []interface{}
	if err := json.Unmarshal(conditionsJSON, &conditionsInterface); err != nil {
		return // Fail silently - could log this in the future
	}

	if err := unstructured.SetNestedSlice(u.Object, conditionsInterface, "status", "conditions"); err != nil {
		return // Fail silently - could log this in the future
	}
}

// wrapInstance creates an adapter that allows unstructured instances to work with condition management
func wrapInstance(instance *unstructured.Unstructured) *unstructuredConditionAdapter {
	return &unstructuredConditionAdapter{Unstructured: instance}
}

// NewConditionsMarkerFor creates a marker to manage conditions and sub-conditions for instances.
// ```
// Ready
//
//	├─ InstanceManaged - Instance finalizers and labels are properly set
//	├─ GraphResolved - Runtime graph created and all resources resolved
//	└─ ResourcesReady - All resources are created and ready
//
// ```
func NewConditionsMarkerFor(instance *unstructured.Unstructured) *ConditionsMarker {
	wrapped := wrapInstance(instance)
	return &ConditionsMarker{cs: instanceConditionTypes.For(wrapped)}
}

// A ConditionsMarker provides an API to mark conditions onto an instance as the controller does work.
type ConditionsMarker struct {
	cs apis.ConditionSet
}

// InstanceManaged signals the instance has proper finalizers and labels set.
func (m *ConditionsMarker) InstanceManaged() {
	m.cs.SetTrueWithReason(InstanceManaged, "Managed", "instance is properly managed with finalizers and labels")
}

// InstanceNotManaged signals there was an issue setting up the instance management.
func (m *ConditionsMarker) InstanceNotManaged(format string, a ...any) {
	m.cs.SetFalse(InstanceManaged, "ManagementFailed", fmt.Sprintf(format, a...))
}

// GraphResolved signals the runtime graph has been created and resources resolved.
func (m *ConditionsMarker) GraphResolved() {
	m.cs.SetTrueWithReason(GraphResolved, "Resolved", "runtime graph created and all resources resolved")
}

// GraphNotResolved signals there was an issue creating the runtime graph or resolving resources.
func (m *ConditionsMarker) GraphNotResolved(format string, a ...any) {
	m.cs.SetFalse(GraphResolved, "ResolutionFailed", fmt.Sprintf(format, a...))
}

// ResourcesReady signals all resources in the graph are created and ready.
func (m *ConditionsMarker) ResourcesReady() {
	m.cs.SetTrueWithReason(ResourcesReady, "AllResourcesReady", "all resources are created and ready")
}

// ResourcesNotReady signals some resources are not yet ready or failed to be created.
func (m *ConditionsMarker) ResourcesNotReady(format string, a ...any) {
	m.cs.SetFalse(ResourcesReady, "ResourcesNotReady", fmt.Sprintf(format, a...))
}

// ResourcesInProgress signals resources are being processed but not yet ready.
func (m *ConditionsMarker) ResourcesInProgress(format string, a ...any) {
	m.cs.SetUnknownWithReason(ResourcesReady, "ResourcesInProgress", fmt.Sprintf(format, a...))
}

// prepareStatus creates the status object for the instance based on current state.
func (igr *instanceGraphReconciler) prepareStatus() map[string]interface{} {
	status := igr.getResolvedStatus()

	// Set status.state based on readiness
	instance := igr.runtime.GetInstance()
	conditionSet := instanceConditionTypes.For(wrapInstance(instance))
	if conditionSet.IsRootReady() {
		status["state"] = InstanceStateActive
	} else {
		status["state"] = igr.state.State
	}

	// Get conditions from the instance (set by condition markers during reconciliation)
	if conditions := conditionSet.List(); len(conditions) > 0 {
		// Marshal conditions to JSON and then unmarshal to []interface{} to get map[string]interface{} representation
		conditionsJSON, err := json.Marshal(conditions)
		if err == nil {
			var conditionsInterface []interface{}
			if err := json.Unmarshal(conditionsJSON, &conditionsInterface); err == nil {
				status["conditions"] = conditionsInterface
			}
		}
	}

	return status
}

// getResolvedStatus retrieves the current status while preserving non-condition fields.
func (igr *instanceGraphReconciler) getResolvedStatus() map[string]interface{} {
	status := map[string]interface{}{
		"conditions": []interface{}{},
	}

	if existingStatus, ok := igr.runtime.GetInstance().Object["status"].(map[string]interface{}); ok {
		// Copy existing status but reset conditions
		for k, v := range existingStatus {
			if k != "conditions" {
				status[k] = v
			}
		}
	}

	return status
}

// patchInstanceStatus updates the status subresource of the instance.
func (igr *instanceGraphReconciler) patchInstanceStatus(ctx context.Context, status map[string]interface{}) error {
	instance := igr.runtime.GetInstance().DeepCopy()
	instance.Object["status"] = status

	// We are using retry.RetryOnConflict to handle conflicts.
	// This is because this method is called in a defer path and there is no way to return an error.
	// TODO(barney-s): We should explore removing the defer path and returning an error.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		instance, err := igr.client.Resource(igr.gvr).
			Namespace(instance.GetNamespace()).
			Get(ctx, instance.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}
		instance.Object["status"] = status
		_, err = igr.client.Resource(igr.gvr).
			Namespace(instance.GetNamespace()).
			UpdateStatus(ctx, instance, metav1.UpdateOptions{})
		return err
	})

	if err != nil {
		return fmt.Errorf("failed to update instance status: %w", err)
	}
	return nil
}

// updateInstanceState updates the instance state based on reconciliation results
func (igr *instanceGraphReconciler) updateInstanceState() {
	switch igr.state.ReconcileErr.(type) {
	case *requeue.NoRequeue, *requeue.RequeueNeeded, *requeue.RequeueNeededAfter:
		// Keep current state for requeue errors
		return
	default:
		if igr.state.ReconcileErr != nil {
			igr.state.State = InstanceStateError
		} else if igr.state.State != InstanceStateDeleting {
			igr.state.State = InstanceStateActive
		}
	}
}
