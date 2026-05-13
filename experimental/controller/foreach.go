// foreach.go implements forEach node expansion — stamping a template once per
// item in a collection.
package graphcontroller

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
)

// reconcileForEach iterates a collection and stamps the template per item.
//
// No state is carried between reconciles — each cycle evaluates fresh.
func (c *clusterAccess) reconcileForEach(ctx context.Context, rs *reconcileScope, node graphpkg.Node, eval *evaluator) (*nodeOutput, error) {
	logger := log.FromContext(ctx)
	var keys []Applied
	// Per-item propagateWhen gate. Per 001-graph.md § propagateWhen:
	// "With forEach, [...] the controller evaluates propagateWhen
	// per-item and halts when the condition is first false."
	//
	// The gate is evaluated before each item against the
	// partially-built collection (eval.scope[node.ID]). Items
	// processed so far have fresh __ready stamps so expressions
	// like ${workers.filter(w, !w.ready()).size() < 2} reflect
	// current readiness. Gated items are Pending — not evaluated, no
	// scope entry produced.
	hasPerItemGate := len(node.PropagateWhen) > 0

	varName := node.ForEach.VarName
	collectionExpr := node.ForEach.Expr

	collection, err := eval.evalString(collectionExpr)
	if err != nil {
		if isPending(err) {
			return nil, fmt.Errorf("evaluating collection %q: %w", collectionExpr, ErrPending)
		}
		return nil, fmt.Errorf("evaluating collection %q: %w", collectionExpr, err)
	}

	items, ok := collection.([]any)
	if !ok {
		items = []any{collection}
	}
	logger.V(1).Info("forEach expanding", "node", node.ID, "var", varName, "count", len(items))

	// Build identity → item map for the current collection.
	currentItems := make(map[string]any, len(items))
	var currentOrder []string
	for _, item := range items {
		id := forEachItemIdentity(item)
		// Per 005-reconciliation.md § forEach: identity must be
		// unique across items. Duplicate identities silently drop one
		// item (map overwrite) — that's data loss, not dedup.
		if _, exists := currentItems[id]; exists {
			return nil, fmt.Errorf("forEach %s: duplicate item identity %q — "+
				"two collection items resolve to the same identity", node.ID, id)
		}
		currentItems[id] = item
		currentOrder = append(currentOrder, id)
	}

	// Pre-evaluate readiness by fetching each item's resource from the
	// cache and evaluating readyWhen. This replaces carry-forward readiness
	// with fresh per-cycle evaluation. Cache hits make this cheap.
	readinessMap := c.preEvaluateReadiness(ctx, eval, node, currentItems, rs.namespace)

	// Per 005-reconciliation.md § Propagation Control:
	// "Ready before NotReady, before error states. Within a
	// readiness class, random."
	sortForEachByReadiness(currentOrder, readinessMap)

	// Evaluate every item every time (simple, correct).
	var allApplied []any
	var childErrors []error                     // track per-child errors for state derivation
	seenResourceKeys := make(map[string]string) // resource key → item identity
	halted := false

	// For forEach metric nodes: reset the metric before iteration so stale
	// dimensions from previous cycles (disappeared items) are removed.
	// Each child contributes fresh values — only active series survive.
	if node.Type() == graphpkg.NodeTypeMetric && node.Metric != nil && rs.metricStore != nil {
		rs.metricStore.Reset(rs.graphKey, node.Metric.Name)
	}

	for _, id := range currentOrder {
		// --- Per-item propagateWhen gate ---
		if hasPerItemGate && !halted {
			// Expose the partially-built collection so the gate
			// expression can inspect already-processed items.
			eval.scope[node.ID] = allApplied
			gate, _ := eval.checkPropagateWhen(node.PropagateWhen, node.ID)
			if gate != gatePass {
				halted = true
				logger.V(1).Info("per-item propagateWhen halted forEach expansion",
					"node", node.ID, "haltedAt", id, "processedCount", len(allApplied))
			}
		}
		if halted {
			// Halted items are not dispatched. Parent becomes Pending,
			// blocking downstream. No scope injection for halted items.
			continue
		}

		item := currentItems[id]

		if !node.HasBody() {
			// Metric nodes don't have a map body (HasBody checks Payload/TemplateExpr).
			// Handle metric forEach inline — evaluate per child with child scope.
			if node.Type() == graphpkg.NodeTypeMetric {
				innerScope := graphpkg.CopyScope(eval.scope)
				innerScope[varName] = item
				innerEval := eval.withScope(innerScope)
				if err := reconcileForEachMetricChild(ctx, node, innerEval, rs.metricStore, rs.graphKey); err != nil {
					childErrors = append(childErrors, fmt.Errorf("forEach metric %s item: %w", node.ID, err))
					logger.V(1).Info("forEach metric item error", "node", node.ID, "item", id, "error", err)
				}
				allApplied = append(allApplied, map[string]any{"__updated": true})
				continue
			}
			continue
		}
		innerScope := graphpkg.CopyScope(eval.scope)
		innerScope[varName] = item
		innerEval := eval.withScope(innerScope)

		// Definition forEach: evaluate the template per item, collect
		// values into []any. No resource is created or tracked.
		if node.Type() == graphpkg.NodeTypeDef {
			evalMap, err := innerEval.toMapNode(node)
			if err != nil {
				childErrors = append(childErrors, fmt.Errorf("forEach defines %s item: %w", node.ID, err))
				logger.V(1).Info("forEach definition item error", "node", node.ID, "item", id, "error", err)
				continue
			}
			// Definitions are always re-evaluated — vacuously updated.
			evalMap["__updated"] = true
			allApplied = append(allApplied, evalMap)
			// No keys — definition nodes have no managed resources.
			continue
		}

		evalMap, err := innerEval.toMapNode(node)
		if err != nil {
			return nil, fmt.Errorf("forEach %s item: %w", node.ID, err)
		}

		// Stamp forEach child identity labels per 005-reconciliation.md § Child Identity.
		// Each child's label key encodes the full resource key:
		//   <parentID>.<name>.<namespace>.<kind>.<group>.<graph>.<graphns>.internal.kro.run/type
		childObj := &unstructured.Unstructured{Object: evalMap}
		if childObj.GetNamespace() == "" {
			childObj.SetNamespace(rs.namespace)
		}
		gvk := childObj.GroupVersionKind()

		// Per 005-reconciliation.md § forEach: "Resource keys must be
		// unique across children of the same parent — validated at expansion
		// time." Detect duplicate resource keys before any apply. Without
		// this, the second item silently overwrites the first in the identity
		// map and one child stops being managed.
		childResKey := resourceCacheKey(childObj.GetAPIVersion(), gvk.Kind, childObj.GetNamespace(), childObj.GetName())
		if prevItemID, exists := seenResourceKeys[childResKey]; exists {
			return nil, fmt.Errorf("forEach %s: duplicate resource key %s from items %q and %q — "+
				"each child must produce a unique resource (apiVersion/kind/namespace/name)", node.ID, childResKey, prevItemID, id)
		}
		seenResourceKeys[childResKey] = id

		// forEach child classification is inherited from the parent's
		// declared keyword. template: → Template per child; patch: →
		// Patch per child. Declaration is authoritative — no per-child
		// runtime resolution.
		childNodeType := node.Type()

		graphpkg.StampForEachChildLabels(childObj, node.ID, rs.name, rs.namespace, eval.effectiveGeneration, childNodeType)
		evalMap = childObj.Object

		var applied *unstructured.Unstructured
		if childNodeType == graphpkg.NodeTypePatch {
			applied, err = c.applySSA(ctx, rs, evalMap, node.ID, graphpkg.NodeTypePatch, eval.effectiveGeneration, node.Lifecycle.ForceApply())
		} else {
			applied, err = c.applySSA(ctx, rs, evalMap, node.ID, graphpkg.NodeTypeTemplate, eval.effectiveGeneration, node.Lifecycle.ForceApply())
		}
		if err != nil {
			// Per 005-reconciliation.md § Parent State: track per-child errors
			// for proper state aggregation. Don't fail fast on the first error.
			childErrors = append(childErrors, err)
			logger.V(1).Info("forEach child error", "node", node.ID, "item", id, "error", err)
			continue
		}
		allApplied = append(allApplied, applied.Object)
		// Just applied with effectiveGeneration — child is on the latest generation.
		applied.Object["__updated"] = true
		itemKeys := []Applied{{
			Key:       resourceKey(applied),
			NodeType:  childNodeType,
			HasStatus: evalMap["status"] != nil,
		}}
		keys = append(keys, itemKeys...)

		// Inline readyWhen stamp: required when per-item
		// propagateWhen is active so the gate expression sees
		// __ready on items processed earlier in this cycle.
		if hasPerItemGate {
			stampSingleItemReady(eval, node.ID, applied.Object, node.ReadyWhen)
		}
	}

	// Per 005-reconciliation.md § Parent State: derive parent state from children.
	// Error states take precedence over Pending; deterministic errors (Error)
	// take precedence over transient errors (SystemError, Conflict).
	//
	// Per 001-graph.md § forEach: "The parent enters scope (enabling
	// downstream evaluation) once all children have applied successfully."
	// Do NOT publish partial scope on error — dependents must see the
	// error classification from the coordinator's Block path, not a
	// partially-applied array. Error-then-publish (not publish-then-error)
	// makes the invariant structural rather than coordinator-dependent.
	if len(childErrors) > 0 {
		delete(eval.scope, node.ID)
		return &nodeOutput{keys: keys}, highestPriorityChildError(childErrors)
	}

	// Per-item propagateWhen halted expansion — the collection is
	// incomplete. Parent is Pending, blocking downstream.
	// Clean up the partial scope set during gate evaluation.
	if halted {
		delete(eval.scope, node.ID)
		return &nodeOutput{keys: keys}, ErrPending
	}

	// Per 001-graph.md: metric nodes "do not publish to scope." Skip scope
	// publication for metric forEach — no downstream node should reference
	// a metric's internal data.
	if node.Type() == graphpkg.NodeTypeMetric {
		return &nodeOutput{keys: keys}, nil
	}

	// Only publish scope when fully expanded — all items dispatched.
	eval.scope[node.ID] = allApplied

	// Check readyWhen per-item and stamp __ready on each item.
	// When per-item propagateWhen is active, readyWhen was already
	// stamped inline during the loop — skip the post-loop pass.
	if !hasPerItemGate {
		if err := forEachStampReadyWhen(eval.scope, node.ID, node.ReadyWhen, eval); err != nil {
			return &nodeOutput{keys: keys}, err
		}
		if len(node.ReadyWhen) > 0 {
			logger.V(1).Info("all forEach items ready", "node", node.ID)
		}
	}

	return &nodeOutput{keys: keys}, nil
}

