package graphcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Graph states
const (
	StateActive     = "Active"
	StateInProgress = "InProgress"
	StateError      = "Error"
	StateDeleting   = "Deleting"
)

// Condition types
const (
	ConditionAccepted = "Accepted"
	ConditionReady    = "Ready"
)

// Condition statuses
const (
	ConditionTrue    = "True"
	ConditionFalse   = "False"
	ConditionUnknown = "Unknown"
)

// reconcileState captures the outcome of a reconcile cycle for status derivation.
type reconcileState struct {
	// Accepted tracks spec validity. Set once when the spec is parsed/compiled.
	// False is terminal — no resources are touched until the spec is fixed.
	accepted    bool
	acceptedErr error // non-nil when accepted=false

	needsRequeue   bool
	hasDataPending bool
	hasNotReady    bool
	hasConflict    bool
	reconcileErr   error
	resourceCount  int
	appliedCount   int
	contributions  []string // resource IDs detected as contributions
}

// deriveState computes the Graph state from the reconcile outcome.
func (s *reconcileState) deriveState() string {
	if !s.accepted || s.reconcileErr != nil {
		return StateError
	}
	if s.needsRequeue || s.hasDataPending || s.hasNotReady {
		return StateInProgress
	}
	return StateActive
}

// deriveAcceptedCondition computes the Accepted condition.
// Accepted is set once when the spec is parsed/compiled. It's permanent until
// the spec changes. False means the Graph will never converge until the spec is fixed.
func (s *reconcileState) deriveAcceptedCondition() (status string, reason string, message string) {
	if s.accepted {
		return ConditionTrue, "Accepted", "Spec is valid"
	}
	if s.acceptedErr != nil {
		// Classify the error
		if errors.Is(s.acceptedErr, ErrCompilationFailed) {
			return ConditionFalse, "CompilationFailed", s.acceptedErr.Error()
		}
		if errors.Is(s.acceptedErr, ErrCycleDetected) {
			return ConditionFalse, "CycleDetected", s.acceptedErr.Error()
		}
		return ConditionFalse, "InvalidSpec", s.acceptedErr.Error()
	}
	return ConditionFalse, "InvalidSpec", "Spec validation failed"
}

// deriveReadyCondition computes the Ready condition from the reconcile outcome.
// When the spec is not accepted, Ready is False — resources can't converge
// until the spec is fixed.
func (s *reconcileState) deriveReadyCondition() (status string, reason string, message string) {
	if !s.accepted {
		return ConditionFalse, "NotAccepted", "Spec is not valid; resources cannot be reconciled"
	}
	if s.reconcileErr != nil {
		return ConditionFalse, "ReconcileError", s.reconcileErr.Error()
	}
	if s.hasConflict {
		return ConditionFalse, "FieldConflict", "One or more resources have SSA field ownership conflicts"
	}
	if s.hasDataPending {
		return ConditionFalse, "DataPending", "One or more resources have unresolvable expressions; waiting for data"
	}
	if s.hasNotReady {
		return ConditionFalse, "ResourcesNotReady", "One or more resources have not satisfied their readyWhen conditions"
	}
	if s.needsRequeue {
		return ConditionFalse, "Progressing", "Reconciliation in progress"
	}
	return ConditionTrue, "Ready", fmt.Sprintf("All %d resources reconciled", s.appliedCount)
}

// updateStatus writes the Graph's status subresource. Reads the latest version
// from the API server to avoid conflicts.
func (r *GraphReconciler) updateStatus(ctx context.Context, graph *unstructured.Unstructured, state *reconcileState) error {
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(GraphGVK)
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(graph), latest); err != nil {
		return fmt.Errorf("reading latest for status update: %w", err)
	}
	return r.updateStatusOnLatest(ctx, latest, state)
}

// updateStatusOnLatest writes status using a pre-fetched latest object.
// This avoids a redundant GET when the caller already has a fresh copy
// (e.g., from pruneAndUpdateStatus).
func (r *GraphReconciler) updateStatusOnLatest(ctx context.Context, latest *unstructured.Unstructured, state *reconcileState) error {
	derivedState := state.deriveState()

	now := time.Now().UTC().Format(time.RFC3339)

	// Build both conditions
	acceptedStatus, acceptedReason, acceptedMessage := state.deriveAcceptedCondition()
	acceptedCondition := map[string]any{
		"type":               ConditionAccepted,
		"status":             acceptedStatus,
		"reason":             acceptedReason,
		"message":            acceptedMessage,
		"lastTransitionTime": now,
	}

	readyStatus, readyReason, readyMessage := state.deriveReadyCondition()
	readyCondition := map[string]any{
		"type":               ConditionReady,
		"status":             readyStatus,
		"reason":             readyReason,
		"message":            readyMessage,
		"lastTransitionTime": now,
	}

	// Preserve lastTransitionTime for conditions whose status hasn't changed
	existingConditions, _, _ := unstructured.NestedSlice(latest.Object, "status", "conditions")
	for _, ec := range existingConditions {
		ecMap, ok := ec.(map[string]any)
		if !ok {
			continue
		}
		switch ecMap["type"] {
		case ConditionAccepted:
			if ecMap["status"] == acceptedStatus {
				if ltt, ok := ecMap["lastTransitionTime"].(string); ok {
					acceptedCondition["lastTransitionTime"] = ltt
				}
			}
		case ConditionReady:
			if ecMap["status"] == readyStatus {
				if ltt, ok := ecMap["lastTransitionTime"].(string); ok {
					readyCondition["lastTransitionTime"] = ltt
				}
			}
		}
	}

	// Start with controller-managed status fields
	status := map[string]any{
		"state": derivedState,
		"conditions": []any{
			acceptedCondition,
			readyCondition,
		},
	}

	// Surface detected contributions in status (design: "The Graph's status
	// surfaces which resources were detected as contributions, making the
	// inference observable.")
	if len(state.contributions) > 0 {
		contribs := make([]any, len(state.contributions))
		for i, id := range state.contributions {
			contribs[i] = id
		}
		status["contributions"] = contribs
	}

	// Skip the status write if nothing changed. Compare via JSON to avoid
	// reflect.DeepEqual issues with map[string]any types. This prevents a
	// spurious resourceVersion bump and the resulting watch → reconcile loop.
	existingStatus, _, _ := unstructured.NestedMap(latest.Object, "status")
	if statusEqual(existingStatus, status) {
		return nil
	}

	latest.Object["status"] = status

	if err := r.Client.Status().Update(ctx, latest); err != nil {
		return fmt.Errorf("updating status: %w", err)
	}

	return nil
}

// statusEqual compares two status maps via JSON serialization.
// Returns true if they're semantically identical.
func statusEqual(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	aj, err1 := json.Marshal(a)
	bj, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(aj) == string(bj)
}
