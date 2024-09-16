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
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/aws-controllers-k8s/symphony/api/v1alpha1"
	"github.com/aws-controllers-k8s/symphony/internal/crd"
	"github.com/aws-controllers-k8s/symphony/internal/k8smetadata"
	"github.com/aws-controllers-k8s/symphony/internal/kubernetes"
	"github.com/aws-controllers-k8s/symphony/internal/resourcegroup"
	"github.com/aws-controllers-k8s/symphony/internal/typesystem/celextractor"
)

func (r *ResourceGroupReconciler) cleanupResourceGroup(ctx context.Context, rgResource *v1alpha1.ResourceGroup) error {
	log, _ := logr.FromContext(ctx)

	log.V(1).Info("Cleaning up resource group")

	log.V(1).Info("Processing open api schema")
	restConfig, err := kubernetes.NewRestConfig()
	if err != nil {
		return err
	}

	builder, err := resourcegroup.NewResourceGroupBuilder(restConfig, celextractor.NewCELExpressionParser())
	if err != nil {
		return err
	}

	processedRG, err := builder.NewResourceGroup(rgResource)
	if err != nil {
		return err
	}
	gvr := k8smetadata.GVKtoGVR(processedRG.Instance.GroupVersionKind)

	log.V(1).Info("Shutting down resource group microcontroller")
	err = r.shutdownResourceGroupMicroController(ctx, &gvr)
	if err != nil {
		return err
	}

	crdResource := crd.NewCRD(
		processedRG.Instance.GroupVersionKind.Version,
		processedRG.Instance.GroupVersionKind.Kind,
		processedRG.Instance.SchemaExt,
	)

	log.V(1).Info("Cleaning up resource group CRD", "crd", crdResource.Name)
	err = r.cleanupResourceGroupCRD(ctx, crdResource.Name)
	if err != nil {
		return err
	}

	log.V(1).Info("Cleaning up resource group graph")
	err = r.cleanupResourceGroupGraph(ctx, &gvr)
	if err != nil {
		return err
	}

	return nil
}

func (r *ResourceGroupReconciler) shutdownResourceGroupMicroController(_ context.Context, gvr *schema.GroupVersionResource) error {
	r.DynamicController.UnregisterGVK(*gvr)
	return nil
}

func (r *ResourceGroupReconciler) cleanupResourceGroupCRD(ctx context.Context, crdName string) error {
	if r.AllowCRDDeletion {
		err := r.CRDManager.Delete(ctx, crdName)
		if err != nil {
			return err
		}
	} else {
		r.log.Info("CRD deletion is disabled, skipping CRD deletion", "crd", crdName)
	}
	return nil
}

func (r *ResourceGroupReconciler) cleanupResourceGroupGraph(_ context.Context, gvr *schema.GroupVersionResource) error {
	r.DynamicController.UnregisterWorkflowOperator(*gvr)
	return nil
}
