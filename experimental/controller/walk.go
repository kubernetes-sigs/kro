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
	keys  []Applied
	state NodeState
	err   error

	// scope is the published value for this node (full K8s object, collection, or definition map).
	scope any

	// resolvedGVK carries the GVK resolved by a dynamic-GVK node's template
	// evaluation. Used to detect staleness per 004-compilation.md § Deferred Types.
	resolvedGVK *schema.GroupVersionKind
}

// walkResult is the output of a complete DAG walk, consumed by the reconciler.
type walkResult struct {
	keys           []Applied            // flattened applied keys from all nodes
	nodeKeys       map[string][]Applied // per-node applied keys (for carry-forward on next reconcile)
	plan           *PlanState    // per-node states
	needsRecompile bool          // dynamic GVK resolved or changed
	nodeErrors     []string      // "nodeID: reason" for status reporting
	summary        PlanSummary
}

// walk executes a sequential DAG walk in topological order.
//
// Each node is evaluated after all hard dependencies have completed. Lazy
// dependencies are available as optionals in scope but do not gate evaluation.
// The walk is sequential — no goroutines, no channels.
func (r *GraphReconciler) walk(ctx context.Context, rs *reconcileScope, state *instanceState, eval *evaluator, dag *dagpkg.DAG, plan *PlanState) *walkResult {
	logger := log.FromContext(ctx)
	cluster := r.cluster()

	result := &walkResult{
		plan: plan,
	}

	// Per-node applied keys — flattened into result.keys after the walk.
	nodeKeys := make(map[string][]Applied, len(dag.Nodes))

	for _, nodeIdx := range dag.TopologicalOrder {
		node := &dag.Nodes[nodeIdx]

		// Finalizer nodes are dormant during normal operation — they only
		// materialize during prune/teardown. Skip them in the forward walk.
		if node.Finalizes != "" {
			plan.SetState(node.ID, NodeReady)
			continue
		}

		// --- Dependency gating ---
		// Check hard dependencies. Lazy deps don't gate dispatch or cause
		// exclusion — the expression has a branch that handles absent data.
		gate := checkDependencyGate(node, plan)

		if gate == depExcluded {
			// Contagious exclusion: any hard dependency Excluded → Excluded.
			// Per 005-reconciliation.md § Propagation step 1.
			logger.V(1).Info("node excluded — dependency excluded", "node", node.ID)
			plan.SetState(node.ID, NodeExcluded)
			markExcluded(eval, node.ID, state)
			continue
		}
		if gate == depBlocked {
			logger.V(1).Info("node blocked — dependency in error state", "node", node.ID)
			plan.SetState(node.ID, NodeBlocked)
			carryForwardKeys(nodeKeys, node.ID, state)
			continue
		}
		if gate == depPending {
			logger.V(1).Info("node pending — dependency pending", "node", node.ID)
			plan.SetState(node.ID, NodePending)
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
				if prev, ok := state.walk.previousScope[node.ID]; ok {
					eval.scope[node.ID] = prev
				}
				carryForwardKeys(nodeKeys, node.ID, state)
				if state.walk.previousPlanStates != nil {
					if prevState, ok := state.walk.previousPlanStates.States[node.ID]; ok {
						plan.States[node.ID] = prevState
					} else {
						plan.States[node.ID] = NodePending
					}
				} else {
					plan.States[node.ID] = NodePending
				}
				continue
			}
		}

		// --- includeWhen ---
		if len(node.IncludeWhen) > 0 {
			included, err := eval.includeWhen(node.IncludeWhen)
			if err != nil {
				carryForwardKeys(nodeKeys, node.ID, state)
				if errors.Is(err, ErrPending) {
					plan.SetState(node.ID, NodePending)
				} else {
					plan.SetState(node.ID, NodeError)
				}
				continue
			}
			if !included {
				logger.V(1).Info("node excluded by includeWhen", "node", node.ID)
				plan.SetState(node.ID, NodeExcluded)
				markExcluded(eval, node.ID, state)
				continue
			}
		}

		// --- Evaluate the node ---
		nr := evaluateNode(ctx, cluster, rs, *node, eval, state)

		// --- Integrate the result into walk state ---
		nrOut := integrateNodeResult(ctx, nr, node, eval, state, plan, nodeKeys)
		result.nodeErrors = append(result.nodeErrors, nrOut.errMsgs...)
		if nrOut.needsRecompile {
			result.needsRecompile = true
		}
		if nrOut.crdCreated && r.SchemaGen != nil {
			r.SchemaGen.AdvanceGeneration()
			result.needsRecompile = true
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

// nodeIntegrationResult carries the outputs of integrateNodeResult back
// to the walk loop. Decouples the walk from node-result classification
// internals (apiErrorInfo structure).
type nodeIntegrationResult struct {
	errMsgs        []string // "nodeID: reason" error messages for status
	needsRecompile bool     // dynamic GVK resolved or changed
	crdCreated     bool     // template node created a CRD (caller advances SchemaGen)
}

// integrateNodeResult processes a single node's evaluation output and
// updates the walk's shared state: plan states, scope, keys, forEach,
// and dynamic GVK resolution. Handles error states (Error, Conflict,
// Pending) and the success path (scope publish, readiness merge, key
// recording). Returns status messages and signals for the caller.
//
// This is a pure integration function — it reads the nodeResult and
// mutates eval, state, plan, and nodeKeys. The CRD detection signal
// is returned rather than acted on, keeping SchemaGeneration coupling
// out of this function.
func integrateNodeResult(
	ctx context.Context,
	nr nodeResult,
	node *graphpkg.Node,
	eval *evaluator,
	state *instanceState,
	plan *PlanState,
	nodeKeys map[string][]Applied,
) nodeIntegrationResult {
	logger := log.FromContext(ctx)
	var out nodeIntegrationResult

	// --- Error states: set plan state, record error, carry forward keys ---
	if nr.state == NodeError {
		info := classifyAPIError(nr.err)
		plan.SetState(node.ID, info.state)
		out.errMsgs = append(out.errMsgs, fmt.Sprintf("%s: %s", node.ID, info.reason))
		logger.V(0).Info("error on node", "node", node.ID, "state", info.state, "reason", info.reason, "error", nr.err)
		carryForwardKeys(nodeKeys, node.ID, state)
		return out
	}
	if nr.state == NodeConflict {
		plan.SetState(node.ID, NodeConflict)
		state.walk.previousScope[node.ID] = nr.scope
		out.errMsgs = append(out.errMsgs, fmt.Sprintf("%s: field conflict", node.ID))
		logger.V(0).Info("conflict on node", "node", node.ID, "error", nr.err)
		carryForwardKeys(nodeKeys, node.ID, state)
		return out
	}
	if nr.state == NodePending {
		plan.SetState(node.ID, NodePending)
		state.walk.previousScope[node.ID] = nr.scope
		logger.V(1).Info("data pending for node", "node", node.ID, "error", nr.err)
		carryForwardKeys(nodeKeys, node.ID, state)
		return out
	}

	// --- Success path ---

	// Publish scope.
	if nr.scope != nil {
		eval.scope[node.ID] = nr.scope
	}

	// CRD creation detection: if a template node just created a CRD,
	// signal the caller to advance the schema generation so the post-walk
	// recompile check catches child graph type errors within the same cycle.
	if node.Type() == graphpkg.NodeTypeTemplate {
		if scopeMap, ok := nr.scope.(map[string]any); ok {
			if scopeMap["apiVersion"] == "apiextensions.k8s.io/v1" && scopeMap["kind"] == "CustomResourceDefinition" {
				out.crdCreated = true
			}
		}
	}

	// Merge node-readiness verdict.
	if eval.nodeReady != nil {
		eval.nodeReady[node.ID] = (nr.state == NodeReady)
	}

	// Update plan state.
	plan.SetState(node.ID, nr.state)
	if nr.state == NodeNotReady && nr.err != nil && errors.Is(nr.err, ErrReadyWhenFailed) {
		out.errMsgs = append(out.errMsgs, fmt.Sprintf("%s: %s", node.ID, nr.err.Error()))
		logger.V(0).Info("readyWhen expression error (not gating dependents)",
			"node", node.ID, "error", nr.err)
	}

	// Record applied keys.
	if nr.state == NodeReady || nr.state == NodeNotReady {
		nodeKeys[node.ID] = nr.keys
	} else {
		carryForwardKeys(nodeKeys, node.ID, state)
	}

	// Save per-node scope for next reconcile.
	state.walk.previousScope[node.ID] = eval.scope[node.ID]

	// Record dynamic GVK resolutions.
	if nr.resolvedGVK != nil {
		if state.mergeDynamicGVK(node.ID, *nr.resolvedGVK) {
			out.needsRecompile = true
			logger.Info("dynamic GVK resolved; will recompile with schema on next reconcile",
				"node", node.ID, "gvk", *nr.resolvedGVK)
		}
	}

	return out
}

// evaluateNode runs reconcileNode for a single node and translates the
// result into a nodeResult. This is the evaluation boundary — all node-type
// dispatch (Definition, Template, Patch, Ref, Watch, ForEach) happens
// inside reconcileNode.
//
// The evaluator is used directly (no snapshot) since the walk is sequential.
// Lazy dependencies are populated as optional values before dispatch.
func evaluateNode(ctx context.Context, c *clusterAccess, rs *reconcileScope, node graphpkg.Node, eval *evaluator, state *instanceState) nodeResult {
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
		if prev, ok := state.walk.previousScope[depID]; ok {
			eval.scope[depID] = celOptionalOf(prev)
		} else {
			eval.scope[depID] = celOptionalNone()
		}
	}

	out, err := reconcileNode(ctx, c, rs, node, eval)

	nr := nodeResult{
		state: NodeReady,
		scope: eval.scope[node.ID],
	}

	// Unpack nodeOutput fields.
	if out != nil {
		nr.keys = out.keys
	}

	if err != nil {
		nr.err = err
		switch {
		case errors.Is(err, ErrPending):
			nr.state = NodePending
		case errors.Is(err, ErrWaitingForReadiness):
			nr.state = NodeNotReady
		case errors.Is(err, ErrReadyWhenFailed):
			// readyWhen is a health signal — does not gate dependents.
			nr.state = NodeNotReady
		case errors.Is(err, ErrFieldConflict):
			nr.state = NodeConflict
		default:
			nr.state = NodeError
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

// depGateOutcome represents the dependency gating outcome for a node.
type depGateOutcome int

const (
	_           depGateOutcome = iota // zero value unused
	depExcluded                       // at least one hard dep is Excluded
	depBlocked                        // at least one hard dep is in an error state
	depPending                        // at least one hard dep is Pending
)

// checkDependencyGate inspects a node's hard dependencies and determines
// whether the node can be dispatched for evaluation. Lazy dependencies
// are ignored — they don't gate dispatch.
//
// Precedence: Excluded > Blocked > Pending, matching
// 005-reconciliation.md § Propagation.
func checkDependencyGate(node *graphpkg.Node, plan *PlanState) depGateOutcome {
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
		case NodeReady, NodeNotReady:
			// Dependency completed — proceed.
		case NodeExcluded:
			hasExcluded = true
		case NodePending:
			hasPending = true
		default:
			// Error, SystemError, Conflict, Blocked → blocked.
			hasBlocked = true
		}
	}
	switch {
	case hasExcluded:
		return depExcluded
	case hasBlocked:
		return depBlocked
	case hasPending:
		return depPending
	default:
		return 0 // all hard deps completed — proceed to evaluation
	}
}

// per-node key map. Used when a node is blocked, pending, or in error — the
// resource may still exist in the cluster, so its keys must remain in the
// applied set to prevent spurious pruning.
func carryForwardKeys(nodeKeys map[string][]Applied, nodeID string, state *instanceState) {
	if prev, ok := state.walk.previousKeys[nodeID]; ok {
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

// markExcluded stamps a node as excluded in scope and carry-forward state.
// Excluded nodes advertise ready=true so that non-excluded status rollup
// expressions don't block on absent nodes.
func markExcluded(eval *evaluator, nodeID string, state *instanceState) {
	eval.scope[nodeID] = map[string]any{"__ready": true}
	if eval.nodeReady != nil {
		eval.nodeReady[nodeID] = true
	}
	state.walk.previousScope[nodeID] = eval.scope[nodeID]
}
