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
	"strings"

	"github.com/go-logr/logr"
	"github.com/gobuffalo/flect"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/awslabs/kro/api/v1alpha1"
	"github.com/awslabs/kro/internal/metadata"
)

func (r *ResourceGroupReconciler) cleanupResourceGroup(ctx context.Context, rg *v1alpha1.ResourceGroup) error {
	log, _ := logr.FromContext(ctx)

	log.V(1).Info("Cleaning up resource group")
	gvr := metadata.GetResourceGroupInstanceGVR(rg.Spec.Schema.APIVersion, rg.Spec.Schema.Kind)

	log.V(1).Info("Shutting down resource group microcontroller")
	err := r.shutdownResourceGroupMicroController(ctx, &gvr)
	if err != nil {
		return err
	}

	crdName := r.extractCRDName(rg.Spec.Schema.Kind)
	log.V(1).Info("Cleaning up resource group CRD", "crd", crdName)
	err = r.cleanupResourceGroupCRD(ctx, crdName)
	if err != nil {
		return err
	}

	return nil
}

func (r *ResourceGroupReconciler) extractCRDName(kind string) string {
	pluralKind := flect.Pluralize(strings.ToLower(kind))
	return fmt.Sprintf("%s.%s", pluralKind, v1alpha1.KroDomainName)
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