// forEachStampReadyWhen evaluates readyWhen per-item and stamps __ready on
// each item in scope[nodeID]. When readyWhen is empty, all items are stamped
// __ready=true (applied = ready). Returns ErrWaitingForReadiness if any item
// fails its readyWhen check.
//
// eval may be nil when readyWhen is empty (no expressions to evaluate).
func forEachStampReadyWhen(scope map[string]any, nodeID string, readyWhen []string, eval *evaluator) error {
	scopeVal := scope[nodeID]
	if scopeVal == nil {
		return nil
	}
	items, ok := scopeVal.([]any)
	if !ok {
		return fmt.Errorf("forEach %s: scope value is %T, expected []any", nodeID, scopeVal)
	}

	if len(readyWhen) > 0 {
		anyNotReady := false
		for _, applied := range items {
			saved := scope[nodeID]
			scope[nodeID] = applied
			err := eval.evalReadinessConditions(readyWhen, nodeID)
			scope[nodeID] = saved
			if err != nil {
				if m, ok := applied.(map[string]any); ok {
					m["__ready"] = false
				}
				anyNotReady = true
				continue
			}
			if m, ok := applied.(map[string]any); ok {
				m["__ready"] = true
			}
		}
		if anyNotReady {
			return ErrWaitingForReadiness
		}
	} else {
		// No readyWhen — all items are ready on apply.
		for _, applied := range items {
			if m, ok := applied.(map[string]any); ok {
				m["__ready"] = true
			}
		}
	}
	return nil
}

