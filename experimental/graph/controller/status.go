package graphcontroller

import (
	"context"
	"encoding/json"
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
	ConditionReady = "Ready"
)

// Condition statuses
const (
	ConditionTrue    = "True"
	ConditionFalse   = "False"
	ConditionUnknown = "Unknown"
)

// reconcileState captures the outcome of a reconcile cycle for status derivation.
type reconcileState struct {
	needsRequeue   bool
	hasDataPending bool
	hasNotReady    bool
	hasConflict    bool
	reconcileErr   error
	resourceCount  int
	appliedCount   int
}

// deriveState computes the Graph state from the reconcile outcome.
func (s *reconcileState) deriveState() string {
	if s.reconcileErr != nil {
		return StateError
	}
	if s.needsRequeue || s.hasDataPending || s.hasNotReady {
		return StateInProgress
	}
	return StateActive
}

// deriveReadyCondition computes the Ready condition from the reconcile outcome.
func (s *reconcileState) deriveReadyCondition() (status string, reason string, message string) {
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
// from the API server to avoid conflicts. Used for error paths early in the
// reconcile loop (before pruneAndUpdateStatus takes over).
func (r *GraphReconciler) updateStatus(ctx context.Context, graph *unstructured.Unstructured, state *reconcileState, statusTemplate map[string]any, eval *evaluator) error {
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(GraphGVK)
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(graph), latest); err != nil {
		return fmt.Errorf("reading latest for status update: %w", err)
	}
	return r.updateStatusOnLatest(ctx, latest, state, statusTemplate, eval)
}

// updateStatusOnLatest writes status using a pre-fetched latest object.
// This avoids a redundant GET when the caller already has a fresh copy
// (e.g., from pruneAndUpdateStatus).
func (r *GraphReconciler) updateStatusOnLatest(ctx context.Context, latest *unstructured.Unstructured, state *reconcileState, statusTemplate map[string]any, eval *evaluator) error {
	derivedState := state.deriveState()
	readyStatus, readyReason, readyMessage := state.deriveReadyCondition()

	now := time.Now().UTC().Format(time.RFC3339)

	// Build the condition
	readyCondition := map[string]any{
		"type":               ConditionReady,
		"status":             readyStatus,
		"reason":             readyReason,
		"message":            readyMessage,
		"lastTransitionTime": now,
	}

	// Check if the condition already exists and preserve lastTransitionTime if status unchanged
	existingConditions, _, _ := unstructured.NestedSlice(latest.Object, "status", "conditions")
	for _, ec := range existingConditions {
		ecMap, ok := ec.(map[string]any)
		if !ok {
			continue
		}
		if ecMap["type"] == ConditionReady {
			if ecMap["status"] == readyStatus {
				if ltt, ok := ecMap["lastTransitionTime"].(string); ok {
					readyCondition["lastTransitionTime"] = ltt
				}
			}
			break
		}
	}

	// Start with controller-managed status fields
	status := map[string]any{
		"state": derivedState,
		"conditions": []any{
			readyCondition,
		},
	}

	// Merge user-defined status fields (soft resolve — omit fields that fail)
	if len(statusTemplate) > 0 && eval != nil {
		userStatus := eval.statusTemplate(statusTemplate)
		for k, v := range userStatus {
			// Don't let user fields overwrite controller-managed fields
			if k == "state" || k == "conditions" {
				continue
			}
			status[k] = v
		}
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
