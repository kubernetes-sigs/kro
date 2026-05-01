// walk.go implements the DAG walk algorithm for a single reconcile cycle.
//
// The walk evaluates nodes sequentially in topological order. Each node is
// evaluated after all of its hard dependencies have been resolved. The walk
// is the single writer to shared state (scope, plan, applied keys).
//
// Per 005-reconciliation.md § Reconcile: "The coordinator walks the DAG in
// topological order, dispatching each node when its dependencies are resolved."
package graphcontroller

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/cel-go/common/types"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	dagpkg "github.com/ellistarn/kro/experimental/controller/dag"
	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
)

// nodeResult carries a node's evaluation output back to the walk loop.
type nodeResult struct {
	keys  []string
	state dagpkg.NodeState
	err   error

	// scope is the published value for this node (full K8s object, collection, or definition map).
	scope any

	// forEach state — returned by forEach expansion for the walk to merge.
	forEach *forEachCarryForward

	// resolvedGVK carries the GVK resolved by a dynamic-GVK node's template
	// evaluation. Used to detect staleness per 004-compilation.md § Deferred Types.
	resolvedGVK *schema.GroupVersionKind
}

// walkResult is the output of a complete DAG walk, consumed by the reconciler.
type walkResult struct {
	keys             []string             // flattened applied keys from all nodes
	nodeKeys         map[string][]string  // per-node applied keys (for carry-forward on next reconcile)
	plan             *dagpkg.PlanState    // per-node states
	scope            map[string]any       // final scope after walk
	nodeReady        map[string]bool      // per-node readiness for .ready() CEL function
	forEach          *forEachCarryForward
	needsRecompile   bool                 // dynamic GVK resolved or changed
	nodeErrors       []string             // "nodeID: reason" for status reporting
	summary          dagpkg.PlanSummary
}

