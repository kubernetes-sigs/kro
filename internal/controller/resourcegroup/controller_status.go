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

package resourcegroup

import (
	"context"
	"errors"

	"github.com/go-logr/logr"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/aws-controllers-k8s/symphony/api/v1alpha1"
	"github.com/aws-controllers-k8s/symphony/internal/controller/resourcegroup/condition"
	serr "github.com/aws-controllers-k8s/symphony/internal/controller/resourcegroup/errors"
	"github.com/aws-controllers-k8s/symphony/internal/requeue"
)

// handleReconcileError will handle errors from reconcile handlers, which
// respects runtime errors.
func (r *ResourceGroupReconciler) handleReconcileError(ctx context.Context, err error) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var requeueNeededAfter *requeue.RequeueNeededAfter
	if errors.As(err, &requeueNeededAfter) {
		after := requeueNeededAfter.Duration()
		log.Info(
			"requeue needed after error",
			"error", requeueNeededAfter.Unwrap(),
			"after", after,
		)
		return ctrl.Result{RequeueAfter: after}, nil
	}

	var requeueNeeded *requeue.RequeueNeeded
	if errors.As(err, &requeueNeeded) {
		log.Info(
			"requeue needed error",
			"error", requeueNeeded.Unwrap(),
		)
		return ctrl.Result{Requeue: true}, nil
	}

	var noRequeue *requeue.NoRequeue
	if errors.As(err, &noRequeue) {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, err
}

func (r *ResourceGroupReconciler) setResourceGroupStatus(ctx context.Context, resourcegroup *v1alpha1.ResourceGroup, topologicalOrder []string, resources []v1alpha1.ResourceInformation, reconcileErr error) error {
	log, _ := logr.FromContext(ctx)

	log.V(1).Info("calculating resource group status and conditions")

	dc := resourcegroup.DeepCopy()

	// set conditions
	dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
		condition.NewReconcilerReadyCondition(metav1.ConditionTrue, "", "micro controller is ready"),
	)
	dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
		condition.NewGraphVerifiedCondition(metav1.ConditionTrue, "", "Directed Acyclic Graph is synced"),
	)
	dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
		condition.NewCustomResourceDefinitionSyncedCondition(metav1.ConditionTrue, "", "Custom Resource Definition is synced"),
	)
	dc.Status.State = v1alpha1.ResourceGroupStateActive
	dc.Status.TopologicalOrder = topologicalOrder
	dc.Status.Resources = resources

	if reconcileErr != nil {
		log.V(1).Info("Error occurred during reconcile", "error", reconcileErr)

		// if the error is graph error, graph condition should be false and the rest should be unknown
		var reconcielGraphErr *serr.ReconcileGraphError
		if errors.As(reconcileErr, &reconcielGraphErr) {
			log.V(1).Info("Processing reconcile graph error", "error", reconcileErr)
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewGraphVerifiedCondition(metav1.ConditionFalse, reconcileErr.Error(), "Directed Acyclic Graph is synced"),
			)

			reason := "Faulty Graph"
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewReconcilerReadyCondition(metav1.ConditionUnknown, reason, "micro controller is ready"),
			)
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewCustomResourceDefinitionSyncedCondition(metav1.ConditionUnknown, reason, "Custom Resource Definition is synced"),
			)
		}

		// if the error is crd error, crd condition should be false, graph condition should be true and the rest should be unknown
		var reconcileCRDErr *serr.ReconcileCRDError
		if errors.As(reconcileErr, &reconcileCRDErr) {
			log.V(1).Info("Processing reconcile crd error", "error", reconcileErr)
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewGraphVerifiedCondition(metav1.ConditionTrue, "", "Directed Acyclic Graph is synced"),
			)
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewCustomResourceDefinitionSyncedCondition(metav1.ConditionFalse, reconcileErr.Error(), "Custom Resource Definition is synced"),
			)
			reason := "CRD not-synced"
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewReconcilerReadyCondition(metav1.ConditionUnknown, reason, "micro controller is ready"),
			)
		}

		// if the error is micro controller error, micro controller condition should be false, graph condition should be true and the rest should be unknown
		var reconcileMicroController *serr.ReconcileMicroControllerError
		if errors.As(reconcileErr, &reconcileMicroController) {
			log.V(1).Info("Processing reconcile micro controller error", "error", reconcileErr)
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewGraphVerifiedCondition(metav1.ConditionTrue, "", "Directed Acyclic Graph is synced"),
			)
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewCustomResourceDefinitionSyncedCondition(metav1.ConditionTrue, "", "Custom Resource Definition is synced"),
			)
			dc.Status.Conditions = condition.SetCondition(dc.Status.Conditions,
				condition.NewReconcilerReadyCondition(metav1.ConditionFalse, reconcileErr.Error(), "micro controller is ready"),
			)
		}

		log.V(1).Info("Setting resource group status to INACTIVE", "error", reconcileErr)
		dc.Status.State = v1alpha1.ResourceGroupStateInactive
	}

	log.V(1).Info("Setting resource group status", "status", dc.Status)
	patch := client.MergeFrom(resourcegroup.DeepCopy())
	return r.Status().Patch(ctx, dc.DeepCopy(), patch)
}

func (r *ResourceGroupReconciler) setManaged(ctx context.Context, resourcegroup *v1alpha1.ResourceGroup) error {
	log := log.FromContext(ctx)
	log.V(1).Info("setting resourcegroup as managed - adding finalizer")

	newFinalizers := []string{v1alpha1.SymphonyDomainName}
	dc := resourcegroup.DeepCopy()
	dc.Finalizers = newFinalizers
	if len(dc.Finalizers) != len(resourcegroup.Finalizers) {
		patch := client.MergeFrom(resourcegroup.DeepCopy())
		return r.Patch(ctx, dc.DeepCopy(), patch)
	}
	return nil
}

func (r *ResourceGroupReconciler) setUnmanaged(ctx context.Context, resourcegroup *v1alpha1.ResourceGroup) error {
	log := log.FromContext(ctx)
	log.V(1).Info("setting resourcegroup as unmanaged - removing finalizer")

	newFinalizers := []string{}
	dc := resourcegroup.DeepCopy()
	dc.Finalizers = newFinalizers
	patch := client.MergeFrom(resourcegroup.DeepCopy())
	return r.Patch(ctx, dc.DeepCopy(), patch)
}

func getGVR(customRD *v1.CustomResourceDefinition) *schema.GroupVersionResource {
	return &schema.GroupVersionResource{
		Group: customRD.Spec.Group,
		// Deal with complex versioning later on
		Version:  customRD.Spec.Versions[0].Name,
		Resource: customRD.Spec.Names.Plural,
	}
}
