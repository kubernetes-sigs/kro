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

package controller

import (
	"context"

	"github.com/go-logr/logr"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/aws-controllers-k8s/symphony/api/v1alpha1"
	"github.com/aws-controllers-k8s/symphony/internal/crd"
	"github.com/aws-controllers-k8s/symphony/internal/errors"
	"github.com/aws-controllers-k8s/symphony/internal/k8smetadata"
	"github.com/aws-controllers-k8s/symphony/internal/resourcegroup"
)

func (r *ResourceGroupReconciler) reconcile(ctx context.Context, resourcegroup *v1alpha1.ResourceGroup) error {
	log, _ := logr.FromContext(ctx)
	// if deletion timestamp is set, call cleanupResourceGroup
	if !resourcegroup.DeletionTimestamp.IsZero() {
		log.V(1).Info("ResourceGroup is being deleted")
		err := r.cleanupResourceGroup(ctx, resourcegroup)
		if err != nil {
			return err
		}
		log.V(1).Info("ResourceGroup cleanup complete")

		log.V(1).Info("Setting resourcegroup as unmanaged")
		// remove finalizer
		err = r.setUnmanaged(ctx, resourcegroup)
		if err != nil {
			return err
		}

		return nil
	}

	log.V(1).Info("Ensure resource group is managed")
	// set finalizer
	err := r.setManaged(ctx, resourcegroup)
	if err != nil {
		return err
	}

	log.V(1).Info("Begin reconciling resource group")
	topologicalOrder, reconcileErr := r.reconcileResourceGroup(ctx, resourcegroup)
	if err != nil {
		return err
	}

	log.V(1).Info("Resource group reconciled")

	log.V(1).Info("Setting resourcegroup status")
	// set status
	err = r.setResourceGroupStatus(ctx, resourcegroup, topologicalOrder, reconcileErr)
	if err != nil {
		return err
	}
	return nil
}

func (r *ResourceGroupReconciler) reconcileResourceGroup(ctx context.Context, rg *v1alpha1.ResourceGroup) ([]string, error) {
	log, _ := logr.FromContext(ctx)

	log.V(1).Info("Process open simple schema CRD")
	/* crdResource, gvr, err := processCRD(ctx, resourcegroup)
	if err != nil {
		return nil, err
	} */

	log.V(1).Info("Reconcile resource group graph")
	processedRG, err := r.reconcileResourceGroupGraph(ctx, rg)
	if err != nil {
		return nil, err
	}

	crdResource := crd.NewCRD(
		processedRG.Instance.GroupVersionKind.Version,
		processedRG.Instance.GroupVersionKind.Kind,
		processedRG.Instance.SchemaExt,
	)
	topologicalOrder, err := processedRG.Dag.TopologicalSort()
	if err != nil {
		return nil, errors.NewReconcileGraphError(err)
	}

	log.V(1).Info("Reconcile resource group CRD")
	err = r.reconcileResourceGroupCRD(ctx, crdResource)
	if err != nil {
		return topologicalOrder, err
	}

	gvr := k8smetadata.GVKtoGVR(processedRG.Instance.GroupVersionKind)
	log.V(1).Info("Reconcile resource group micro controller")
	err = r.reconcileResourceGroupMicroController(ctx, &gvr)
	if err != nil {
		return topologicalOrder, err
	}

	return topologicalOrder, nil
}

func (r *ResourceGroupReconciler) reconcileResourceGroupGraph(ctx context.Context, rg *v1alpha1.ResourceGroup) (*resourcegroup.ResourceGroup, error) {
	processedRG, err := r.DynamicController.RegisterWorkflowOperator(
		ctx,
		rg,
	)
	if err != nil {
		return nil, errors.NewReconcileGraphError(err)
	}
	return processedRG, nil
}

func (r *ResourceGroupReconciler) reconcileResourceGroupCRD(ctx context.Context, crd *v1.CustomResourceDefinition) error {
	err := r.CRDManager.Ensure(ctx, *crd)
	if err != nil {
		return errors.NewReconcileCRDError(err)
	}

	return nil
}

func (r *ResourceGroupReconciler) reconcileResourceGroupMicroController(ctx context.Context, gvr *schema.GroupVersionResource) error {
	r.DynamicController.SafeRegisterGVK(*gvr)
	return nil
}