// walk executes a sequential DAG walk in topological order.
//
// Each node is evaluated after all hard dependencies have completed. Lazy
// dependencies are available as optionals in scope but do not gate evaluation.
// The walk is sequential — no goroutines, no channels.
func (r *GraphReconciler) walk(ctx context.Context, rs *reconcileScope, state *instanceState, eval *evaluator, dag *dagpkg.DAG, plan *dagpkg.PlanState) *walkResult {
	logger := log.FromContext(ctx)
	cluster := r.cluster()

	result := &walkResult{
		plan:      plan,
		scope:     eval.scope,
		nodeReady: eval.nodeReady,
		forEach:   state.forEach,
	}

	// Per-node applied keys — flattened into result.keys after the walk.
	nodeKeys := make(map[string][]string, len(dag.Nodes))

	for _, nodeIdx := range dag.TopologicalOrder {
		node := &dag.Nodes[nodeIdx]

		// Finalizer nodes are dormant during normal operation — they only
		// materialize during prune/teardown. Skip them in the forward walk.
		if node.Finalizes != "" {
			plan.SetState(dag, node.ID, dagpkg.NodeReady)
			continue
		}

		// --- Dependency gating ---
		// Check hard dependencies. Lazy deps don't gate dispatch or cause
		// exclusion — the expression has a branch that handles absent data.
		gate := checkDependencyGate(node, plan)

		if gate == gateExcluded {
			// Contagious exclusion: any hard dependency Excluded → Excluded.
			// Per 005-reconciliation.md § Propagation step 1.
			logger.V(1).Info("node excluded — dependency excluded", "node", node.ID)
			plan.SetState(dag, node.ID, dagpkg.NodeExcluded)
			// Excluded nodes advertise ready=true so that non-excluded status
			// rollup expressions don't block on absent nodes.
			eval.scope[node.ID] = map[string]any{"__ready": true}
			if eval.nodeReady != nil {
				eval.nodeReady[node.ID] = true
			}
			state.previousScope[node.ID] = eval.scope[node.ID]
			continue
		}
		if gate == gateBlocked {
			logger.V(1).Info("node blocked — dependency in error state", "node", node.ID)
			plan.SetState(dag, node.ID, dagpkg.NodeBlocked)
			carryForwardKeys(nodeKeys, node.ID, state)
			continue
		}
		if gate == gatePending {
			logger.V(1).Info("node pending — dependency pending", "node", node.ID)
			plan.SetState(dag, node.ID, dagpkg.NodePending)
			carryForwardKeys(nodeKeys, node.ID, state)
			continue
		}

		// --- propagateWhen input gate ---
		// For forEach nodes, propagateWhen is evaluated per-item inside
		// reconcileForEach, not here.
		if len(node.PropagateWhen) > 0 && node.ForEach == nil {
			// Populate __kroDeps for this node so that <id>.dependencies()
			// expressions resolve correctly.
			populateDepsMap(eval, node)

			gate := eval.checkPropagateWhen(node.PropagateWhen, node.ID)
			if gate != gatePass {
				unsatisfied := eval.firstUnsatisfiedCondition(node.PropagateWhen)
				logger.V(1).Info("propagateWhen input gate — retaining previous state",
					"node", node.ID, "unsatisfied", unsatisfied)
				if prev, ok := state.previousScope[node.ID]; ok {
					eval.scope[node.ID] = prev
				}
				carryForwardKeys(nodeKeys, node.ID, state)
				if state.previousPlanStates != nil {
					if prevState, ok := state.previousPlanStates.States[node.ID]; ok {
						plan.States[node.ID] = prevState
					} else {
						plan.States[node.ID] = dagpkg.NodePending
					}
				} else {
					plan.States[node.ID] = dagpkg.NodePending
				}
				continue
			}
		}

		// --- includeWhen ---
		if len(node.IncludeWhen) > 0 {
			included, err := eval.includeWhen(node.IncludeWhen)
			if err != nil {
				carryForwardKeys(nodeKeys, node.ID, state)
				if errors.Is(err, compiler.ErrPending) {
					plan.SetState(dag, node.ID, dagpkg.NodePending)
				} else {
					plan.SetState(dag, node.ID, dagpkg.NodeError)
				}
				continue
			}
			if !included {
				logger.V(1).Info("node excluded by includeWhen", "node", node.ID)
				plan.SetState(dag, node.ID, dagpkg.NodeExcluded)
				eval.scope[node.ID] = map[string]any{"__ready": true}
				if eval.nodeReady != nil {
					eval.nodeReady[node.ID] = true
				}
				state.previousScope[node.ID] = eval.scope[node.ID]
				continue
			}
		}

		// --- Evaluate the node ---
		nr := cluster.evaluateNode(ctx, rs, *node, eval, state)

		// --- Process the result ---
		if nr.state == dagpkg.NodeError {
			info := classifyAPIError(nr.err)
			plan.SetState(dag, node.ID, info.state)
			result.nodeErrors = append(result.nodeErrors, fmt.Sprintf("%s: %s", node.ID, info.reason))
			logger.V(0).Info("error on node", "node", node.ID, "state", info.state, "reason", info.reason, "error", nr.err)
			carryForwardKeys(nodeKeys, node.ID, state)
			continue
		}
		if nr.state == dagpkg.NodeConflict {
			plan.SetState(dag, node.ID, dagpkg.NodeConflict)
			state.previousScope[node.ID] = nr.scope
			result.nodeErrors = append(result.nodeErrors, fmt.Sprintf("%s: field conflict", node.ID))
			logger.V(0).Info("conflict on node", "node", node.ID, "error", nr.err)
			carryForwardKeys(nodeKeys, node.ID, state)
			continue
		}
		if nr.state == dagpkg.NodePending {
			plan.SetState(dag, node.ID, dagpkg.NodePending)
			state.previousScope[node.ID] = nr.scope
			logger.V(1).Info("data pending for node", "node", node.ID, "error", nr.err)
			carryForwardKeys(nodeKeys, node.ID, state)
			continue
		}

		// Publish scope.
		if nr.scope != nil {
			eval.scope[node.ID] = nr.scope
		}

		// CRD creation detection: if a template node just created a CRD,
		// advance the schema generation so the post-walk recompile check
		// catches child graph type errors within the same cycle.
		if r.SchemaGen != nil && node.Type() == graphpkg.NodeTypeTemplate {
			if scopeMap, ok := nr.scope.(map[string]any); ok {
				if scopeMap["apiVersion"] == "apiextensions.k8s.io/v1" && scopeMap["kind"] == "CustomResourceDefinition" {
					r.SchemaGen.AdvanceGeneration()
				}
			}
		}

		// Merge node-readiness verdict.
		if eval.nodeReady != nil {
			eval.nodeReady[node.ID] = (nr.state == dagpkg.NodeReady)
		}

		// Merge forEach state updates.
		if nr.forEach != nil {
			for k, v := range nr.forEach.items {
				state.forEach.items[k] = v
			}
			for nodeID, itemScopes := range nr.forEach.itemScope {
				state.forEach.itemScope[nodeID] = itemScopes
			}
			for nodeID, itemKeys := range nr.forEach.itemKeys {
				state.forEach.itemKeys[nodeID] = itemKeys
			}
		}

		// Update plan state.
		plan.SetState(dag, node.ID, nr.state)
		if nr.state == dagpkg.NodeNotReady && nr.err != nil && errors.Is(nr.err, compiler.ErrReadyWhenFailed) {
			result.nodeErrors = append(result.nodeErrors, fmt.Sprintf("%s: %s", node.ID, nr.err.Error()))
			logger.V(0).Info("readyWhen expression error (not gating dependents)",
				"node", node.ID, "error", nr.err)
		}

		// Record applied keys.
		if nr.state == dagpkg.NodeReady || nr.state == dagpkg.NodeNotReady {
			nodeKeys[node.ID] = nr.keys
		} else {
			carryForwardKeys(nodeKeys, node.ID, state)
		}

		// Save per-node scope for next reconcile.
		state.previousScope[node.ID] = eval.scope[node.ID]

		// Record dynamic GVK resolutions.
		if nr.resolvedGVK != nil {
			if state.mergeDynamicGVK(node.ID, *nr.resolvedGVK) {
				result.needsRecompile = true
				logger.Info("dynamic GVK resolved; will recompile with schema on next reconcile",
					"node", node.ID, "gvk", *nr.resolvedGVK)
			}
		}
	}

	// --- Post-walk ---

	// Flatten per-node keys into the applied key set.
	result.nodeKeys = nodeKeys
	for _, keys := range nodeKeys {
		result.keys = append(result.keys, keys...)
	}

	// Derive aggregate state from the DAG plan.
	result.summary = plan.Summary()

	return result
}

