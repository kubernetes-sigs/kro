package graphcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// conditionType identifies a condition on a Graph or GraphRevision.
type conditionType string

// Condition types
const (
	conditionCompiled conditionType = "Compiled"
	conditionReady    conditionType = "Ready"
)

// conditionStatus is the boolean-ish status of a condition (True/False/Unknown).
type conditionStatus string

// Condition statuses
const (
	conditionTrue    conditionStatus = "True"
	conditionFalse   conditionStatus = "False"
	conditionUnknown conditionStatus = "Unknown"
)

// reconcileState captures the outcome of a reconcile cycle for status derivation.
type reconcileState struct {
	// compiled tracks spec validity. Set once when the spec is parsed/compiled.
	// False is terminal — no resources are touched until the spec is fixed.
	compiled    bool
	compiledErr error // non-nil when compiled=false

	planSummary dagpkg.PlanSummary
	// nodeErrors carries error messages ("nodeID: reason") surfaced when any
	// of the HasX flags fire. These are the reason text for NotReady/Error/
	// Blocked conditions.
	nodeErrors []string
	// nodeNotes carries informational messages that don't gate Ready — e.g.,
	// FinalizerSkipped emitted during prune when the target resource was
	// already absent. Notes appear in the Ready condition message when the
	// graph is otherwise healthy, so operators see the event without being
	// misled by an Unknown/False status. Per 005-reconciliation.md §
	// Finalization: "FinalizerSkipped is not an error — finalization was
	// bypassed because there was nothing to finalize."
	nodeNotes []string
}

// deriveCompiledCondition computes the Compiled condition.
// Compiled is set once when the spec is parsed/compiled. It's permanent until
// the spec changes. False means the Graph will never converge until the spec is fixed.
func (s *reconcileState) deriveCompiledCondition() (status conditionStatus, reason string, message string) {
	if s.compiled {
		return conditionTrue, "Compiled", "Spec is valid"
	}
	if s.compiledErr != nil {
		// Classify the error
		if errors.Is(s.compiledErr, compiler.ErrInvalidExpression) {
			return conditionFalse, "ExpressionError", s.compiledErr.Error()
		}
		if errors.Is(s.compiledErr, compiler.ErrDependencyError) {
			return conditionFalse, "DependencyError", s.compiledErr.Error()
		}
		return conditionFalse, "DeclarationError", s.compiledErr.Error()
	}
	return conditionFalse, "DeclarationError", "Spec validation failed"
}

