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
	kindReady := isConditionTrueForObservedGeneration(conditions.Get(KindReady), o.GetGeneration())
	controllerReady := isConditionTrueForObservedGeneration(conditions.Get(ControllerReady), o.GetGeneration())

	// ResourceGraphAccepted indicates whether the current spec compiles,
	// while RGD serving state is about whether traffic can flow through the
	// infra path (CRD established + dynamic controller running). We keep these
	// concerns separate so graph validity and serving availability can evolve
	// independently.
	//
	// An RGD can be Active while only a subset of revisions are valid, and that
	// is expected.
	//
	// This can happen after restarts: older GraphRevisions may be re-evaluated
	// against a different cluster shape (for example, a referenced CRD was
	// deleted or changed).
	if kindReady && controllerReady {
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

func isConditionTrueForObservedGeneration(cond *v1alpha1.Condition, generation int64) bool {
	if cond == nil {
		return false
	}
	return cond.IsTrue() && cond.ObservedGeneration == generation
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
	Ready                   = "Ready"
	ResourceGraphAccepted   = "ResourceGraphAccepted"
	RevisionLineageResolved = "RevisionLineageResolved"
	KindReady               = "KindReady"
	ControllerReady         = "ControllerReady"

	waitingForGraphRevisionSettlementReason  = "WaitingForGraphRevisionSettlement"
	waitingForGraphRevisionWarmupReason      = "WaitingForGraphRevisionWarmup"
	waitingForGraphRevisionCompilationReason = "WaitingForGraphRevisionCompilation"
)

var rgdConditionTypes = apis.NewReadyConditions(KindReady, ControllerReady)

// NewConditionsMarkerFor creates a marker to manage conditions and sub-conditions for ResourceGraphDefinitions.
//
// ```
// Ready
//	├─ KindReady - The CRD status created on behalf of this RGD.
//	└─ ControllerReady - The status of the controller thread reconciling this resource.
//
// ResourceGraphAccepted and RevisionLineageResolved are informational conditions
// and do not participate in Ready calculation. GC health is surfaced via the
// kro_graph_revision_gc_errors_total metric instead of a condition.
// ```

func NewConditionsMarkerFor(o apis.Object) *ConditionsMarker {
	return &ConditionsMarker{cs: rgdConditionTypes.For(o)}
}

// A ConditionsMarker provides an API to mark conditions onto a ResourceGraphDefinition as the controller does work.
type ConditionsMarker struct {
	cs apis.ConditionSet
}

// InitializeServingConditions marks serving conditions as reconciling for a
// new generation unless they have already been observed for this generation.
func (m *ConditionsMarker) InitializeServingConditions(generation int64) {
	if cond := m.cs.Get(KindReady); cond == nil || cond.ObservedGeneration != generation {
		m.cs.SetUnknownWithReason(KindReady, "Reconciling", "waiting for CRD to reconcile")
	}
	if cond := m.cs.Get(ControllerReady); cond == nil || cond.ObservedGeneration != generation {
		m.cs.SetUnknownWithReason(ControllerReady, "Reconciling", "waiting for dynamic controller to reconcile")
	}
}

// ResourceGraphValid signals the rgd.spec.schema and rgd.spec.resources fields have been accepted.
func (m *ConditionsMarker) ResourceGraphValid() {
	m.cs.SetTrueWithReason(ResourceGraphAccepted, "Valid", "resource graph and schema are valid")
}

// ResourceGraphInvalid signals there is something wrong with the rgd.spec.schema or rgd.spec.resources fields.
func (m *ConditionsMarker) ResourceGraphInvalid(msg string) {
	m.cs.SetFalse(ResourceGraphAccepted, "InvalidResourceGraph", msg)
}

// InvalidResourceGraph signals the current spec or latest revision cannot be served.
func (m *ConditionsMarker) InvalidResourceGraph(msg string) {
	m.ResourceGraphInvalid(msg)
	m.RevisionLineageFailed(msg)
	m.ServingUnavailable("InvalidResourceGraph", msg)
}

// RevisionLineageResolved signals the latest GraphRevision lineage is settled
// and the selected revision is ready to serve.
func (m *ConditionsMarker) RevisionLineageResolved(revision int64) {
	m.cs.SetTrueWithReason(
		RevisionLineageResolved,
		"Resolved",
		fmt.Sprintf("graph revision lineage resolved at revision %d", revision),
	)
}

// RevisionLineagePending signals lineage reconciliation is still converging.
func (m *ConditionsMarker) RevisionLineagePending(reason, msg string) {
	m.cs.SetUnknownWithReason(RevisionLineageResolved, reason, msg)
}

// RevisionLineageFailed signals lineage reconciliation could not converge.
func (m *ConditionsMarker) RevisionLineageFailed(msg string) {
	m.cs.SetFalse(RevisionLineageResolved, "ResolutionFailed", msg)
}

// ServingUnknown signals the current generation has not converged to a known
// serving state yet.
func (m *ConditionsMarker) ServingUnknown(reason, msg string) {
	m.cs.SetUnknownWithReason(KindReady, reason, msg)
	m.cs.SetUnknownWithReason(ControllerReady, reason, msg)
}

// ServingPending signals the current generation is not yet serving because
// CRD/controller reconciliation is still in flight.
func (m *ConditionsMarker) ServingPending(reason, msg string) {
	m.ServingUnknown(reason, msg)
}

// WaitingForGraphRevisionSettlement signals deletion/settlement is blocking convergence.
func (m *ConditionsMarker) WaitingForGraphRevisionSettlement() {
	const msg = "waiting for terminating graph revisions to settle"

	m.RevisionLineagePending(waitingForGraphRevisionSettlementReason, msg)
	m.ServingPending(waitingForGraphRevisionSettlementReason, msg)
}

// WaitingForGraphRevisionWarmup signals the latest revision is not yet present in the runtime registry.
func (m *ConditionsMarker) WaitingForGraphRevisionWarmup() {
	const msg = "waiting for the latest graph revision to warm the in-memory registry"

	m.RevisionLineagePending(waitingForGraphRevisionWarmupReason, msg)
	m.ServingPending(waitingForGraphRevisionWarmupReason, msg)
}

// GraphRevisionIssuedPendingCompilation signals a new revision was issued and is awaiting compilation.
func (m *ConditionsMarker) GraphRevisionIssuedPendingCompilation(revision int64) {
	msg := fmt.Sprintf("graph revision %d issued and awaiting compilation", revision)

	m.RevisionLineagePending(waitingForGraphRevisionCompilationReason, msg)
	m.ServingPending(waitingForGraphRevisionCompilationReason, msg)
}

// WaitingForGraphRevisionCompilation signals the latest revision is still compiling.
func (m *ConditionsMarker) WaitingForGraphRevisionCompilation(revision int64) {
	msg := fmt.Sprintf("waiting for graph revision %d to compile", revision)

	m.RevisionLineagePending(waitingForGraphRevisionCompilationReason, msg)
	m.ServingPending(waitingForGraphRevisionCompilationReason, msg)
}

// WaitingForGraphRevisionState signals the latest revision has not yet settled.
func (m *ConditionsMarker) WaitingForGraphRevisionState(revision int64) {
	msg := fmt.Sprintf("waiting for graph revision %d to settle", revision)

	m.RevisionLineagePending(waitingForGraphRevisionCompilationReason, msg)
	m.ServingPending(waitingForGraphRevisionCompilationReason, msg)
}

// ServingUnavailable signals the current generation cannot be served.
func (m *ConditionsMarker) ServingUnavailable(reason, msg string) {
	m.cs.SetFalse(KindReady, reason, msg)
	m.cs.SetFalse(ControllerReady, reason, msg)
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
