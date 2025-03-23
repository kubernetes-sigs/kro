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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
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
	log, _ := logr.FromContext(ctx)
	log.V(1).Info("setting resourcegraphdefinition as managed")

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
	log, _ := logr.FromContext(ctx)
	log.V(1).Info("setting resourcegraphdefinition as unmanaged")

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

// reconcileStatus updates the status of the RGD instance based on the status of its child resources
func (r *ResourceGraphDefinitionReconciler) reconcileStatus(ctx context.Context, instance *v1alpha1.ResourceGraphDefinition) error {
	// Get all child resources
	children, err := r.getChildResources(ctx, instance)
	if err != nil {
		return fmt.Errorf("failed to get child resources: %w", err)
	}

	// Check status of all child resources
	allReady := true
	for _, child := range children {
		ready, err := r.isResourceReady(ctx, child)
		if err != nil {
			return fmt.Errorf("failed to check resource status: %w", err)
		}
		if !ready {
			allReady = false
			break
		}
	}

	// Update instance status
	if allReady {
		instance.Status.State = v1alpha1.ResourceGraphDefinitionStateActive
		instance.Status.ObservedGeneration = instance.Generation

		// Update status conditions
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "ResourcesReady",
			Message:            "All resources are ready",
			ObservedGeneration: instance.Generation,
		})
	} else {
		instance.Status.State = v1alpha1.ResourceGraphDefinitionStateInProgress

		// Update status conditions
		meta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "ResourcesPending",
			Message:            "Waiting for resources to be ready",
			ObservedGeneration: instance.Generation,
		})
	}

	// Update the status
	if err := r.Status().Update(ctx, instance); err != nil {
		return fmt.Errorf("failed to update instance status: %w", err)
	}

	return nil
}

// isResourceReady checks if a resource is in ready state
func (r *ResourceGraphDefinitionReconciler) isResourceReady(ctx context.Context, obj client.Object) (bool, error) {
	// Get latest resource state
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return false, err
	}

	// Check resource specific conditions
	switch o := obj.(type) {
	case *ec2v1alpha1.SecurityGroup:
		return o.Status.ACK.Conditions.IsReady(), nil
	// Add cases for other resource types
	default:
		// For resources without specific status checks, consider them ready if they exist
		return true, nil
	}
}

// getChildResources returns all child resources owned by the RGD instance
func (r *ResourceGraphDefinitionReconciler) getChildResources(ctx context.Context, instance *v1alpha1.ResourceGraphDefinition) ([]client.Object, error) {
	var children []client.Object

	// List all resources with owner reference to this instance
	for _, resource := range instance.Spec.Resources {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   resource.Template.APIVersion,
			Kind:    resource.Template.Kind,
			Version: "v1alpha1", // This should be dynamic based on the resource
		})

		if err := r.List(ctx, list, client.MatchingFields{
			"metadata.ownerReferences.uid": string(instance.UID),
		}); err != nil {
			return nil, err
		}

		for _, item := range list.Items {
			children = append(children, &item)
		}
	}

	return children, nil
}
