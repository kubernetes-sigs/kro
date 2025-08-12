// Copyright 2025 The Kube Resource Orchestrator Authors
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

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-logr/logr"
	"github.com/kro-run/kro/api/v1alpha1"
	"github.com/kro-run/kro/pkg/apis"
	"github.com/kro-run/kro/pkg/metadata"
)

// setResourceGraphDefinitionStatus calculates the ResourceGraphDefinition status and updates it
// in the API server.
func (r *ResourceGraphDefinitionReconciler) updateStatus(
	ctx context.Context,
	o *v1alpha1.ResourceGraphDefinition,
	topologicalOrder []string,
	resources []v1alpha1.ResourceInformation,
) error {
	log, _ := logr.FromContext(ctx)
	log.V(1).Info("calculating resource graph definition status and conditions")

	// Set status.state.
	if rgdConditionTypes.For(o).IsRootReady() {
		o.Status.State = v1alpha1.ResourceGraphDefinitionStateActive
	} else {
		o.Status.State = v1alpha1.ResourceGraphDefinitionStateInactive
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy to avoid conflicts
		current := &v1alpha1.ResourceGraphDefinition{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(o), current); err != nil {
			return fmt.Errorf("failed to get current resource graph definition: %w", err)
		}

		// Update status
		dc := current.DeepCopy()
		dc.Status.Conditions = o.Status.Conditions
		dc.Status.State = o.Status.State
		dc.Status.TopologicalOrder = topologicalOrder
		dc.Status.Resources = resources

		log.V(1).Info("updating resource graph definition status",
			"state", dc.Status.State,
			"conditions", len(dc.Status.Conditions),
		)

		// If there's nothing to update, just return.
		if equality.Semantic.DeepEqual(current.Status, o.Status) {
			return nil
		}

		return r.Status().Patch(ctx, dc, client.MergeFrom(current))
	})
}

// setManaged sets the resourcegraphdefinition as managed, by adding the
// default finalizer if it doesn't exist.
func (r *ResourceGraphDefinitionReconciler) setManaged(ctx context.Context, rgd *v1alpha1.ResourceGraphDefinition) error {
	ctrl.LoggerFrom(ctx).V(1).Info("setting resourcegraphdefinition as managed")

	// Skip if finalizer already exists
	if metadata.HasResourceGraphDefinitionFinalizer(rgd) {
		return nil
	}

	dc := rgd.DeepCopy()
	metadata.SetResourceGraphDefinitionFinalizer(dc)
	return r.Patch(ctx, dc, client.MergeFrom(rgd))
}

// setUnmanaged sets the resourcegraphdefinition as unmanaged, by removing the
// default finalizer if it exists.
func (r *ResourceGraphDefinitionReconciler) setUnmanaged(ctx context.Context, rgd *v1alpha1.ResourceGraphDefinition) error {
	ctrl.LoggerFrom(ctx).V(1).Info("setting resourcegraphdefinition as unmanaged")

	// Skip if finalizer already removed
	if !metadata.HasResourceGraphDefinitionFinalizer(rgd) {
		return nil
	}

	dc := rgd.DeepCopy()
	metadata.RemoveResourceGraphDefinitionFinalizer(dc)
	return r.Patch(ctx, dc, client.MergeFrom(rgd))
}

const (
	Ready                 = "Ready"
	ResourceGraphAccepted = "ResourceGraphAccepted"
	KindReady             = "KindReady"
	ControllerReady       = "ControllerReady"
)

var rgdConditionTypes = apis.NewReadyConditions(ResourceGraphAccepted, KindReady, ControllerReady)

// NewConditionsMarkerFor creates a marker to manage conditions and sub-conditions for ResourceGraphDefinitions.
//
// ```
// Ready
//	├─ ResourceGraphAccepted - This controller has accepted the spec.schema and spec.resources.
//	├─ KindReady - The CRD status created on behalf of this RGD.
//	└─ ControllerReady - The status of the controller thread reconciling this resource.
// ```

func NewConditionsMarkerFor(o apis.Object) *ConditionsMarker {
	return &ConditionsMarker{cs: rgdConditionTypes.For(o)}
}

// A ConditionsMarker provides an API to mark conditions onto a ResourceGraphDefinition as the controller does work.
type ConditionsMarker struct {
	cs apis.ConditionSet
}

// ResourceGraphValid signals the rgd.spec.schema and rgd.spec.resources fields have been accepted.
func (m *ConditionsMarker) ResourceGraphValid() {
	m.cs.SetTrueWithReason(ResourceGraphAccepted, "Valid", "resource graph and schema are valid")
}

// ResourceGraphInvalid signals there is something wrong with the rgd.spec.schema or rgd.spec.resources fields.
func (m *ConditionsMarker) ResourceGraphInvalid(msg string) {
	m.cs.SetFalse(ResourceGraphAccepted, "InvalidResourceGraph", msg)
}

// FailedLabelerSetup signals that the controller was unable to start the resource labeler and failed to continue.
func (m *ConditionsMarker) FailedLabelerSetup(msg string) {
	m.cs.SetFalse(ControllerReady, "FailedLabelerSetup", msg)
}

// KindUnready signals the CustomResourceDefinition has either not been synced or has not become ready to use.
func (m *ConditionsMarker) KindUnready(msg string) {
	m.cs.SetFalse(KindReady, "Failed", msg)
}

// TODO: it would be nice to know if the Kind was not accepted at all OR if a CRD exists.

// KindReady signals the CustomResourceDefinition has been synced and is ready.
func (m *ConditionsMarker) KindReady(kind string) {
	m.cs.SetTrueWithReason(KindReady, "Ready", fmt.Sprintf("kind %s has been accepted and ready", kind))
}

// ControllerFailedToStart signals the microcontroller had an issue when starting.
func (m *ConditionsMarker) ControllerFailedToStart(msg string) {
	m.cs.SetFalse(ControllerReady, "FailedToStart", msg)
}

// ControllerRunning signals the microcontroller is up and running for this RGD-Kind.
func (m *ConditionsMarker) ControllerRunning() {
	m.cs.SetTrueWithReason(ControllerReady, "Running", "controller is running")
}
