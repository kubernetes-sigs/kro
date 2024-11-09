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

	"github.com/awslabs/kro/api/v1alpha1"
	instancectrl "github.com/awslabs/kro/internal/controller/instance"
	"github.com/awslabs/kro/internal/controller/resourcegroup/errors"
	"github.com/awslabs/kro/internal/dynamiccontroller"
	"github.com/awslabs/kro/internal/graph"
	"github.com/awslabs/kro/internal/metadata"
)

func (r *ResourceGroupReconciler) reconcileResourceGroup(ctx context.Context, rg *v1alpha1.ResourceGroup) ([]string, []v1alpha1.ResourceInformation, error) {
	log, _ := logr.FromContext(ctx)

	log.V(1).Info("Reconciling resource group graph")
	processedRG, resourcesInformation, err := r.reconcileResourceGroupGraph(ctx, rg)
	if err != nil {
		return nil, nil, errors.NewReconcileGraphError(err)
	}

	log.V(1).Info("Reconciling resource group CRD")
	err = r.reconcileResourceGroupCRD(ctx, processedRG.Instance.GetCRD())
	if err != nil {
		return processedRG.TopologicalOrder, resourcesInformation, err
	}

	rgLabeler := metadata.NewResourceGroupLabeler(rg)
	// Merge the ResourceGroupLabeler with the KroLabeler
	graphExecLabeler, err := r.metadataLabeler.Merge(rgLabeler)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to merge labelers: %w", err)
	}

	gvr := processedRG.Instance.GetGroupVersionResource()
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
		r.clientSet,
		rg.Spec.DefaultServiceAccounts,
		graphExecLabeler,
	)

	log.V(1).Info("Reconcile resource group micro controller")
	err = r.reconcileResourceGroupMicroController(ctx, &gvr, graphexecController.Reconcile)
	if err != nil {
		return processedRG.TopologicalOrder, resourcesInformation, err
	}

	return processedRG.TopologicalOrder, resourcesInformation, nil
}

func (r *ResourceGroupReconciler) reconcileResourceGroupGraph(_ context.Context, rg *v1alpha1.ResourceGroup) (*graph.Graph, []v1alpha1.ResourceInformation, error) {
	processedRG, err := r.rgBuilder.NewResourceGroup(rg)
	if err != nil {
		return nil, nil, errors.NewReconcileGraphError(err)
	}

	resourcesInformation := make([]v1alpha1.ResourceInformation, 0, len(processedRG.Resources))

	for name, resource := range processedRG.Resources {
		if len(resource.GetDependencies()) > 0 {
			d := make([]v1alpha1.Dependency, 0, len(resource.GetDependencies()))
			for _, dependency := range resource.GetDependencies() {
				d = append(d, v1alpha1.Dependency{Name: dependency})
			}
			resourcesInformation = append(resourcesInformation, v1alpha1.ResourceInformation{
				Name:         name,
				Dependencies: d,
			})
		}
	}

	return processedRG, resourcesInformation, nil
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
