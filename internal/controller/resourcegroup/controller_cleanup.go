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

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/aws-controllers-k8s/symphony/api/v1alpha1"
	"github.com/aws-controllers-k8s/symphony/internal/k8smetadata"
)

func (r *ResourceGroupReconciler) cleanupResourceGroup(ctx context.Context, rg *v1alpha1.ResourceGroup) error {
	log, _ := logr.FromContext(ctx)

	log.V(1).Info("Cleaning up resource group")
	processedRG, err := r.rgBuilder.NewResourceGroup(rg)
	if err != nil {
		return err
	}
	gvr := k8smetadata.GVKtoGVR(processedRG.Instance.GroupVersionKind)

	log.V(1).Info("Shutting down resource group microcontroller")
	err = r.shutdownResourceGroupMicroController(ctx, &gvr)
	if err != nil {
		return err
	}

	crdResource := processedRG.Instance.CRD
	log.V(1).Info("Cleaning up resource group CRD", "crd", crdResource.Name)
	err = r.cleanupResourceGroupCRD(ctx, crdResource.Name)
	if err != nil {
		return err
	}

	return nil
}

func (r *ResourceGroupReconciler) shutdownResourceGroupMicroController(ctx context.Context, gvr *schema.GroupVersionResource) error {
	return r.dynamicController.StopServiceGVK(ctx, *gvr)
}

func (r *ResourceGroupReconciler) cleanupResourceGroupCRD(ctx context.Context, crdName string) error {
	if r.allowCRDDeletion {
		err := r.crdManager.Delete(ctx, crdName)
		if err != nil {
			return err
		}
	} else {
		r.log.Info("CRD deletion is disabled, skipping CRD deletion", "crd", crdName)
	}
	return nil
}
