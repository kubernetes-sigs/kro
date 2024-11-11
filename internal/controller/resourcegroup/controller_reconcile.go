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
	"github.com/awslabs/kro/internal/dynamiccontroller"
	"github.com/awslabs/kro/internal/graph"
	"github.com/awslabs/kro/internal/metadata"
)

// reconcileResourceGroup orchestrates the reconciliation of a ResourceGroup by:
// 1. Processing the resource graph
// 2. Ensuring CRDs are present
// 3. Setting up and starting the microcontroller
func (r *ResourceGroupReconciler) reconcileResourceGroup(ctx context.Context, rg *v1alpha1.ResourceGroup) ([]string, []v1alpha1.ResourceInformation, error) {
	log, _ := logr.FromContext(ctx)

	// Process resource group graph first to validate structure
	log.V(1).Info("reconciling resource group graph")
	processedRG, resourcesInfo, err := r.reconcileResourceGroupGraph(ctx, rg)
	if err != nil {
		return nil, nil, err
	}

	// Ensure CRD exists and is up to date
	log.V(1).Info("reconciling resource group CRD")
	if err := r.reconcileResourceGroupCRD(ctx, processedRG.Instance.GetCRD()); err != nil {
		return processedRG.TopologicalOrder, resourcesInfo, err
	}

	// Setup metadata labeling
	graphExecLabeler, err := r.setupLabeler(rg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to setup labeler: %w", err)
	}

	// Setup and start microcontroller
	gvr := processedRG.Instance.GetGroupVersionResource()
	controller := r.setupMicroController(gvr, processedRG, rg.Spec.DefaultServiceAccounts, graphExecLabeler)

	log.V(1).Info("reconciling resource group micro controller")
	if err := r.reconcileResourceGroupMicroController(ctx, &gvr, controller.Reconcile); err != nil {
		return processedRG.TopologicalOrder, resourcesInfo, err
	}

	return processedRG.TopologicalOrder, resourcesInfo, nil
}

// setupLabeler creates and merges the required labelers for the resource group
func (r *ResourceGroupReconciler) setupLabeler(rg *v1alpha1.ResourceGroup) (metadata.Labeler, error) {
	rgLabeler := metadata.NewResourceGroupLabeler(rg)
	return r.metadataLabeler.Merge(rgLabeler)
}

// setupMicroController creates a new controller instance with the required configuration
func (r *ResourceGroupReconciler) setupMicroController(
	gvr schema.GroupVersionResource,
	processedRG *graph.Graph,
	defaultSVCs map[string]string,
	labeler metadata.Labeler,
) *instancectrl.Controller {

	instanceLogger := r.rootLogger.WithName("controller." + gvr.Resource)

	return instancectrl.NewController(
		instanceLogger,
		instancectrl.ReconcileConfig{
			DefaultRequeueDuration:    3 * time.Second,
			DeletionGraceTimeDuration: 30 * time.Second,
			DeletionPolicy:            "Delete",
		},
		gvr,
		processedRG,
		r.clientSet,
		defaultSVCs,
		labeler,
	)
}

// reconcileResourceGroupGraph processes the resource group to build a dependency graph
// and extract resource information
func (r *ResourceGroupReconciler) reconcileResourceGroupGraph(_ context.Context, rg *v1alpha1.ResourceGroup) (*graph.Graph, []v1alpha1.ResourceInformation, error) {
	processedRG, err := r.rgBuilder.NewResourceGroup(rg)
	if err != nil {
		return nil, nil, newGraphError(err)
	}

	resourcesInfo := make([]v1alpha1.ResourceInformation, 0, len(processedRG.Resources))
	for name, resource := range processedRG.Resources {
		deps := resource.GetDependencies()
		if len(deps) > 0 {
			resourcesInfo = append(resourcesInfo, buildResourceInfo(name, deps))
		}
	}

	return processedRG, resourcesInfo, nil
}

// buildResourceInfo creates a ResourceInformation struct from name and dependencies
func buildResourceInfo(name string, deps []string) v1alpha1.ResourceInformation {
	dependencies := make([]v1alpha1.Dependency, 0, len(deps))
	for _, dep := range deps {
		dependencies = append(dependencies, v1alpha1.Dependency{Name: dep})
	}
	return v1alpha1.ResourceInformation{
		Name:         name,
		Dependencies: dependencies,
	}
}

// reconcileResourceGroupCRD ensures the CRD is present and up to date in the cluster
func (r *ResourceGroupReconciler) reconcileResourceGroupCRD(ctx context.Context, crd *v1.CustomResourceDefinition) error {
	if err := r.crdManager.Ensure(ctx, *crd); err != nil {
		return newCRDError(err)
	}
	return nil
}

// reconcileResourceGroupMicroController starts the microcontroller for handling the resources
func (r *ResourceGroupReconciler) reconcileResourceGroupMicroController(ctx context.Context, gvr *schema.GroupVersionResource, handler dynamiccontroller.Handler) error {
	err := r.dynamicController.StartServingGVK(ctx, *gvr, handler)
	if err != nil {
		return newMicroControllerError(err)
	}
	return nil
}

// Error types for the resourcegroup controller
type (
	graphError           struct{ err error }
	crdError             struct{ err error }
	microControllerError struct{ err error }
)

// Error interface implementation
func (e *graphError) Error() string           { return e.err.Error() }
func (e *crdError) Error() string             { return e.err.Error() }
func (e *microControllerError) Error() string { return e.err.Error() }

// Unwrap interface implementation
func (e *graphError) Unwrap() error           { return e.err }
func (e *crdError) Unwrap() error             { return e.err }
func (e *microControllerError) Unwrap() error { return e.err }

// Error constructors
func newGraphError(err error) error           { return &graphError{err} }
func newCRDError(err error) error             { return &crdError{err} }
func newMicroControllerError(err error) error { return &microControllerError{err} }