// deriveReadyCondition computes the Ready condition from the reconcile outcome.
//
// Ready is a rollup of node plan states. Each reason maps to the node state
// blocking convergence. Precedence: SystemError > Error > Conflict > Blocked >
// Pending > NotReady. SystemError surfaces first because it signals degraded
// reconciliation infrastructure — deterministic errors (Error) and conflicts
// may be artifacts of system instability, not real spec problems. Once the
// system recovers, the durable errors will still be there. Surfacing Error
// first would send operators to debug their spec while the real problem is
// infrastructure.
//
//	Ready       → True    — all resources reconciled
//	Pending     → Unknown — waiting for upstream data
//	NotReady    → Unknown — applied but readyWhen conditions not met
//	Blocked     → Unknown — dependency in error state, waiting for resolve
//	NotCompiled → False   — spec invalid; rollup of Compiled=False
//	SystemError → False   — server or infrastructure failure (5xx)
//	Error       → False   — client request failed (4xx)
//	Conflict    → False   — SSA field ownership contested
func (s *reconcileState) deriveReadyCondition() (status conditionStatus, reason string, message string) {
	if !s.compiled {
		return conditionFalse, "NotCompiled", "Spec is not valid; resources cannot be reconciled"
	}
	if s.planSummary.HasSystemError {
		return conditionFalse, "SystemError",
			fmt.Sprintf("Resources with server/infrastructure errors: %s",
				strings.Join(s.nodeErrors, "; "))
	}
	if s.planSummary.HasError {
		return conditionFalse, "Error",
			fmt.Sprintf("Resources with errors: %s",
				strings.Join(s.nodeErrors, "; "))
	}
	if s.planSummary.HasConflict {
		msg := "One or more resources have SSA field ownership conflicts"
		if len(s.nodeErrors) > 0 {
			msg += ": " + strings.Join(s.nodeErrors, "; ")
		}
		return conditionFalse, "Conflict", msg
	}
	if s.planSummary.HasBlocked {
		msg := "One or more resources blocked by upstream errors"
		// Surface TeardownBlocked reasons (third-party field managers,
		// finalizer creation failure, finalizer not ready) so operators can
		// pick the right remediation. Per 005-reconciliation.md §
		// Finalization, the three causes need different responses — collapsing
		// them into one message is observability without actionability.
		if len(s.nodeErrors) > 0 {
			msg += " (" + strings.Join(s.nodeErrors, "; ") + ")"
		}
		return conditionUnknown, "Blocked", msg
	}
	if s.planSummary.HasPending {
		msg := "One or more resources waiting for upstream data"
		if len(s.nodeErrors) > 0 {
			msg += " (" + strings.Join(s.nodeErrors, "; ") + ")"
		}
		return conditionUnknown, "Pending", msg
	}
	if s.planSummary.HasNotReady {
		msg := "One or more resources have not satisfied their readyWhen conditions"
		// Surface readyWhen expression errors so operators can distinguish
		// "waiting for Deployment to roll out" from "your CEL expression
		// returns int64, expected bool." Without this, broken readyWhen
		// expressions appear as transient NotReady with no actionable signal.
		if len(s.nodeErrors) > 0 {
			msg += " (" + strings.Join(s.nodeErrors, "; ") + ")"
		}
		return conditionUnknown, "NotReady", msg
	}
	msg := fmt.Sprintf("All %d resources reconciled", s.planSummary.ReadyCount)
	// Surface informational notes (e.g., FinalizerSkipped) that don't
	// constitute errors but are operationally useful. Per 005-reconciliation.md
	// § Finalization: "The Graph's status surfaces this: FinalizerSkipped
	// with a message naming the resource."
	//
	// Notes are kept separate from errors (reconcileState.nodeErrors) so that
	// a healthy graph with an informational note doesn't have its message
	// parenthesized with error text — the parens are reserved for actionable
	// problems. Callers route errors to nodeErrors and info to nodeNotes.
	if len(s.nodeNotes) > 0 {
		msg += " (" + strings.Join(s.nodeNotes, "; ") + ")"
	}
	return conditionTrue, "Ready", msg
}

// updateStatus writes the Graph's status subresource. Reads the latest version
// from the API server to avoid conflicts.
func (r *GraphReconciler) updateStatus(ctx context.Context, graph *unstructured.Unstructured, state *reconcileState) error {
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(GraphGVK)
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(graph), latest); err != nil {
		return fmt.Errorf("reading latest for status update: %w", err)
	}

	// Build both conditions
	compiledStatus, compiledReason, compiledMessage := state.deriveCompiledCondition()
	compiledCondition := buildCondition(string(conditionCompiled), compiledStatus, compiledReason, compiledMessage)

	readyStatus, readyReason, readyMessage := state.deriveReadyCondition()
	readyCondition := buildCondition(string(conditionReady), readyStatus, readyReason, readyMessage)

	// Preserve lastTransitionTime for conditions whose status hasn't changed
	existingConditions, _, _ := unstructured.NestedSlice(latest.Object, "status", "conditions")
	preserveTransitionTime(existingConditions, compiledCondition, compiledStatus)
	preserveTransitionTime(existingConditions, readyCondition, readyStatus)

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

// buildCondition creates a condition map with the current timestamp.
func buildCondition(condType string, status conditionStatus, reason, message string) map[string]any {
	return map[string]any{
		"type":               condType,
		"status":             string(status),
		"reason":             reason,
		"message":            message,
		"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
	}
}

// preserveTransitionTime scans existing conditions for a match on type+status
// and preserves the lastTransitionTime if the status hasn't changed.
func preserveTransitionTime(existing []any, cond map[string]any, status conditionStatus) {
	for _, ec := range existing {
		ecMap, ok := ec.(map[string]any)
		if !ok {
			continue
		}
		if ecMap["type"] == cond["type"] && ecMap["status"] == string(status) {
			if ltt, ok := ecMap["lastTransitionTime"].(string); ok {
				cond["lastTransitionTime"] = ltt
			}
			return
		}
	}
}
