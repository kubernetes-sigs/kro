// foreach.go implements forEach node expansion — stamping a template once per
// item in a collection. Includes item diffing (identity comparison, unchanged
// detection) and the snapshot/merge plumbing for passing forEach state between
// the coordinator and workers.
package graphcontroller

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// snapshotFor builds a worker evaluator for a specific node. The snapshot
// contains the node's dependency data (read-only) and, for forEach nodes,
// the previous forEach state from the instance. The worker writes to its own
// maps — the coordinator merges them back after the worker returns.
func (e *evaluator) snapshotFor(node *Node, state *instanceState) *evaluator {
	snap := make(map[string]any, len(node.Dependencies))
	for depID := range node.Dependencies {
		if v, ok := e.scope[depID]; ok {
			snap[depID] = v
		}
	}

	worker := &evaluator{
		compiled:         e.compiled,
		scope:            snap,
		forEachNewScope:  map[string]map[string]any{},
		forEachNewKeys:   map[string]map[string][]string{},
		forEachNewItems:  map[string][]any{},
		forEachPrevItems: map[string][]any{},
		forEachPrevScope: map[string]map[string]any{},
		forEachPrevKeys:  map[string]map[string][]string{},
	}

	// Copy forEach previous state from the shared instance for this node.
	if node.ForEach != nil && state != nil {
		for varName := range node.ForEach {
			cacheKey := node.ID + "/" + varName
			if items, ok := state.forEachItems[cacheKey]; ok {
				worker.forEachPrevItems[cacheKey] = items
			}
		}
		// Copy per-item state — keyed by node ID in outer map.
		if itemScope, ok := state.forEachItemScope[node.ID]; ok {
			copied := make(map[string]any, len(itemScope))
			for k, v := range itemScope {
				copied[k] = v
			}
			worker.forEachPrevScope[node.ID] = copied
		}
		if itemKeys, ok := state.forEachItemKeys[node.ID]; ok {
			copied := make(map[string][]string, len(itemKeys))
			for k, v := range itemKeys {
				copied[k] = v
			}
			worker.forEachPrevKeys[node.ID] = copied
		}
	}

	return worker
}

