// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package resourcegraphdefinition

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-logr/logr"
	"github.com/kro-run/kro/api/v1alpha1"
	"github.com/kro-run/kro/pkg/metadata"
)

// StatusProcessor handles the processing of ResourceGraphDefinition status updates
type StatusProcessor struct {
	conditions []v1alpha1.Condition
	state      v1alpha1.ResourceGraphDefinitionState
}

// NewStatusProcessor creates a new StatusProcessor with default active state
func NewStatusProcessor() *StatusProcessor {
	return &StatusProcessor{
		conditions: []v1alpha1.Condition{},
		state:      v1alpha1.ResourceGraphDefinitionStateActive,
	}
}

// setDefaultConditions sets the default conditions for an active resource graph definition
func (sp *StatusProcessor) setDefaultConditions() {
	sp.conditions = []v1alpha1.Condition{
		newReconcilerReadyCondition(metav1.ConditionTrue, ""),
		newGraphVerifiedCondition(metav1.ConditionTrue, ""),
		newCustomResourceDefinitionSyncedCondition(metav1.ConditionTrue, ""),
	}
}

// processGraphError handles graph-related errors
func (sp *StatusProcessor) processGraphError(err error) {
	sp.conditions = []v1alpha1.Condition{
		newGraphVerifiedCondition(metav1.ConditionFalse, err.Error()),
		newReconcilerReadyCondition(metav1.ConditionUnknown, "Faulty Graph"),
		newCustomResourceDefinitionSyncedCondition(metav1.ConditionUnknown, "Faulty Graph"),
	}
	sp.state = v1alpha1.ResourceGraphDefinitionStateInactive
}

// processCRDError handles CRD-related errors
func (sp *StatusProcessor) processCRDError(err error) {
	sp.conditions = []v1alpha1.Condition{
		newGraphVerifiedCondition(metav1.ConditionTrue, ""),
		newCustomResourceDefinitionSyncedCondition(metav1.ConditionFalse, err.Error()),
		newReconcilerReadyCondition(metav1.ConditionUnknown, "CRD not-synced"),
	}
	sp.state = v1alpha1.ResourceGraphDefinitionStateInactive
}

// processMicroControllerError handles microcontroller-related errors
func (sp *StatusProcessor) processMicroControllerError(err error) {
	sp.conditions = []v1alpha1.Condition{
		newGraphVerifiedCondition(metav1.ConditionTrue, ""),
		newCustomResourceDefinitionSyncedCondition(metav1.ConditionTrue, ""),
		newReconcilerReadyCondition(metav1.ConditionFalse, err.Error()),
	}
	sp.state = v1alpha1.ResourceGraphDefinitionStateInactive
}

// setResourceGraphDefinitionStatus calculates the ResourceGraphDefinition status and updates it
// in the API server.
func (r *ResourceGraphDefinitionReconciler) setResourceGraphDefinitionStatus(
	ctx context.Context,
	resourcegraphdefinition *v1alpha1.ResourceGraphDefinition,
	topologicalOrder []string,
	resources []v1alpha1.ResourceInformation,
	reconcileErr error,
) error {
	log, _ := logr.FromContext(ctx)
	log.V(1).Info("calculating resource graph definition status and conditions")

	processor := NewStatusProcessor()

	if reconcileErr == nil {
		processor.setDefaultConditions()
	} else {
		log.V(1).Info("processing reconciliation error", "error", reconcileErr)

		var graphErr *graphError
		var crdErr *crdError
		var microControllerErr *microControllerError

		switch {
		case errors.As(reconcileErr, &graphErr):
			processor.processGraphError(reconcileErr)
		case errors.As(reconcileErr, &crdErr):
			processor.processCRDError(reconcileErr)
		case errors.As(reconcileErr, &microControllerErr):
			processor.processMicroControllerError(reconcileErr)
		default:
			log.Error(reconcileErr, "unhandled reconciliation error type")
			return fmt.Errorf("unhandled reconciliation error: %w", reconcileErr)
		}
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy to avoid conflicts
		current := &v1alpha1.ResourceGraphDefinition{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(resourcegraphdefinition), current); err != nil {
			return fmt.Errorf("failed to get current resource graph definition: %w", err)
		}

		// Update status
		dc := current.DeepCopy()
		dc.Status.Conditions = processor.conditions
		dc.Status.State = processor.state
		dc.Status.TopologicalOrder = topologicalOrder
		dc.Status.Resources = resources

		log.V(1).Info("updating resource graph definition status",
			"state", dc.Status.State,
			"conditions", len(dc.Status.Conditions),
		)

		return r.Status().Patch(ctx, dc, client.MergeFrom(current))
	})
}

// setManaged sets the resourcegraphdefinition as managed, by adding the
// default finalizer if it doesn't exist.
func (r *ResourceGraphDefinitionReconciler) setManaged(ctx context.Context, rgd *v1alpha1.ResourceGraphDefinition) error {
	ctrl.LoggerFrom(ctx).V(1).Info("setting resourcegraphdefinition as managed")

	// Skip if finalizer already exists
	if metadata.HasResourceGraphDefinitionFinalizer(rgd) {
		return nil
	}

	dc := rgd.DeepCopy()
	metadata.SetResourceGraphDefinitionFinalizer(dc)
	return r.Patch(ctx, dc, client.MergeFrom(rgd))
}

// setUnmanaged sets the resourcegraphdefinition as unmanaged, by removing the
// default finalizer if it exists.
func (r *ResourceGraphDefinitionReconciler) setUnmanaged(ctx context.Context, rgd *v1alpha1.ResourceGraphDefinition) error {
	ctrl.LoggerFrom(ctx).V(1).Info("setting resourcegraphdefinition as unmanaged")

	// Skip if finalizer already removed
	if !metadata.HasResourceGraphDefinitionFinalizer(rgd) {
		return nil
	}

	dc := rgd.DeepCopy()
	metadata.RemoveResourceGraphDefinitionFinalizer(dc)
	return r.Patch(ctx, dc, client.MergeFrom(rgd))
}

func newReconcilerReadyCondition(status metav1.ConditionStatus, reason string) v1alpha1.Condition {
	return v1alpha1.NewCondition(v1alpha1.ResourceGraphDefinitionConditionTypeReconcilerReady, status, reason, "micro controller is ready")
}

func newGraphVerifiedCondition(status metav1.ConditionStatus, reason string) v1alpha1.Condition {
	return v1alpha1.NewCondition(v1alpha1.ResourceGraphDefinitionConditionTypeGraphVerified, status, reason, "Directed Acyclic Graph is synced")
}

func newCustomResourceDefinitionSyncedCondition(status metav1.ConditionStatus, reason string) v1alpha1.Condition {
	return v1alpha1.NewCondition(v1alpha1.ResourceGraphDefinitionConditionTypeCustomResourceDefinitionSynced, status, reason, "Custom Resource Definition is synced")
}
