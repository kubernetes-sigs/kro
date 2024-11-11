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

// cleanupResourceGroup handles the deletion of a ResourceGroup by shutting down its associated
// microcontroller and cleaning up the CRD if enabled. It executes cleanup operations in order:
// 1. Shuts down the microcontroller
// 2. Deletes the associated CRD (if CRD deletion is enabled)
func (r *ResourceGroupReconciler) cleanupResourceGroup(ctx context.Context, rg *v1alpha1.ResourceGroup) error {
	log, _ := logr.FromContext(ctx)
	log.V(1).Info("cleaning up resource group", "name", rg.Name)

	// shutdown microcontroller
	gvr := metadata.GetResourceGroupInstanceGVR(rg.Spec.Schema.APIVersion, rg.Spec.Schema.Kind)
	if err := r.shutdownResourceGroupMicroController(ctx, &gvr); err != nil {
		return fmt.Errorf("failed to shutdown microcontroller: %w", err)
	}

	// cleanup CRD
	crdName := extractCRDName(rg.Spec.Schema.Kind)
	if err := r.cleanupResourceGroupCRD(ctx, crdName); err != nil {
		return fmt.Errorf("failed to cleanup CRD %s: %w", crdName, err)
	}

	return nil
}

// shutdownResourceGroupMicroController stops the dynamic controller associated with the given GVR.
// This ensures no new reconciliations occur for this resource type.
func (r *ResourceGroupReconciler) shutdownResourceGroupMicroController(ctx context.Context, gvr *schema.GroupVersionResource) error {
	if err := r.dynamicController.StopServiceGVK(ctx, *gvr); err != nil {
		return fmt.Errorf("error stopping service: %w", err)
	}
	return nil
}

// cleanupResourceGroupCRD deletes the CRD with the given name if CRD deletion is enabled.
// If CRD deletion is disabled, it logs the skip and returns nil.
func (r *ResourceGroupReconciler) cleanupResourceGroupCRD(ctx context.Context, crdName string) error {
	if !r.allowCRDDeletion {
		log, _ := logr.FromContext(ctx)
		log.Info("skipping CRD deletion (disabled)", "crd", crdName)
		return nil
	}

	if err := r.crdManager.Delete(ctx, crdName); err != nil {
		return fmt.Errorf("error deleting CRD: %w", err)
	}
	return nil
}

// extractCRDName generates the CRD name from a given kind by converting it to plural form
// and appending the Kro domain name.
func extractCRDName(kind string) string {
	return fmt.Sprintf("%s.%s",
		flect.Pluralize(strings.ToLower(kind)),
		v1alpha1.KroDomainName)
}
