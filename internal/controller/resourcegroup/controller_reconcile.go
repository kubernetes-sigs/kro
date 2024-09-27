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
	"fmt"
	"time"

	"github.com/go-logr/logr"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/aws-controllers-k8s/symphony/api/v1alpha1"
	instancectrl "github.com/aws-controllers-k8s/symphony/internal/controller/instance"
	"github.com/aws-controllers-k8s/symphony/internal/dynamiccontroller"
	"github.com/aws-controllers-k8s/symphony/internal/errors"
	"github.com/aws-controllers-k8s/symphony/internal/k8smetadata"
	"github.com/aws-controllers-k8s/symphony/internal/resourcegroup"
)

func (r *ResourceGroupReconciler) reconcileResourceGroup(ctx context.Context, rg *v1alpha1.ResourceGroup) ([]string, error) {
	log, _ := logr.FromContext(ctx)

	log.V(1).Info("Reconciling resource group graph")
	processedRG, err := r.reconcileResourceGroupGraph(ctx, rg)
	if err != nil {
		return nil, errors.NewReconcileGraphError(err)
	}

	log.V(1).Info("Reconciling resource group CRD")
	err = r.reconcileResourceGroupCRD(ctx, processedRG.Instance.CRD)
	if err != nil {
		return processedRG.TopologicalOrder, err
	}

	rgLabeler := k8smetadata.NewResourceGroupLabeler(rg)
	// Merge the ResourceGroupLabeler with the SymphonyLabeler
	graphExecLabeler, err := r.metadataLabeler.Merge(rgLabeler)
	if err != nil {
		return nil, fmt.Errorf("failed to merge labelers: %w", err)
	}

	gvr := k8smetadata.GVKtoGVR(processedRG.Instance.GroupVersionKind)
	//id := fmt.Sprintf("%s.%s/%s", gvr.Resource, gvr.Group, gvr.Version)
	instanceLogger := r.rootLogger.WithName("controller." + gvr.Resource)
	// instanceLogger = instanceLogger.WithValues("gvr", id)

	graphexecController := instancectrl.NewController(
		instanceLogger,
		instancectrl.ReconcileConfig{
			DefaultRequeueDuration:    3 * time.Second,
			DeletionGraceTimeDuration: 30 * time.Second,
			DeletionPolicy:            "Delete",
		},
		gvr,
		processedRG,
		r.dynamicClient,
		graphExecLabeler,
	)

	log.V(1).Info("Reconcile resource group micro controller")
	err = r.reconcileResourceGroupMicroController(ctx, &gvr, graphexecController.Reconcile)
	if err != nil {
		return processedRG.TopologicalOrder, err
	}

	return processedRG.TopologicalOrder, nil
}

func (r *ResourceGroupReconciler) reconcileResourceGroupGraph(ctx context.Context, rg *v1alpha1.ResourceGroup) (*resourcegroup.ResourceGroup, error) {
	processedRG, err := r.rgBuilder.NewResourceGroup(rg)
	if err != nil {
		return nil, errors.NewReconcileGraphError(err)
	}
	return processedRG, nil
}

func (r *ResourceGroupReconciler) reconcileResourceGroupCRD(ctx context.Context, crd *v1.CustomResourceDefinition) error {
	err := r.crdManager.Ensure(ctx, *crd)
	if err != nil {
		return errors.NewReconcileCRDError(err)
	}

	return nil
}

func (r *ResourceGroupReconciler) reconcileResourceGroupMicroController(ctx context.Context, gvr *schema.GroupVersionResource, handler dynamiccontroller.Handler) error {
	return r.dynamicController.StartServingGVK(ctx, *gvr, handler)
}
