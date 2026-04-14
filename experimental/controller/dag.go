package graphcontroller

import (
	"fmt"
)

// DAG builds a dependency graph from a list of Nodes by scanning their
// CEL expressions for variable references. It provides topological ordering
// and dependency-aware planning for the reconcile loop.
type DAG struct {
	// Nodes in declaration order (same as spec.nodes)
	Nodes []Node
	// Index from node ID to node index
	Index map[string]int
	// TopologicalOrder is the apply order (respects dependencies).
	// Computed via Kahn's algorithm — declaration order is not significant.
	TopologicalOrder []int
	// References maps node ID to its detected reference type.
	References map[string]Reference
	// Levels groups node indices by topological level. Nodes within
	// the same level are independent and can be processed in parallel.
	// Level 0 has no dependencies, level 1 depends only on level 0, etc.
	Levels [][]int
	// Dependents maps a node ID to the indices of nodes that depend on it.
	// Reverse adjacency list for eager scheduling — when a node completes,
	// check its dependents to see if they can be dispatched.
	Dependents map[string][]int
	// Finalizers maps a target node ID to the IDs of nodes that declare
	// `finalizes` pointing at it. These nodes are created only when the
	// target becomes a prune candidate. The DAG records the relationship
	// but finalization logic lives in the prune phase, not the walk.
	Finalizers map[string][]string
}

// BuildDAG constructs a dependency graph from a node list.
// exprPaths contains pre-extracted field paths from CEL ASTs (computed during
// compilation in compileGraphSpec). These replace string-scanning with AST-walked
// field paths per 004-graph-execution.md § Change detection.
// Returns an error if the dependency graph contains a cycle (ErrCircularDependency).
// Declaration order is not significant — topological order is computed from
// the dependency graph via Kahn's algorithm.
func BuildDAG(nodes []Node, exprPaths map[string]map[string][]FieldPath) (*DAG, error) {
	dag := &DAG{
		Nodes:      make([]Node, len(nodes)),
		Index:      make(map[string]int, len(nodes)),
		References: make(map[string]Reference),
		Dependents: make(map[string][]int),
		Finalizers: make(map[string][]string),
	}

	for i, node := range nodes {
		node.Dependencies, node.DepPaths, node.SelfPaths, node.ReadinessDeps = extractReferencedPathsFromNode(node, exprPaths)
		dag.Nodes[i] = node
		dag.Index[node.ID] = i
		dag.References[node.ID] = node.Reference()
	}

	// Build finalizer map: target node ID → list of finalizer node IDs.
	// Validate that finalizer targets exist in the DAG.
	for _, node := range dag.Nodes {
		if node.Finalizes != "" {
			if _, exists := dag.Index[node.Finalizes]; !exists {
				return nil, fmt.Errorf("node %q declares finalizes %q, but no node with that ID exists", node.ID, node.Finalizes)
			}
			dag.Finalizers[node.Finalizes] = append(dag.Finalizers[node.Finalizes], node.ID)
		}
	}

	// Push downstream dependency paths into upstream SelfPaths.
	// If node B references deploy.status.availableReplicas, the deploy node
	// needs ["status", "availableReplicas"] in its SelfPaths so self-state
	// changes are detected and the updated scope propagates to B. Without
	// this, a bare Own node with no readyWhen/propagateWhen would have empty
	// SelfPaths — status changes would be invisible to downstream consumers.
	for _, node := range dag.Nodes {
		for depID, paths := range node.DepPaths {
			depIdx, exists := dag.Index[depID]
			if !exists {
				continue
			}
			for _, p := range paths {
				addFieldPath(&dag.Nodes[depIdx].SelfPaths, p)
			}
		}
	}

	// Build reverse adjacency list: for each node, record which nodes depend on it.
	for i, node := range dag.Nodes {
		for depID := range node.Dependencies {
			if _, exists := dag.Index[depID]; exists {
				dag.Dependents[depID] = append(dag.Dependents[depID], i)
			}
		}
	}

	// Kahn's algorithm: topological sort with cycle detection.
	// inDegree counts how many in-graph dependencies each node has.
	n := len(nodes)
	inDegree := make([]int, n)
	for i, node := range dag.Nodes {
		for depID := range node.Dependencies {
			if _, exists := dag.Index[depID]; exists {
				inDegree[i]++
			}
		}
	}

	// Seed the queue with nodes that have no in-graph dependencies.
	var queue []int
	for i, d := range inDegree {
		if d == 0 {
			queue = append(queue, i)
		}
	}

	var order []int
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		order = append(order, curr)

		currID := dag.Nodes[curr].ID
		// Decrement in-degree for every node that depends on curr.
		// Uses the Dependents reverse adjacency list for O(V+E) traversal
		// instead of scanning all nodes — same optimization as propagateState.
		for _, depIdx := range dag.Dependents[currID] {
			inDegree[depIdx]--
			if inDegree[depIdx] == 0 {
				queue = append(queue, depIdx)
			}
		}
	}

	if len(order) != n {
		// Nodes remaining with non-zero in-degree form the cycle.
		var cycleIDs []string
		for i, d := range inDegree {
			if d > 0 {
				cycleIDs = append(cycleIDs, dag.Nodes[i].ID)
			}
		}
		return nil, fmt.Errorf("nodes %v form a dependency cycle: %w", cycleIDs, ErrDependencyError)
	}

	dag.TopologicalOrder = order

	// Compute topological levels. Level[i] = max(Level[dep] for dep in dependencies) + 1.
	// Nodes with no dependencies are level 0.
	nodeLevel := make([]int, n)
	maxLevel := 0
	for _, idx := range order {
		level := 0
		for depID := range dag.Nodes[idx].Dependencies {
			if depIdx, ok := dag.Index[depID]; ok {
				if nodeLevel[depIdx]+1 > level {
					level = nodeLevel[depIdx] + 1
				}
			}
		}
		nodeLevel[idx] = level
		if level > maxLevel {
			maxLevel = level
		}
	}
	dag.Levels = make([][]int, maxLevel+1)
	for idx, level := range nodeLevel {
		dag.Levels[level] = append(dag.Levels[level], idx)
	}

	return dag, nil
}