// evaluateNode runs reconcileNode for a single node and translates the
// result into a nodeResult. This is the evaluation boundary — all node-type
// dispatch (Definition, Template, Patch, Ref, Watch, ForEach) happens
// inside reconcileNode.
//
// The evaluator is used directly (no snapshot) since the walk is sequential.
// Lazy dependencies are populated as optional values before dispatch.
func (c *clusterAccess) evaluateNode(ctx context.Context, rs *reconcileScope, node graphpkg.Node, eval *evaluator, state *instanceState) nodeResult {
	// Populate lazy dependencies as CEL optional values. Per 005-reconciliation.md:
	// "Lazy dependencies are always in scope as optional values."
	for depID, kind := range node.Dependencies {
		if kind != graphpkg.DepLazy {
			continue
		}
		if _, exists := eval.scope[depID]; exists {
			continue // already in scope from a previous node evaluation
		}
		// Lazy dep not yet in scope — set optional.none() so the
		// expression's branch that handles absent data can fire.
		if prev, ok := state.previousScope[depID]; ok {
			eval.scope[depID] = celOptionalOf(prev)
		} else {
			eval.scope[depID] = celOptionalNone()
		}
	}

	// Pass carry-forward forEach state directly for forEach nodes.
	var prevForEachState *forEachCarryForward
	if node.ForEach != nil {
		prevForEachState = state.forEach
	}
	out, err := c.reconcileNode(ctx, rs, node, eval, false, prevForEachState)

	nr := nodeResult{
		state: dagpkg.NodeReady,
		scope: eval.scope[node.ID],
	}

	// Unpack nodeOutput fields.
	if out != nil {
		nr.keys = out.keys
		nr.forEach = out.forEach
	}

	if err != nil {
		nr.err = err
		switch {
		case errors.Is(err, compiler.ErrPending):
			nr.state = dagpkg.NodePending
		case errors.Is(err, compiler.ErrWaitingForReadiness):
			nr.state = dagpkg.NodeNotReady
		case errors.Is(err, compiler.ErrReadyWhenFailed):
			// readyWhen is a health signal — does not gate dependents.
			nr.state = dagpkg.NodeNotReady
		case errors.Is(err, compiler.ErrFieldConflict):
			nr.state = dagpkg.NodeConflict
		default:
			nr.state = dagpkg.NodeError
		}
	}

	// Dynamic GVK resolution: check if reconcileApply stored a resolved GVK.
	// In the simplified model, the evaluator tracks this per-node.
	if node.HasDynamicGVR() {
		if scopeMap, ok := nr.scope.(map[string]any); ok {
			gvk := graphpkg.GVKFromMap(scopeMap)
			if gvk.Kind != "" {
				nr.resolvedGVK = &gvk
			}
		}
	}

	return nr
}

