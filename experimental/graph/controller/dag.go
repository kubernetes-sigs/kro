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
	// ReverseOrder is the delete order (reverse of TopologicalOrder).
	ReverseOrder []int
	// Shapes maps node ID to its detected template shape.
	Shapes map[string]TemplateShape
	// Levels groups node indices by topological level. Nodes within
	// the same level are independent and can be processed in parallel.
	// Level 0 has no dependencies, level 1 depends only on level 0, etc.
	Levels [][]int
	// Dependents maps a node ID to the indices of nodes that depend on it.
	// Reverse adjacency list for eager scheduling — when a node completes,
	// check its dependents to see if they can be dispatched.
	Dependents map[string][]int
}

// BuildDAG constructs a dependency graph from a node list.
// Dependencies are extracted by scanning CEL expressions for variable references.
// Returns an error if the dependency graph contains a cycle (ErrCycleDetected).
// Declaration order is not significant — topological order is computed from
// the dependency graph via Kahn's algorithm.
func BuildDAG(nodes []Node) (*DAG, error) {
	dag := &DAG{
		Nodes:      make([]Node, len(nodes)),
		Index:      make(map[string]int, len(nodes)),
		Shapes:     make(map[string]TemplateShape),
		Dependents: make(map[string][]int),
	}

	for i, node := range nodes {
		node.Dependencies = extractReferencedIDs(node)
		dag.Nodes[i] = node
		dag.Index[node.ID] = i
		dag.Shapes[node.ID] = node.Shape()
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
		for i, node := range dag.Nodes {
			if node.Dependencies[currID] {
				inDegree[i]--
				if inDegree[i] == 0 {
					queue = append(queue, i)
				}
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
		return nil, fmt.Errorf("nodes %v form a dependency cycle: %w", cycleIDs, ErrCycleDetected)
	}

	dag.TopologicalOrder = order
	dag.ReverseOrder = make([]int, n)
	for i, idx := range order {
		dag.ReverseOrder[n-1-i] = idx
	}

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
	NodePending     NodeState = iota // Not yet processed
	NodeReady                        // Applied and readyWhen satisfied
	NodeNotReady                     // Applied but readyWhen not satisfied
	NodeExcluded                     // Excluded by includeWhen or contagious exclusion
	NodeDataPending                  // CEL expression couldn't resolve (retryable)
	NodeError                        // Fatal error
	NodeConflict                     // SSA 409 — field ownership taken by another actor
)

// PlanState tracks the state of all nodes during a reconcile cycle.
type PlanState struct {
	States map[string]NodeState
	// PropagateReady tracks whether each node's propagateWhen conditions are
	// satisfied. Nodes without propagateWhen are always true. Used by the walk
	// to gate data flow to dependents during transitions.
	PropagateReady map[string]bool
}

// NewPlanState creates a fresh plan state with all nodes pending.
func NewPlanState(dag *DAG) *PlanState {
	ps := &PlanState{
		States:         make(map[string]NodeState, len(dag.Nodes)),
		PropagateReady: make(map[string]bool, len(dag.Nodes)),
	}
	for _, node := range dag.Nodes {
		ps.States[node.ID] = NodePending
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

// CanProcess returns true if the node's dependencies are all in a state
// that allows this node to proceed. A node can proceed if all its
// dependencies are Ready or not referenced at all.
//
// Returns (canProcess, blockingNodeID).
func (ps *PlanState) CanProcess(node *Node) (bool, string) {
	for depID := range node.Dependencies {
		state, exists := ps.States[depID]
		if !exists {
			continue // not a scope variable (could be a CEL builtin)
		}
		switch state {
		case NodeReady, NodeNotReady:
			// Both are "applied and in scope" — dependents proceed.
			// readyWhen is a health signal, not a gate.
			continue
		case NodePending:
			// Dependency hasn't been processed yet — shouldn't happen
			// in topological order, but be safe
			return false, depID
		case NodeExcluded:
			return false, depID
		case NodeDataPending:
			return false, depID
		case NodeError:
			return false, depID
		case NodeConflict:
			return false, depID
		}
	}
	return true, ""
}

// SetState updates a node's state and propagates contagious exclusion.
// When a node is excluded, all nodes that depend on it are also excluded.
// NotReady does NOT propagate exclusion — data is in scope regardless.
func (ps *PlanState) SetState(dag *DAG, id string, state NodeState) {
	ps.States[id] = state

	// Propagate contagious exclusion
	if state == NodeExcluded || state == NodeDataPending || state == NodeError || state == NodeConflict {
		ps.propagateExclusion(dag, id)
	}
}

// propagateExclusion marks all downstream dependents of a node as excluded.
func (ps *PlanState) propagateExclusion(dag *DAG, excludedID string) {
	for _, node := range dag.Nodes {
		if ps.States[node.ID] != NodePending {
			continue // already processed
		}
		if node.Dependencies[excludedID] {
			ps.States[node.ID] = NodeExcluded
			// Recurse: this node's dependents are also excluded
			ps.propagateExclusion(dag, node.ID)
		}
	}
}

// Summary returns aggregate state for status reporting.
func (ps *PlanState) Summary() (hasDataPending, hasNotReady, hasExcluded, hasError, hasConflict bool, readyCount int) {
	for _, state := range ps.States {
		switch state {
		case NodeReady:
			readyCount++
		case NodeNotReady:
			hasNotReady = true
		case NodeDataPending:
			hasDataPending = true
		case NodeExcluded:
			hasExcluded = true
		case NodeError:
			hasError = true
		case NodeConflict:
			hasConflict = true
		}
	}
	return
}