// NodeState tracks the reconcile-time state of a single node.
type NodeState int

const (
	// nodeUnvisited is the zero-value sentinel — "not yet processed by the
	// walk." Unexported: it is walk-internal machinery, not a design state.
	// The dispatch guard (tryDispatch) uses != nodeUnvisited to detect nodes
	// that have already been assigned a state by the walk or by propagation.
	nodeUnvisited NodeState = iota

	NodePending     // Data not yet available (retryable)
	NodeReady       // Applied and readyWhen satisfied
	NodeNotReady    // Applied but readyWhen not satisfied
	NodeExcluded    // Definitive absence: excluded by includeWhen evaluating to false
	NodeBlocked     // Uncertain absence: dependency in error state
	NodeError       // Client request failed (4xx)
	NodeConflict    // SSA 409 — field ownership taken by another actor
	NodeSystemError // Server/infrastructure failure (5xx, timeout, network)
)

// String returns the human-readable name of the NodeState.
func (s NodeState) String() string {
	switch s {
	case nodeUnvisited:
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
	// PropagateReady tracks whether each node's propagateWhen conditions are
	// satisfied. Nodes without propagateWhen are always true. Used by the walk
	// to gate data flow to dependents during transitions.
	PropagateReady map[string]bool
}

// NewPlanState creates a fresh plan state with all nodes unvisited.
func NewPlanState(dag *DAG) *PlanState {
	ps := &PlanState{
		States:         make(map[string]NodeState, len(dag.Nodes)),
		PropagateReady: make(map[string]bool, len(dag.Nodes)),
	}
	for _, node := range dag.Nodes {
		ps.States[node.ID] = nodeUnvisited
		// Nodes without propagateWhen propagate immediately.
		ps.PropagateReady[node.ID] = len(node.PropagateWhen) == 0
	}
	return ps
}

// DependencyPropagateBlocked returns the ID of a dependency whose
// propagateWhen is unsatisfied, or "" if all dependencies propagate.
// Only checks dependencies that are actual DAG nodes (not CEL builtins).
func (ps *PlanState) DependencyPropagateBlocked(node *Node) string {
	for depID := range node.Dependencies {
		propagates, exists := ps.PropagateReady[depID]
		if !exists {
			continue // not a DAG node (CEL builtin, forEach variable, etc.)
		}
		if !propagates {
			return depID
		}
	}
	return ""
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
func (ps *PlanState) SetState(dag *DAG, id string, state NodeState) {
	ps.States[id] = state
}

// PlanSummary holds aggregate state from a completed DAG walk.
type PlanSummary struct {
	HasPending     bool
	HasNotReady    bool
	HasBlocked     bool
	HasConflict    bool
	HasError       bool
	HasSystemError bool
	ReadyCount     int
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