// stampSingleItemReady evaluates readyWhen for a single forEach item and
// stamps __ready on it. Used during per-item propagateWhen gating so that
// the gate expression can see readiness state for items processed earlier
// in the same cycle.
func stampSingleItemReady(eval *evaluator, nodeID string, item any, readyWhen []string) {
	m, ok := item.(map[string]any)
	if !ok {
		return
	}
	if len(readyWhen) == 0 {
		m["__ready"] = true
		return
	}
	// Temporarily point scope[nodeID] at this single item so readyWhen
	// expressions resolve against the item under evaluation.
	saved := eval.scope[nodeID]
	eval.scope[nodeID] = item
	err := eval.evalReadinessConditions(readyWhen, nodeID)
	eval.scope[nodeID] = saved
	if err != nil {
		m["__ready"] = false
	} else {
		m["__ready"] = true
	}
}

// forEachItemIdentity extracts a stable identity from a forEach collection item.
// Uses metadata.name if the item is a Kubernetes object, otherwise falls back
// to a content hash. This ensures collection reordering doesn't cause churn.
func forEachItemIdentity(item any) string {
	if m, ok := item.(map[string]any); ok {
		if md, ok := m["metadata"].(map[string]any); ok {
			name, hasName := md["name"].(string)
			ns, hasNs := md["namespace"].(string)
			switch {
			case hasName && hasNs && ns != "":
				// Namespaced k8s object: namespace/name is the cluster-unique
				// key. Using only name collides across namespaces when a
				// forEach collection spans them (e.g. watching ConfigMaps
				// cluster-wide: every namespace has a kube-root-ca.crt).
				return ns + "/" + name
			case hasName:
				return name
			}
		}
		// No metadata — use deterministic string representation.
		// fmt.Sprintf("%v") on Go maps sorts keys as of Go 1.12.
		return fmt.Sprintf("%v", m)
	}
	// Scalar item — use string representation.
	// %v on Go maps and slices is deterministic (sorts map keys as of Go 1.12),
	// and %v on numeric types stringifies float64(5) and int64(5) to the same
	// "5", so CEL type drift does not cause phantom child churn.
	return fmt.Sprintf("%v", item)
}