// reconcileForEach iterates a collection and stamps the template per item.
// Implements forEach item diffing from design 004: the parent diffs the
// current collection against cached state and only re-evaluates changed items.
//
// driftCorrection bypasses the apply-hash check in child applies.
//
// forEach state is passed in via the evaluator's forEachPrev* fields and
// returned via forEachNew* fields. The coordinator merges the output back
// into the shared cache — workers never touch shared state directly.
func (r *GraphReconciler) reconcileForEach(ctx context.Context, graph *unstructured.Unstructured, node Node, eval *evaluator, watcher *graphWatcher, driftCorrection bool) ([]string, error) {
	logger := log.FromContext(ctx)
	var keys []string

	for varName, collectionExpr := range node.ForEach {
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
			// Per 004-graph-reconciliation.md § forEach: identity must be
			// unique across items. Duplicate identities silently drop one
			// item (map overwrite) — that's data loss, not dedup.
			if _, exists := currentItems[id]; exists {
				return nil, fmt.Errorf("forEach %s: duplicate item identity %q — "+
					"two collection items resolve to the same identity", node.ID, id)
			}
			currentItems[id] = item
			currentOrder = append(currentOrder, id)
		}

		// Build previous identity → item map from the worker's snapshot.
		cacheKey := node.ID + "/" + varName
		prevItems := make(map[string]any)
		if prev, ok := eval.forEachPrevItems[cacheKey]; ok {
			for _, item := range prev {
				id := forEachItemIdentity(item)
				prevItems[id] = item
			}
		}

		// Load per-item previous state from nested maps (keyed by node ID, then item ID).
		prevItemScope := eval.forEachPrevScope[node.ID]
		if prevItemScope == nil {
			prevItemScope = map[string]any{}
		}
		prevItemKeys := eval.forEachPrevKeys[node.ID]
		if prevItemKeys == nil {
			prevItemKeys = map[string][]string{}
		}

		// Prepare output maps for this node.
		newItemScope := make(map[string]any)
		newItemKeys := make(map[string][]string)

		// Diff: identify changed, unchanged, and removed items.
		var allApplied []any
		var childErrors []error                     // track per-child errors for state derivation
		seenResourceKeys := make(map[string]string) // resource key → item identity
		for _, id := range currentOrder {
			item := currentItems[id]
			prevItem, existed := prevItems[id]

			// Skip unchanged items: retain previous applied state.
			if existed && forEachItemUnchanged(prevItem, item) {
				if prevKeys, ok := prevItemKeys[id]; ok {
					keys = append(keys, prevKeys...)
				}
				if prevScope, ok := prevItemScope[id]; ok {
					allApplied = append(allApplied, prevScope)
					// Carry forward to new state.
					newItemScope[id] = prevScope
					newItemKeys[id] = prevItemKeys[id]
					logger.V(2).Info("forEach item unchanged, skipping", "node", node.ID, "item", id)
					continue
				}
				// No previous scope — fall through to evaluate.
			}

			if node.Template == nil {
				continue
			}
			innerScope := copyScope(eval.scope)
			innerScope[varName] = item
			innerEval := eval.withScope(innerScope)

			// Definition forEach: evaluate the template per item, collect
			// values into []any. No resource is created or tracked.
			if DetectReference(node.Template) == ReferenceDefinition {
				evalMap, err := innerEval.toMap(node.Template)
				if err != nil {
					childErrors = append(childErrors, fmt.Errorf("forEach defines %s item: %w", node.ID, err))
					logger.V(1).Info("forEach definition item error", "node", node.ID, "item", id, "error", err)
					continue
				}
				allApplied = append(allApplied, evalMap)
				newItemScope[id] = evalMap
				// No keys — definition nodes have no managed resources.
				continue
			}

			evalMap, err := innerEval.toMap(node.Template)
			if err != nil {
				return nil, fmt.Errorf("forEach %s item: %w", node.ID, err)
			}

			// Stamp forEach child identity labels per 004-graph-execution.md § Child Identity.
			// Each child's label key encodes the full resource key:
			//   <parentID>.<name>.<namespace>.<kind>.<group>.<graph>.<graphns>.internal.kro.run/reference
			childObj := &unstructured.Unstructured{Object: evalMap}
			if childObj.GetNamespace() == "" {
				childObj.SetNamespace(graph.GetNamespace())
			}
			gvk := childObj.GroupVersionKind()

			// Per 004-graph-reconciliation.md § forEach: "Resource keys must be
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

			gv, _ := schema.ParseGroupVersion(childObj.GetAPIVersion())
			generation := fmt.Sprintf("%d", graph.GetGeneration())

			// Resolve Own vs Contribute per child. Each child targets a
			// different resource, so the reference type is item-specific.
			childRef, refErr := r.resolveForEachChildReference(ctx, graph, childObj)
			if refErr != nil {
				childErrors = append(childErrors, fmt.Errorf("forEach %s item: resolving reference for %s: %w", node.ID, childObj.GetName(), refErr))
				logger.V(1).Info("forEach child reference resolution failed", "node", node.ID, "item", id, "error", refErr)
				if prevKeys, ok := prevItemKeys[id]; ok {
					keys = append(keys, prevKeys...)
				}
				continue
			}

			lbls := childObj.GetLabels()
			if lbls == nil {
				lbls = map[string]string{}
			}
			lbls = setForEachChildIdentityLabels(
				lbls, node.ID,
				childObj.GetName(), childObj.GetNamespace(),
				gvk.Kind, gv.Group,
				graph.GetName(), graph.GetNamespace(),
				generation, childRef,
			)
			childObj.SetLabels(lbls)
			evalMap = childObj.Object

			var applied *unstructured.Unstructured
			if childRef == ReferenceContribute {
				applied, err = r.applySSA(ctx, graph, evalMap, watcher, node.ID, ReferenceContribute, driftCorrection)
			} else {
				applied, err = r.applySSA(ctx, graph, evalMap, watcher, node.ID, ReferenceOwn, driftCorrection)
			}
			if err != nil {
				// Per 004-graph-execution.md § Parent State: track per-child errors
				// for proper state aggregation. Don't fail fast on the first error.
				childErrors = append(childErrors, err)
				logger.V(1).Info("forEach child error", "node", node.ID, "item", id, "error", err)
				// Retain previous keys for this item if available
				if prevKeys, ok := prevItemKeys[id]; ok {
					keys = append(keys, prevKeys...)
				}
				continue
			}
			allApplied = append(allApplied, applied.Object)
			var itemKeys []string
			if childRef == ReferenceContribute {
				hasStatus := evalMap["status"] != nil
				itemKeys = []string{contributeKey(applied, hasStatus)}
			} else {
				itemKeys = []string{resourceKey(applied)}
			}
			keys = append(keys, itemKeys...)

			// Record per-item state.
			newItemScope[id] = applied.Object
			newItemKeys[id] = itemKeys
		}

		// Record updated state for coordinator to merge back.
		eval.forEachNewScope[node.ID] = newItemScope
		eval.forEachNewKeys[node.ID] = newItemKeys

		// Record updated collection for next reconcile's diff.
		eval.forEachNewItems[cacheKey] = items

		eval.scope[node.ID] = allApplied

		// Per 004-graph-execution.md § Parent State: derive parent state from children.
		// Error states take precedence over Pending; deterministic errors (Error)
		// take precedence over transient errors (SystemError, Conflict).
		if len(childErrors) > 0 {
			return keys, highestPriorityChildError(childErrors)
		}
	}

	// Check readyWhen per-item: all items must pass for the collection to be Ready.
	// Per 001-graph.md: "For forEach nodes, readyWhen is evaluated per-child —
	// each child checks readyWhen independently using the standard per-node mechanism."
	if len(node.ReadyWhen) > 0 {
		scopeVal := eval.scope[node.ID]
		if scopeVal != nil {
			anyNotReady := false
			for _, applied := range scopeVal.([]any) {
				saved := eval.scope[node.ID]
				eval.scope[node.ID] = applied
				err := eval.checkReadiness(node.ReadyWhen, node.ID)
				eval.scope[node.ID] = saved // restore before branching
				if err != nil {
					if m, ok := applied.(map[string]any); ok {
						m["__ready"] = false
					}
					anyNotReady = true
					// Don't return immediately — mark all items' readiness first
					// so .ready() on the parent reflects accurate per-item state.
					continue
				}
				if m, ok := applied.(map[string]any); ok {
					m["__ready"] = true
				}
			}
			if anyNotReady {
				return keys, ErrWaitingForReadiness
			}
			logger.V(1).Info("all forEach items ready", "node", node.ID)
		}
	} else if scopeVal := eval.scope[node.ID]; scopeVal != nil {
		// No readyWhen — all items are ready on apply
		for _, applied := range scopeVal.([]any) {
			if m, ok := applied.(map[string]any); ok {
				m["__ready"] = true
			}
		}
	}

	return keys, nil
}