// gateState represents the dependency gating outcome for a node.
type gateState int

const (
	gateDispatch  gateState = iota // all hard deps completed — proceed to evaluation
	gateExcluded                    // at least one hard dep is Excluded
	gateBlocked                     // at least one hard dep is in an error state
	gatePending                     // at least one hard dep is Pending
)

// checkDependencyGate inspects a node's hard dependencies and determines
// whether the node can be dispatched for evaluation. Lazy dependencies
// are ignored — they don't gate dispatch.
//
// Precedence: Excluded > Blocked > Pending, matching
// 005-reconciliation.md § Propagation.
func checkDependencyGate(node *graphpkg.Node, plan *dagpkg.PlanState) gateState {
	hasExcluded := false
	hasBlocked := false
	hasPending := false
	for depID, kind := range node.Dependencies {
		if kind != graphpkg.DepHard {
			continue
		}
		depState, exists := plan.States[depID]
		if !exists {
			continue
		}
		switch depState {
		case dagpkg.NodeReady, dagpkg.NodeNotReady:
			// Dependency completed — proceed.
		case dagpkg.NodeExcluded:
			hasExcluded = true
		case dagpkg.NodePending:
			hasPending = true
		default:
			// Error, SystemError, Conflict, Blocked → blocked.
			hasBlocked = true
		}
	}
	switch {
	case hasExcluded:
		return gateExcluded
	case hasBlocked:
		return gateBlocked
	case hasPending:
		return gatePending
	default:
		return gateDispatch
	}
}
// per-node key map. Used when a node is blocked, pending, or in error — the
// resource may still exist in the cluster, so its keys must remain in the
// applied set to prevent spurious pruning.
func carryForwardKeys(nodeKeys map[string][]string, nodeID string, state *instanceState) {
	if prev, ok := state.previousKeys[nodeID]; ok {
		nodeKeys[nodeID] = prev
	}
}

// populateDepsMap injects the __kroDeps map entry for a node so that
// <id>.dependencies() CEL expressions resolve correctly.
func populateDepsMap(eval *evaluator, node *graphpkg.Node) {
	depsMap, _ := eval.scope[compiler.ReservedDepsMapVar].(map[string]any)
	if depsMap == nil {
		depsMap = make(map[string]any, 8)
		eval.scope[compiler.ReservedDepsMapVar] = depsMap
	}
	depValues := make([]any, 0, len(node.Dependencies))
	for depID := range node.Dependencies {
		if v, ok := eval.scope[depID]; ok {
			depValues = append(depValues, v)
		}
	}
	depsMap[node.ID] = depValues
}

// celOptionalOf wraps a scope value in a CEL optional.of() so lazy
// dependencies are available to expressions as optional values.
func celOptionalOf(v any) any {
	return types.OptionalOf(types.DefaultTypeAdapter.NativeToValue(v))
}

// celOptionalNone returns a CEL optional.none() for absent lazy dependencies.
func celOptionalNone() any {
	return types.OptionalNone
}