// sortForEachByReadiness orders items by readiness class per
// 005-reconciliation.md § Propagation Control: Ready before NotReady,
// before error states. Within each class, random.
//
// Approach: shuffle the entire slice randomly, then stable-sort by class.
// This gives random order within each class and class ordering across classes.
// readinessMap maps item identity → readiness class (0=Ready, 1=NotReady, 2=Error).
func sortForEachByReadiness(items []string, readinessMap map[string]int) {
	rand.Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })
	sort.SliceStable(items, func(i, j int) bool {
		return readinessMap[items[i]] < readinessMap[items[j]]
	})
}

// preEvaluateReadiness does a lightweight pre-pass over forEach items to
// determine each item's readiness class by fetching the resource from the
// cache and evaluating readyWhen. Per 005-reconciliation.md § Propagation
// Control.
//
// Returns a map of item identity → readiness class:
//
//	0 = Ready (resource exists AND readyWhen passes, or no readyWhen)
//	1 = NotReady (resource doesn't exist OR exists but readyWhen fails)
//	2 = Error (pre-evaluation itself failed: template eval error, non-404 GET error, etc.)
//
// Errors in the pre-pass do NOT block the main loop — they sort last (class 2).
func (c *clusterAccess) preEvaluateReadiness(ctx context.Context, eval *evaluator, node graphpkg.Node, items map[string]any, defaultNS string) map[string]int {
	logger := log.FromContext(ctx)
	result := make(map[string]int, len(items))

	// Definition nodes have no managed resource — always ready.
	if node.Type() == graphpkg.NodeTypeDef {
		for id := range items {
			result[id] = 0
		}
		return result
	}

	// If the node has no body (can't evaluate template), treat all as not ready.
	if !node.HasBody() {
		for id := range items {
			result[id] = 1
		}
		return result
	}

	varName := node.ForEach.VarName

	for id, item := range items {
		class := c.preEvaluateOneItem(ctx, eval, node, varName, item, defaultNS)
		result[id] = class
		if class != 0 {
			logger.V(2).Info("pre-evaluated readiness", "node", node.ID, "item", id, "class", class)
		}
	}
	return result
}

