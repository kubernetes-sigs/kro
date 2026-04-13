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

// ConditionType identifies a condition on a Graph or GraphRevision.
type ConditionType string

// Condition types
const (
	ConditionCompiled ConditionType = "Compiled"
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
	// compiled tracks spec validity. Set once when the spec is parsed/compiled.
	// False is terminal — no resources are touched until the spec is fixed.
	compiled    bool
	compiledErr error // non-nil when compiled=false

	PlanSummary
	nodeErrors []string // "nodeID: reason" for status message
	nodeCount  int
}

// deriveCompiledCondition computes the Compiled condition.
// Compiled is set once when the spec is parsed/compiled. It's permanent until
// the spec changes. False means the Graph will never converge until the spec is fixed.
func (s *reconcileState) deriveCompiledCondition() (status ConditionStatus, reason string, message string) {
	if s.compiled {
		return ConditionTrue, "Compiled", "Spec is valid"
	}
	if s.compiledErr != nil {
		// Classify the error
		if errors.Is(s.compiledErr, ErrInvalidExpression) {
			return ConditionFalse, "ExpressionError", s.compiledErr.Error()
		}
		if errors.Is(s.compiledErr, ErrDependencyError) {
			return ConditionFalse, "DependencyError", s.compiledErr.Error()
		}
		return ConditionFalse, "DeclarationError", s.compiledErr.Error()
	}
	return ConditionFalse, "DeclarationError", "Spec validation failed"
}

// deriveReadyCondition computes the Ready condition from the reconcile outcome.
//
// Ready is a rollup of node plan states. Each reason maps to the node state
// blocking convergence:
//
//	Ready       → True    — all resources reconciled
//	Pending     → Unknown — waiting for upstream data
//	NotReady    → Unknown — applied but readyWhen conditions not met
//	NotCompiled → False   — spec invalid; rollup of Compiled=False
//	Error       → False   — client request failed (4xx)
//	SystemError → False   — server or infrastructure failure (5xx)
//	Conflict    → False   — SSA field ownership contested
func (s *reconcileState) deriveReadyCondition() (status ConditionStatus, reason string, message string) {
	if !s.compiled {
		return ConditionFalse, "NotCompiled", "Spec is not valid; resources cannot be reconciled"
	}
	if s.HasSystemError {
		return ConditionFalse, "SystemError",
			fmt.Sprintf("Resources with server/infrastructure errors: %s",
				strings.Join(s.nodeErrors, "; "))
	}
	if s.HasError {
		return ConditionFalse, "Error",
			fmt.Sprintf("Resources with errors: %s",
				strings.Join(s.nodeErrors, "; "))
	}
	if s.HasConflict {
		return ConditionFalse, "Conflict", "One or more resources have SSA field ownership conflicts"
	}
	if s.HasBlocked {
		return ConditionUnknown, "Blocked", "One or more resources blocked by upstream errors"
	}
	if s.HasPending {
		return ConditionUnknown, "Pending", "One or more resources waiting for upstream data"
	}
	if s.HasNotReady {
		return ConditionUnknown, "NotReady", "One or more resources have not satisfied their readyWhen conditions"
	}
	msg := fmt.Sprintf("All %d resources reconciled", s.ReadyCount)
	// Surface informational notes (e.g., FinalizerSkipped) that don't
	// constitute errors but are operationally useful. Per 004-graph-execution.md
	// § Finalization: "The Graph's status surfaces this: FinalizerSkipped
	// with a message naming the resource."
	if len(s.nodeErrors) > 0 {
		msg += " (" + strings.Join(s.nodeErrors, "; ") + ")"
	}
	return ConditionTrue, "Ready", msg
}

// updateStatus writes the Graph's status subresource. Reads the latest version
// from the API server to avoid conflicts.
func (r *GraphReconciler) updateStatus(ctx context.Context, graph *unstructured.Unstructured, state *reconcileState) error {
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(GraphGVK)
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(graph), latest); err != nil {
		return fmt.Errorf("reading latest for status update: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Build both conditions
	compiledStatus, compiledReason, compiledMessage := state.deriveCompiledCondition()
	compiledCondition := map[string]any{
		"type":               string(ConditionCompiled),
		"status":             string(compiledStatus),
		"reason":             compiledReason,
		"message":            compiledMessage,
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
		case string(ConditionCompiled):
			if ecMap["status"] == string(compiledStatus) {
				if ltt, ok := ecMap["lastTransitionTime"].(string); ok {
					compiledCondition["lastTransitionTime"] = ltt
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

	status := map[string]any{
		"conditions": []any{
			compiledCondition,
			readyCondition,
		},
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
