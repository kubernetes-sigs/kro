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

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-logr/logr"
	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/apis"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
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

	oldState := o.Status.State

	conditions := rgdConditionTypes.For(o)
	// State reflects accepted serving availability rather than full graph revision convergence.
	// An RGD can remain Active while GraphRevisionsResolved is Unknown if the accepted graph is
	// still being served by the existing CRD and dynamic controller.
	if conditions.IsTrue(GraphAccepted, KindReady, ControllerReady) {
		o.Status.State = v1alpha1.ResourceGraphDefinitionStateActive
	} else {
		o.Status.State = v1alpha1.ResourceGraphDefinitionStateInactive
	}

	if oldState != o.Status.State && oldState != "" {
		stateTransitionsTotal.WithLabelValues(o.Name, string(oldState), string(o.Status.State)).Inc()
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
		dc.Status.LastIssuedRevision = o.Status.LastIssuedRevision

		log.V(1).Info("updating resource graph definition status",
			"state", dc.Status.State,
			"conditions", len(dc.Status.Conditions),
		)

		// If there's nothing to update, just return.
		if equality.Semantic.DeepEqual(current.Status, dc.Status) {
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
	Ready                  = "Ready"
	GraphAccepted          = "GraphAccepted"
	GraphRevisionsResolved = "GraphRevisionsResolved"
	KindReady              = "KindReady"
	ControllerReady        = "ControllerReady"

	waitingForGraphRevisionSettlementReason  = "WaitingForGraphRevisionSettlement"
	waitingForGraphRevisionWarmupReason      = "WaitingForGraphRevisionWarmup"
	waitingForGraphRevisionCompilationReason = "WaitingForGraphRevisionCompilation"
)

var rgdConditionTypes = apis.NewReadyConditions(GraphRevisionsResolved, GraphAccepted, KindReady, ControllerReady)

// NewConditionsMarkerFor creates a marker to manage conditions for ResourceGraphDefinitions.
//
// Ready
// ├── GraphRevisionsResolved  — graph revisions settled and latest compiled
// ├── GraphAccepted   — spec valid and revision created
// ├── KindReady       — CRD established
// └── ControllerReady — dynamic controller registered
//
// All four are dependents of Ready.

func NewConditionsMarkerFor(o apis.Object) *ConditionsMarker {
	return &ConditionsMarker{cs: rgdConditionTypes.For(o)}
}

// ConditionsMarker sets conditions on an RGD. Each method touches only its own condition.
type ConditionsMarker struct {
	cs apis.ConditionSet
}

// --- GraphRevisionsResolved ---

// GraphRevisionsResolved signals graph revisions are settled and the latest revision is compiled and active.
func (m *ConditionsMarker) GraphRevisionsResolved(revision int64) {
	m.cs.SetTrueWithReason(GraphRevisionsResolved, "Resolved", fmt.Sprintf("revision %d compiled and active", revision))
}

// GraphRevisionsResolving signals graph revisions are still converging.
func (m *ConditionsMarker) GraphRevisionsResolving(reason, msg string) {
	m.cs.SetUnknownWithReason(GraphRevisionsResolved, reason, msg)
}

// GraphRevisionsSettling signals terminating graph revisions are still being removed.
func (m *ConditionsMarker) GraphRevisionsSettling() {
	m.GraphRevisionsResolving(
		waitingForGraphRevisionSettlementReason,
		"waiting for terminating graph revisions to settle",
	)
}

// GraphRevisionsWarmingUp signals the latest graph revision has not reached the in-memory registry yet.
func (m *ConditionsMarker) GraphRevisionsWarmingUp() {
	m.GraphRevisionsResolving(
		waitingForGraphRevisionWarmupReason,
		"waiting for the latest graph revision to warm the in-memory registry",
	)
}

// GraphRevisionsCompiling signals a newly issued revision is still compiling.
func (m *ConditionsMarker) GraphRevisionsCompiling(revision int64) {
	m.GraphRevisionsResolving(
		waitingForGraphRevisionCompilationReason,
		fmt.Sprintf("graph revision %d issued and awaiting compilation", revision),
	)
}

// GraphRevisionsAwaitingCompilation signals an existing latest revision is still compiling.
func (m *ConditionsMarker) GraphRevisionsAwaitingCompilation(revision int64) {
	m.GraphRevisionsResolving(
		waitingForGraphRevisionCompilationReason,
		fmt.Sprintf("waiting for graph revision %d to compile", revision),
	)
}

// GraphRevisionsAwaitingSettlement signals an existing latest revision is still settling.
func (m *ConditionsMarker) GraphRevisionsAwaitingSettlement(revision int64) {
	m.GraphRevisionsResolving(
		waitingForGraphRevisionSettlementReason,
		fmt.Sprintf("waiting for graph revision %d to settle", revision),
	)
}

// GraphRevisionsUnresolved signals graph revisions could not be resolved.
func (m *ConditionsMarker) GraphRevisionsUnresolved(msg string) {
	m.cs.SetFalse(GraphRevisionsResolved, "Failed", msg)
}

// --- GraphAccepted ---

// ResourceGraphValid signals the rgd.spec.schema and rgd.spec.resources fields have been accepted.
func (m *ConditionsMarker) ResourceGraphValid() {
	m.cs.SetTrueWithReason(GraphAccepted, "Valid", "resource graph and schema are valid")
}

// ResourceGraphInvalid signals there is something wrong with the rgd.spec.schema or rgd.spec.resources fields.
func (m *ConditionsMarker) ResourceGraphInvalid(msg string) {
	m.cs.SetFalse(GraphAccepted, "InvalidResourceGraph", msg)
}

// --- KindReady ---

// KindReady signals the CustomResourceDefinition has been synced and is ready.
func (m *ConditionsMarker) KindReady(kind string) {
	m.cs.SetTrueWithReason(KindReady, "Ready", fmt.Sprintf("kind %s has been accepted and ready", kind))
}

// KindUnready signals the CustomResourceDefinition has either not been synced or has not become ready to use.
func (m *ConditionsMarker) KindUnready(msg string) {
	m.cs.SetFalse(KindReady, "Failed", msg)
}

// --- ControllerReady ---

// ControllerRunning signals the microcontroller is up and running for this RGD-Kind.
func (m *ConditionsMarker) ControllerRunning() {
	m.cs.SetTrueWithReason(ControllerReady, "Running", "controller is running")
}

// ControllerFailedToStart signals the microcontroller had an issue when starting.
func (m *ConditionsMarker) ControllerFailedToStart(msg string) {
	m.cs.SetFalse(ControllerReady, "FailedToStart", msg)
}

// FailedLabelerSetup signals that the controller was unable to start the resource labeler and failed to continue.
func (m *ConditionsMarker) FailedLabelerSetup(msg string) {
	m.cs.SetFalse(ControllerReady, "FailedLabelerSetup", msg)
}
