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
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kro-run/kro/api/v1alpha1"
	"github.com/kro-run/kro/pkg/requeue"
)

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

// prepareStatus creates the status object for the instance based on current state.
func (igr *instanceGraphReconciler) prepareStatus() map[string]interface{} {
	status := igr.getResolvedStatus()
	generation := igr.runtime.GetInstance().GetGeneration()

	status["state"] = igr.state.State
	status["conditions"] = igr.prepareConditions(igr.state.ReconcileErr, generation)

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

// prepareConditions creates the conditions array for the instance status.
func (igr *instanceGraphReconciler) prepareConditions(
	reconcileErr error,
	generation int64,
) []interface{} {
	var conditions []interface{}

	// Add primary reconciliation condition
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

	return conditions
}

// patchInstanceStatus updates the status subresource of the instance.
func (igr *instanceGraphReconciler) patchInstanceStatus(ctx context.Context, status map[string]interface{}) error {
	instance := igr.runtime.GetInstance().DeepCopy()
	instance.Object["status"] = status

	_, err := igr.client.Resource(igr.gvr).
		Namespace(instance.GetNamespace()).
		UpdateStatus(ctx, instance, metav1.UpdateOptions{})

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
