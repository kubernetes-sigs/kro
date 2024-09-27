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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/aws-controllers-k8s/symphony/api/v1alpha1"
)

// ResourceState represents the state of a resource.
type ResourceState struct {
	State string
	Err   error
}

func (igr *InstanceGraphReconciler) prepareStatus(instanceState string, reconcileErr error, resourceStates map[string]*ResourceState) map[string]interface{} {
	// Get what ever is resolved in the status
	instanceStatus := igr.getResolvedStatus()

	// Update the instance status with the current state and conditions
	instanceStatus["state"] = instanceState
	instanceStatus["conditions"] = igr.prepareConditions(instanceStatus, reconcileErr, resourceStates, igr.runtime.Instance.GetGeneration())
	return instanceStatus
}

func (igr *InstanceGraphReconciler) getResolvedStatus() map[string]interface{} {
	status := map[string]interface{}{
		"conditions": []interface{}{},
	}

	if err := igr.runtime.ResolveInstanceStatus(); err != nil {
		igr.log.Error(err, "Failed to resolve instance status")
		return status
	}

	resolvedStatus, ok := igr.runtime.Instance.Object["status"].(map[string]interface{})
	if !ok {
		return status
	}

	// clear conditions
	resolvedStatus["conditions"] = []interface{}{}

	return resolvedStatus
}

func (igr *InstanceGraphReconciler) prepareConditions(status map[string]interface{}, reconcileErr error, resourceStates map[string]*ResourceState, generation int64) []interface{} {
	conditions := status["conditions"].([]interface{})

	// Add overall reconciliation condition
	if reconcileErr != nil {
		conditions = append(conditions, createCondition(
			"InstanceSynced",
			corev1.ConditionFalse,
			"ReconciliationFailed",
			reconcileErr.Error(),
			generation,
		))
	} else {
		conditions = append(conditions, createCondition(
			"InstanceSynced",
			corev1.ConditionTrue,
			"ReconciliationSucceeded",
			"Instance reconciled successfully",
			generation,
		))
	}

	conditionType := "ResourceSynced"
	// Add conditions for each resource
	for resourceID, resourceState := range resourceStates {
		if resourceState.Err != nil {
			conditions = append(conditions, createCondition(
				v1alpha1.ConditionType(conditionType),
				corev1.ConditionFalse,
				"Resource sync failed",
				fmt.Sprintf("Resource %s sync failed: %v", resourceID, resourceState.Err),
				generation,
			))
		} else {
			conditions = append(conditions, createCondition(
				v1alpha1.ConditionType(conditionType),
				corev1.ConditionTrue,
				"Resource synced successfully",
				fmt.Sprintf("Resource %s synced successfully", resourceID),
				generation,
			))
		}
	}

	return conditions
}

func (igr *InstanceGraphReconciler) patchInstanceStatus(ctx context.Context, status map[string]interface{}) error {
	instanceUnstructuredCopy := igr.runtime.Instance.DeepCopy()
	instanceUnstructuredCopy.Object["status"] = status

	_, err := igr.client.Resource(igr.gvr).Namespace(instanceUnstructuredCopy.GetNamespace()).UpdateStatus(ctx, instanceUnstructuredCopy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update instance status: %w", err)
	}
	return nil
}

func createCondition(conditionType v1alpha1.ConditionType, status corev1.ConditionStatus, reason, message string, generation int64) map[string]interface{} {
	return map[string]interface{}{
		"type":               string(conditionType),
		"status":             string(status),
		"reason":             reason,
		"message":            message,
		"lastTransitionTime": time.Now().Format(time.RFC3339),
		"observedGeneration": generation,
	}
}