// preEvaluateOneItem evaluates readiness for a single forEach item.
// Per 005-reconciliation.md § Propagation Control.
//
// Returns readiness class:
//
//	0 = Ready (resource exists AND readyWhen passes)
//	1 = NotReady (resource doesn't exist OR readyWhen fails)
//	2 = Error (template eval error, non-404 GET error, GVK resolution failure)
func (c *clusterAccess) preEvaluateOneItem(ctx context.Context, eval *evaluator, node graphpkg.Node, varName string, item any, defaultNS string) int {
	// Save and restore the iterator variable to avoid polluting eval.scope.
	savedVar, hadVar := eval.scope[varName]
	eval.scope[varName] = item
	defer func() {
		if hadVar {
			eval.scope[varName] = savedVar
		} else {
			delete(eval.scope, varName)
		}
	}()

	// Evaluate the template to get the desired resource state.
	evalMap, err := eval.toMapNode(node)
	if err != nil {
		return 2 // template eval failed — error
	}

	obj := &unstructured.Unstructured{Object: evalMap}
	if obj.GetNamespace() == "" {
		obj.SetNamespace(defaultNS)
	}
	gvk := obj.GroupVersionKind()
	if gvk.Kind == "" {
		return 2 // can't resolve GVK — error
	}

	// GET the resource from the cache — existence check only.
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(gvk)
	key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	err = c.client.Get(ctx, key, existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return 1 // resource doesn't exist yet — not ready
		}
		return 2 // non-404 GET error — error
	}

	// Resource exists. If no readyWhen, it's ready by definition.
	if len(node.ReadyWhen) == 0 {
		return 0
	}

	// Evaluate readyWhen against the EXISTING resource (observed state),
	// not the template. Status-based readyWhen expressions (e.g.,
	// status.readyReplicas == spec.replicas) require the actual resource
	// from cache — templates have no .status field.
	savedNode, hadNode := eval.scope[node.ID]
	eval.scope[node.ID] = existing.Object
	err = eval.evalReadinessConditions(node.ReadyWhen, node.ID)
	if hadNode {
		eval.scope[node.ID] = savedNode
	} else {
		delete(eval.scope, node.ID)
	}

	if err != nil {
		return 1 // readyWhen failed — not ready
	}
	return 0 // ready
}

// highestPriorityChildError returns the highest-priority error from a list
// of forEach child errors. Per 005-reconciliation.md § Parent State:
// "Deterministic errors (Error) take precedence over transient errors
// (SystemError, Conflict) — if any child's failure is deterministic,
// retrying cannot resolve the parent."
//
// Priority order: NodeError (deterministic) > NodeConflict > NodeSystemError > NodePending.
func highestPriorityChildError(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	best := errs[0]
	bestPriority := childErrorPriority(best)
	for _, err := range errs[1:] {
		p := childErrorPriority(err)
		if p > bestPriority {
			best = err
			bestPriority = p
		}
	}
	return best
}

// childErrorPriority returns a numeric priority for a forEach child error.
// Higher values mean higher priority (deterministic errors > transient).
func childErrorPriority(err error) int {
	if errors.Is(err, ErrPending) {
		return 0
	}
	if errors.Is(err, ErrFieldConflict) {
		return 2 // Conflict — transient, but a specific positive signal
	}
	info := classifyAPIError(err)
	switch info.state {
	case NodeSystemError:
		return 1 // SystemError — transient
	default:
		return 3 // NodeError — deterministic
	}
}

// resourceCacheKey builds a composite key from a resource's identifying fields.
// Used by forEach duplicate-key detection to ensure each child produces a
// unique resource identity.
func resourceCacheKey(apiVersion, kind, namespace, name string) string {
	return apiVersion + "/" + kind + "/" + namespace + "/" + name
}
