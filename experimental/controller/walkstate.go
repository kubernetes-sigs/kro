// walkstate.go defines per-reconcile node and plan state for DAG traversal.
//
// NodeState tracks the reconcile-time outcome for each node. PlanState
// aggregates node states across a single reconcile cycle. These are
// created fresh per reconcile and do not persist across cycles — the
// controller's instanceState retains what it needs (previousPlanStates)
// for cross-cycle comparison.
//
// Per 005-reconciliation.md § Node States.
package graphcontroller

import (
	"fmt"

	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
)

// NodeState tracks the reconcile-time state of a single node.
type NodeState int

const (
	// NodeUnvisited is the zero-value sentinel — "not yet processed by the
	// propagation." The dispatch guard (tryDispatch) uses != NodeUnvisited to detect nodes
	// that have already been assigned a state by the propagation or by inheritance.
	NodeUnvisited NodeState = iota

	NodePending     // Data not yet available (retryable)
	NodeReady       // Applied and readyWhen satisfied
	NodeNotReady    // Applied but readyWhen not satisfied
	NodeExcluded    // Definitive absence: excluded by includeWhen evaluating to false
	NodeBlocked     // Uncertain absence: dependency in error state
	NodeError       // Client request failed (4xx)
	NodeConflict    // SSA 409 — field ownership taken by another actor
	NodeSystemError // Server/infrastructure failure (5xx, timeout, network)

	// _nodeStateCount is a sentinel for compile-time assertions.
	// It must remain the last constant in this block. Adding a new
	// NodeState without updating nodeStateLabels in metrics.go will
	// break the build.
	_nodeStateCount
)

// String returns the human-readable name of the NodeState.
func (s NodeState) String() string {
	switch s {
	case NodeUnvisited:
		return "Unvisited"
	case NodePending:
		return "Pending"
	case NodeReady:
		return "Ready"
	case NodeNotReady:
		return "NotReady"
	case NodeExcluded:
		return "Excluded"
	case NodeBlocked:
		return "Blocked"
	case NodeError:
		return "Error"
	case NodeConflict:
		return "Conflict"
	case NodeSystemError:
		return "SystemError"
	default:
		return fmt.Sprintf("NodeState(%d)", int(s))
	}
}

// PlanState tracks the state of all nodes during a reconcile cycle.
type PlanState struct {
	States map[string]NodeState
}

// NewPlanState creates a fresh plan state with all nodes unvisited.
func NewPlanState(dag *dagpkg.DAG) *PlanState {
	ps := &PlanState{
		States: make(map[string]NodeState, len(dag.Nodes)),
	}
	for _, node := range dag.Nodes {
		ps.States[node.ID] = NodeUnvisited
	}
	return ps
}

// FinalizeSkippedStates resolves node states for nodes that took the
// outputsReady skip path and never re-dispatched via a propagation trigger.
// Their plan.States stay at NodeUnvisited through the walk; status derivation
// needs a real state. Restores from previousPlanStates when available, falls
// back to NodePending when the invariant "skipped nodes have prior state" is
// violated.
//
// Fallthrough case: outputsReady without a previousPlanStates entry is
// structurally impossible today — the skip paths that set outputsReady only
// fire when the node has prior state — but defending explicitly makes the
// invariant checkable. A silent Ready (zero-value NodeUnvisited slipping
// through PlanSummary, which ignores it) would under-report node count and
// mask latent bugs. Treat "skipped with no prior state" as Pending.
func FinalizeSkippedStates(plan *PlanState, outputsReady map[string]bool, previousPlanStates map[string]NodeState, unrecognizedSkip func(nodeID string)) {
	for nodeID := range outputsReady {
		if plan.States[nodeID] != NodeUnvisited {
			continue
		}
		if prev, ok := previousPlanStates[nodeID]; ok {
			plan.States[nodeID] = prev
			continue
		}
		plan.States[nodeID] = NodePending
		if unrecognizedSkip != nil {
			unrecognizedSkip(nodeID)
		}
	}
}

// SetState records a node's authoritative state.
//
// State does NOT propagate to dependents — the walk coordinator
// dispatches dependents explicitly via tryDispatch, which evaluates
// all dependencies with full precedence (Excluded > Blocked > Pending).
//
// Previous versions eagerly propagated state via a first-wins flood
// fill. This violated precedence in diamond dependencies: if an Error
// parent propagated before an Excluded parent, the child was marked
// Blocked instead of Excluded — an incorrect classification that
// prevented pruning resources that should have been pruned.
func (ps *PlanState) SetState(id string, state NodeState) {
	ps.States[id] = state
}

// PlanSummary holds aggregate state from a completed DAG propagation.
type PlanSummary struct {
	HasPending     bool
	HasNotReady    bool
	HasBlocked     bool
	HasConflict    bool
	HasError       bool
	HasSystemError bool
	ReadyCount     int
}

// HasUncertainty reports whether any node state creates uncertainty about
// which resources should exist. Per 005-reconciliation.md: "Uncertain absence
// blocks pruning — the resource might reappear once the blocker resolves."
func (s PlanSummary) HasUncertainty() bool {
	return s.HasPending || s.HasBlocked || s.HasError || s.HasSystemError
}

// IsClean reports whether all nodes have converged with no errors or pending
// states. Used to determine if superseded revisions can be garbage collected.
func (s PlanSummary) IsClean() bool {
	return !s.HasPending && !s.HasNotReady && !s.HasBlocked && !s.HasConflict && !s.HasError && !s.HasSystemError
}

// Summary returns aggregate state for status reporting.
func (ps *PlanState) Summary() PlanSummary {
	var s PlanSummary
	for _, state := range ps.States {
		switch state {
		case NodeReady:
			s.ReadyCount++
		case NodeNotReady:
			s.HasNotReady = true
		case NodePending:
			s.HasPending = true
		case NodeBlocked:
			s.HasBlocked = true
		case NodeExcluded:
			// Counted but not surfaced — excluded nodes propagate through
			// the DAG via contagious exclusion and are observable in
			// per-node status, not the aggregate summary.
		case NodeError:
			s.HasError = true
		case NodeSystemError:
			s.HasSystemError = true
		case NodeConflict:
			s.HasConflict = true
		}
	}
	return s
}
