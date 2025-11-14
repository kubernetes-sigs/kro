// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resourcegraphdefinition

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gobuffalo/flect"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

const (
	crdDeletionRequeueDuration = 500 * time.Millisecond
)

// cleanupResourceGraphDefinition handles the deletion of a ResourceGraphDefinition by shutting down its associated
// microcontroller and cleaning up the CRD if enabled. It executes cleanup operations in order:
// 1. Shuts down the microcontroller
// 2. Deletes the associated CRD (if CRD deletion is enabled)
// Returns a requeue error if CRD deletion is still in progress.
func (r *ResourceGraphDefinitionReconciler) cleanupResourceGraphDefinition(ctx context.Context, rgd *v1alpha1.ResourceGraphDefinition) error {
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("cleaning up resource graph definition", "name", rgd.Name)

	// shutdown microcontroller
	gvr := metadata.GetResourceGraphDefinitionInstanceGVR(rgd.Spec.Schema.Group, rgd.Spec.Schema.APIVersion, rgd.Spec.Schema.Kind)
	if err := r.shutdownResourceGraphDefinitionMicroController(ctx, &gvr); err != nil {
		return fmt.Errorf("failed to shutdown microcontroller: %w", err)
	}

	group := rgd.Spec.Schema.Group
	if group == "" {
		group = v1alpha1.KRODomainName
	}
	// cleanup CRD
	crdName := extractCRDName(group, rgd.Spec.Schema.Kind)
	completed, err := r.cleanupResourceGraphDefinitionCRD(ctx, crdName)
	if err != nil {
		return fmt.Errorf("failed to cleanup CRD %s: %w", crdName, err)
	}

	if !completed {
		log.V(1).Info("CRD deletion in progress, requeuing", "crd", crdName)
		return requeue.NeededAfter(nil, crdDeletionRequeueDuration)
	}

	return nil
}

// shutdownResourceGraphDefinitionMicroController stops the dynamic controller associated with the given GVR.
// This ensures no new reconciliations occur for this resource type.
func (r *ResourceGraphDefinitionReconciler) shutdownResourceGraphDefinitionMicroController(ctx context.Context, gvr *schema.GroupVersionResource) error {
	if err := r.dynamicController.Deregister(ctx, *gvr); err != nil {
		return fmt.Errorf("error stopping service: %w", err)
	}
	return nil
}

// cleanupResourceGraphDefinitionCRD deletes the CRD with the given name if CRD deletion is enabled.
// Returns (completed, error) where completed indicates if cleanup is fully done.
func (r *ResourceGraphDefinitionReconciler) cleanupResourceGraphDefinitionCRD(ctx context.Context, crdName string) (bool, error) {
	if !r.allowCRDDeletion {
		ctrl.LoggerFrom(ctx).Info("skipping CRD deletion (disabled)", "crd", crdName)
		// When CRD deletion is disabled, cleanup is immediately "complete"
		return true, nil
	}

	completed, err := r.deleteCRD(ctx, crdName)
	if err != nil {
		return false, fmt.Errorf("error deleting CRD: %w", err)
	}
	return completed, nil
}

// deleteCRD handles CRD deletion in a non-blocking manner.
// Returns (completed, error) where completed indicates if the CRD is fully deleted.
func (r *ResourceGraphDefinitionReconciler) deleteCRD(ctx context.Context, name string) (bool, error) {
	log := ctrl.LoggerFrom(ctx)

	// Check if CRD exists
	crd, err := r.clientSet.CRD().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// CRD is gone, deletion complete
			return true, nil
		}
		return false, err
	}

	// If CRD has no deletion timestamp, initiate deletion
	if crd.DeletionTimestamp.IsZero() {
		log.V(1).Info("initiating CRD deletion", "crd", name)
		err = r.clientSet.CRD().Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		// Deletion initiated, but not complete yet
		return false, nil
	}

	// CRD has deletion timestamp, deletion is in progress
	log.V(1).Info("CRD deletion in progress", "crd", name, "deletionTimestamp", crd.DeletionTimestamp)
	return false, nil
}

// extractCRDName generates the CRD name from a given kind by converting it to plural form
// and appending the kro domain name.
func extractCRDName(group, kind string) string {
	return fmt.Sprintf("%s.%s",
		flect.Pluralize(strings.ToLower(kind)),
		group)
}