// forEachItemIdentity extracts a stable identity from a forEach collection item.
// Uses metadata.name if the item is a Kubernetes object, otherwise falls back
// to a content hash. This ensures collection reordering doesn't cause churn.
func forEachItemIdentity(item any) string {
	if m, ok := item.(map[string]any); ok {
		if md, ok := m["metadata"].(map[string]any); ok {
			if name, ok := md["name"].(string); ok {
				return name
			}
			if uid, ok := md["uid"].(string); ok {
				return uid
			}
		}
		// No metadata — use content hash
		h, err := hashDesiredState(m)
		if err != nil {
			log.Log.V(1).Info("forEach item hash failed, using empty identity", "error", err)
		}
		return h
	}
	// Scalar item — use string representation
	return fmt.Sprintf("%v", item)
}

// forEachItemUnchanged returns true if two forEach items have the same content.
// Uses a content hash comparison to avoid deep equality checks.
func forEachItemUnchanged(prev, current any) bool {
	prevMap, prevOk := prev.(map[string]any)
	currMap, currOk := current.(map[string]any)
	if prevOk && currOk {
		prevHash, err1 := hashDesiredState(prevMap)
		currHash, err2 := hashDesiredState(currMap)
		if err1 != nil || err2 != nil {
			log.Log.V(1).Info("forEach item comparison hash failed, treating as changed", "prevErr", err1, "currErr", err2)
			return false // fail-safe: treat as changed
		}
		return prevHash == currHash
	}
	return fmt.Sprintf("%v", prev) == fmt.Sprintf("%v", current)
}

// resolveForEachChildReference determines whether a forEach child targets a
// pre-existing resource (Contribute) or creates a new one (Own). Each forEach
// child targets a different resource, so the reference type is item-specific.
//
// Force annotation → Own. Otherwise delegates to classifyReference.
// Transient errors (network, server) are propagated — the caller should
// treat this as a per-child error rather than silently claiming ownership.
func (r *GraphReconciler) resolveForEachChildReference(ctx context.Context, graph *unstructured.Unstructured, child *unstructured.Unstructured) (Reference, error) {
	if isForceApply(child) {
		return ReferenceOwn, nil
	}

	return r.classifyReference(ctx, graph, child.GroupVersionKind(), child.GetNamespace(), child.GetName())
}

// highestPriorityChildError returns the highest-priority error from a list
// of forEach child errors. Per 004-graph-execution.md § Parent State:
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
