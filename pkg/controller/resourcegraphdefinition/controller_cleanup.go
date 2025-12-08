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

	"github.com/gobuffalo/flect"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller/watchtracker"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// cleanupResourceGraphDefinition handles the deletion of a ResourceGraphDefinition.
// Returns true if cleanup is complete and finalizer can be removed.
// Returns false if still waiting for microcontroller to stop.
func (r *ResourceGraphDefinitionReconciler) cleanupResourceGraphDefinition(ctx context.Context, rgd *v1alpha1.ResourceGraphDefinition) (bool, error) {
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("cleaning up resource graph definition", "name", rgd.Name)

	gvr := metadata.GetResourceGraphDefinitionInstanceGVR(rgd.Spec.Schema.Group, rgd.Spec.Schema.APIVersion, rgd.Spec.Schema.Kind)
	watchState, found := r.dynamicController.GetWatchState(gvr)

	// Not found means never registered or already stopped - proceed with cleanup
	if !found {
		log.V(1).Info("watch state not found, proceeding with CRD cleanup", "gvr", gvr)
		return r.cleanupCRD(ctx, rgd)
	}

	// Stopped means deregistration completed - proceed with cleanup
	if watchState.Status == watchtracker.StatusStopped {
		log.V(1).Info("microcontroller stopped, proceeding with CRD cleanup", "gvr", gvr)
		return r.cleanupCRD(ctx, rgd)
	}

	// Still active - trigger deregistration and wait
	log.V(1).Info("microcontroller still active, triggering deregistration", "gvr", gvr, "watchStatus", watchState.Status)
	r.dynamicController.Deregister(ctx, gvr)

	if rgd.Status.State != v1alpha1.ResourceGraphDefinitionStateDeactivating {
		r.setDeactivatingState(ctx, rgd)
	}
	return false, nil
}

func (r *ResourceGraphDefinitionReconciler) cleanupCRD(ctx context.Context, rgd *v1alpha1.ResourceGraphDefinition) (bool, error) {
	crdName := extractCRDName(rgd.Spec.Schema.Group, rgd.Spec.Schema.Kind)
	if err := r.cleanupResourceGraphDefinitionCRD(ctx, crdName); err != nil {
		return false, fmt.Errorf("failed to cleanup CRD %s: %w", crdName, err)
	}
	return true, nil
}

func (r *ResourceGraphDefinitionReconciler) setDeactivatingState(ctx context.Context, rgd *v1alpha1.ResourceGraphDefinition) {
	rgd.Status.State = v1alpha1.ResourceGraphDefinitionStateDeactivating
	if err := r.Status().Update(ctx, rgd); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "failed to set deactivating state")
	}
}

// cleanupResourceGraphDefinitionCRD deletes the CRD with the given name if CRD deletion is enabled.
// If CRD deletion is disabled, it logs the skip and returns nil.
func (r *ResourceGraphDefinitionReconciler) cleanupResourceGraphDefinitionCRD(ctx context.Context, crdName string) error {
	log := ctrl.LoggerFrom(ctx)

	if !r.allowCRDDeletion {
		log.Info("skipping CRD deletion (disabled)", "crd", crdName)
		return nil
	}

	log.Info("Deleting CRD", "name", crdName)
	err := r.crdClient.Delete(ctx, crdName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("error deleting CRD: %w", err)
	}
	return nil
}

// extractCRDName generates the CRD name from a given kind by converting it to plural form
// and appending the kro domain name.
func extractCRDName(group, kind string) string {
	return fmt.Sprintf("%s.%s",
		flect.Pluralize(strings.ToLower(kind)),
		group)
}
