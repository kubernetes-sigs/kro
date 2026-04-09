package graphcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GraphState describes the high-level state of a Graph object.
type GraphState string

// Graph states
const (
	StateActive     GraphState = "Active"
	StateInProgress GraphState = "InProgress"
	StateError      GraphState = "Error"
)

// ConditionType identifies a condition on a Graph or GraphRevision.
type ConditionType string

// Condition types
const (
	ConditionAccepted ConditionType = "Accepted"
	ConditionReady    ConditionType = "Ready"
)

// ConditionStatus is the boolean-ish status of a condition (True/False/Unknown).
type ConditionStatus string

// Condition statuses
const (
	ConditionTrue    ConditionStatus = "True"
	ConditionFalse   ConditionStatus = "False"
	ConditionUnknown ConditionStatus = "Unknown"
)

// reconcileState captures the outcome of a reconcile cycle for status derivation.
type reconcileState struct {
	// Accepted tracks spec validity. Set once when the spec is parsed/compiled.
	// False is terminal — no resources are touched until the spec is fixed.
	accepted    bool
	acceptedErr error // non-nil when accepted=false

	hasDataPending bool
	hasNotReady    bool
	hasConflict    bool
	hasError       bool     // any node in NodeError (4xx)
	hasSystemError bool     // any node in NodeSystemError (5xx)
	nodeErrors     []string // "nodeID: reason" for status message
	pruneErr       error    // non-nil if resource pruning failed
	nodeCount      int
	appliedCount   int
	contributions  []string // node IDs detected as contributions
}

// deriveState computes the Graph state from the reconcile outcome.
func (s *reconcileState) deriveState() GraphState {
	if !s.accepted {
		return StateError
	}
	if s.hasDataPending || s.hasNotReady || s.hasConflict || s.hasError || s.hasSystemError {
		return StateInProgress
	}
	return StateActive
}

// deriveAcceptedCondition computes the Accepted condition.
// Accepted is set once when the spec is parsed/compiled. It's permanent until
// the spec changes. False means the Graph will never converge until the spec is fixed.
func (s *reconcileState) deriveAcceptedCondition() (status ConditionStatus, reason string, message string) {
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
func (s *reconcileState) deriveReadyCondition() (status ConditionStatus, reason string, message string) {
	if !s.accepted {
		return ConditionFalse, "NotAccepted", "Spec is not valid; resources cannot be reconciled"
	}
	if s.hasSystemError {
		return ConditionFalse, "SystemError",
			fmt.Sprintf("Nodes with server/infrastructure errors (retrying): %s",
				strings.Join(s.nodeErrors, "; "))
	}
	if s.hasError {
		return ConditionFalse, "NodeError",
			fmt.Sprintf("Nodes with errors (retrying): %s",
				strings.Join(s.nodeErrors, "; "))
	}
	if s.pruneErr != nil {
		return ConditionFalse, "PruneError", fmt.Sprintf("Failed to prune removed resources: %v", s.pruneErr)
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
	return ConditionTrue, "Ready", fmt.Sprintf("All %d nodes reconciled", s.appliedCount)
}

// updateStatus writes the Graph's status subresource. Reads the latest version
// from the API server to avoid conflicts.
func (r *GraphReconciler) updateStatus(ctx context.Context, graph *unstructured.Unstructured, state *reconcileState) error {
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(GraphGVK)
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(graph), latest); err != nil {
		return fmt.Errorf("reading latest for status update: %w", err)
	}

	derivedState := state.deriveState()

	now := time.Now().UTC().Format(time.RFC3339)

	// Build both conditions
	acceptedStatus, acceptedReason, acceptedMessage := state.deriveAcceptedCondition()
	acceptedCondition := map[string]any{
		"type":               string(ConditionAccepted),
		"status":             string(acceptedStatus),
		"reason":             acceptedReason,
		"message":            acceptedMessage,
		"lastTransitionTime": now,
	}

	readyStatus, readyReason, readyMessage := state.deriveReadyCondition()
	readyCondition := map[string]any{
		"type":               string(ConditionReady),
		"status":             string(readyStatus),
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
		case string(ConditionAccepted):
			if ecMap["status"] == string(acceptedStatus) {
				if ltt, ok := ecMap["lastTransitionTime"].(string); ok {
					acceptedCondition["lastTransitionTime"] = ltt
				}
			}
		case string(ConditionReady):
			if ecMap["status"] == string(readyStatus) {
				if ltt, ok := ecMap["lastTransitionTime"].(string); ok {
					readyCondition["lastTransitionTime"] = ltt
				}
			}
		}
	}

	// Start with controller-managed status fields
	status := map[string]any{
		"state": string(derivedState),
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
