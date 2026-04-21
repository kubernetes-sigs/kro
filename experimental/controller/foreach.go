// foreach.go implements forEach node expansion — stamping a template once per
// item in a collection. Includes item diffing (identity comparison, unchanged
// detection) and the snapshot/merge plumbing for passing forEach state between
// the coordinator and workers.
package graphcontroller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// snapshotFor builds a worker evaluator for a specific node. The snapshot
// contains the node's dependency data (read-only) and, for forEach nodes,
// the previous forEach state from the instance. The worker writes to its own
// maps — the coordinator merges them back after the worker returns.
func (e *evaluator) snapshotFor(node *Node, state *instanceState) *evaluator {
	snap := make(map[string]any, len(node.Dependencies)+1)
	for depID := range node.Dependencies {
		if v, ok := e.scope[depID]; ok {
			snap[depID] = v
		}
	}
	// For nodes with zero declared dependencies (e.g., status reporter
	// nodes whose template only uses .ready() calls — which the field
	// path extractor skips), include scope entries for ALL graph nodes
	// so the worker can evaluate .ready() without "no such attribute"
	// errors. Use previous scope (carries __ready stamps from last
	// reconcile) with empty-map fallback for new nodes.
	if len(node.Dependencies) == 0 && e.compiled != nil {
		for nodeID := range e.compiled.topology.Index {
			if _, exists := snap[nodeID]; exists {
				continue
			}
			if v, ok := e.scope[nodeID]; ok {
				snap[nodeID] = v
			} else if prev, ok := state.previousScope[nodeID]; ok {
				snap[nodeID] = prev
			} else {
				snap[nodeID] = map[string]any{}
			}
		}
	}
	// The node-readiness sidecar must be visible to rewritten
	// `<wk_id>.ready()` lookups, including those nested inside CEL
	// comprehensions evaluated by the worker. The worker receives a
	// COPY so its writes don't race with the coordinator or other
	// workers; the coordinator merges the worker's verdict back via
	// nodeResult.nodeReadyUpdate. Readiness is monotonic within a
	// reconcile (a node's verdict is set once), so the copy sees a
	// consistent snapshot.
	var workerReady map[string]bool
	if e.nodeReady != nil {
		workerReady = make(map[string]bool, len(e.nodeReady))
		for k, v := range e.nodeReady {
			workerReady[k] = v
		}
		snap[reservedNodeReadyVar] = workerReady
	}

	// Propagate dynamic GVK tracking to the worker if the compiled graph has
	// dynamic nodes. Each worker gets its own map (no sharing) — the resolved
	// GVK is returned via nodeResult.resolvedGVK.
	var workerDynamicGVK map[string]schema.GroupVersionKind
	if e.dynamicGVKResolved != nil {
		workerDynamicGVK = make(map[string]schema.GroupVersionKind, 1)
	}

	worker := &evaluator{
		compiled:            e.compiled,
		scope:               snap,
		effectiveGeneration: e.effectiveGeneration,
		nodeReady:           workerReady,
		dynamicGVKResolved:  workerDynamicGVK,
		forEachNewScope:     map[string]map[string]any{},
		forEachNewKeys:      map[string]map[string][]string{},
		forEachNewHashes:    map[string]map[string]string{},
		forEachNewItems:     map[string][]any{},
		forEachPrevItems:    map[string][]any{},
		forEachPrevScope:    map[string]map[string]any{},
		forEachPrevKeys:     map[string]map[string][]string{},
		forEachPrevHashes:   map[string]map[string]string{},
	}

	// Copy forEach previous state from the shared instance for this node.
	if node.ForEach != nil && state != nil {
		cacheKey := node.ID + "/" + node.ForEach.VarName
		if items, ok := state.forEachItems[cacheKey]; ok {
			worker.forEachPrevItems[cacheKey] = items
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
		if itemHashes, ok := state.forEachItemHashes[node.ID]; ok {
			copied := make(map[string]string, len(itemHashes))
			for k, v := range itemHashes {
				copied[k] = v
			}
			worker.forEachPrevHashes[node.ID] = copied
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
	// Per-item propagateWhen gate. Per 001-graph.md § propagateWhen:
	// "With forEach, [...] the controller evaluates propagateWhen
	// per-item and halts when the condition is first false."
	//
	// The gate is evaluated before each item against the
	// partially-built collection (eval.scope[node.ID]). Items
	// processed so far have fresh __ready stamps so expressions
	// like ${workers.filter(w, !w.ready()).size() < 2} reflect
	// current readiness. Gated items carry forward their previous
	// applied state (including __ready from the last reconcile).
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
	// Stable iteration order across reconciles. Without this,
	// per-item propagateWhen halts at inconsistent positions when
	// the source collection reorders (e.g., Watch list from an
	// informer cache with non-deterministic iteration).
	sort.Strings(currentOrder)

	// Build previous identity → item map from the worker's snapshot.
	// When cached hashes are available (Layer 1), the previous identity
	// map is still needed for items that changed — hashDesiredState(prevItem)
	// is replaced by the cached hash lookup, but new/removed item detection
	// still needs the identity set.
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
	prevItemHashes := eval.forEachPrevHashes[node.ID]
	if prevItemHashes == nil {
		prevItemHashes = map[string]string{}
	}

	// Prepare output maps for this node.
	newItemScope := make(map[string]any)
	newItemKeys := make(map[string][]string)
	newItemHashes := make(map[string]string, len(currentItems))

	// Compute shared dependency context hash once. Included in each per-item
	// cached hash so that when a shared dep changes but collection items are
	// stable, all cached hashes become stale and items are re-evaluated.
	contextHash := hashForEachContext(eval.scope, node.Dependencies)

	// Diff: identify changed, unchanged, and removed items.
	var allApplied []any
	var childErrors []error                     // track per-child errors for state derivation
	seenResourceKeys := make(map[string]string) // resource key → item identity
	halted := false
	for _, id := range currentOrder {
		// --- Per-item propagateWhen gate ---
		if hasPerItemGate && !halted {
			// Expose the partially-built collection so the gate
			// expression can inspect already-processed items.
			eval.scope[node.ID] = allApplied
			if !eval.checkPropagateWhen(node.PropagateWhen, node.ID) {
				halted = true
				logger.V(1).Info("per-item propagateWhen halted forEach expansion",
					"node", node.ID, "haltedAt", id, "processedCount", len(allApplied))
			}
		}
		if halted {
			// Carry forward: retain previous applied state for gated items.
			// Note: carried-forward items retain __ready from their last
			// processed reconcile. If readyWhen depends on cross-node state
			// that changed, the stamp is stale until the gate opens far
			// enough for this item to be re-processed.
			if prevKeys, ok := prevItemKeys[id]; ok {
				keys = append(keys, prevKeys...)
			}
			if prevScope, ok := prevItemScope[id]; ok {
				allApplied = append(allApplied, prevScope)
				newItemScope[id] = prevScope
				newItemKeys[id] = prevItemKeys[id]
				newItemHashes[id] = prevItemHashes[id] // carry forward hash
				// Re-stamp __updated: carried-forward items retain their
				// generation label from when they were last applied. Compare
				// against effectiveGeneration to determine if this item is
				// on the latest generation. Per 005-reconciliation.md
				// § Propagation Control: gated items on an old generation
				// show updated()=false ("Pending" or "Stuck" state).
				if m, ok := prevScope.(map[string]any); ok {
					m["__updated"] = isForEachItemUpdated(m, node.ID, graph.GetName(), graph.GetNamespace(), eval.effectiveGeneration)
				}
			}
			// If no previous scope (new item, never created), it is
			// absent from the collection — downstream sees a growing
			// list as the gate opens over successive reconciles.
			continue
		}

		item := currentItems[id]
		_, existed := prevItems[id]

		// Incremental skip (Layer 2): when forEachChangedItems is set
		// and this item is NOT in the changed set, carry forward without
		// any hash computation. The coordinator proved that only specific
		// collection items changed and this isn't one of them.
		if eval.forEachChangedItems != nil && !eval.forEachChangedItems[id] && existed {
			if prevScope, ok := prevItemScope[id]; ok {
				if prevKeys, ok := prevItemKeys[id]; ok {
					keys = append(keys, prevKeys...)
				}
				allApplied = append(allApplied, prevScope)
				newItemScope[id] = prevScope
				newItemKeys[id] = prevItemKeys[id]
				newItemHashes[id] = prevItemHashes[id] // carry forward hash
				logger.V(2).Info("forEach item not in changed set, skipping", "node", node.ID, "item", id)
				if m, ok := prevScope.(map[string]any); ok {
					m["__updated"] = isForEachItemUpdated(m, node.ID, graph.GetName(), graph.GetNamespace(), eval.effectiveGeneration)
				}
				if hasPerItemGate {
					stampSingleItemReady(eval, node.ID, prevScope, node.ReadyWhen)
				}
				continue
			}
			// No previous scope — fall through to evaluate.
		}

		// Skip unchanged items: retain previous applied state.
		// Cached hash includes context prefix — when shared deps change,
		// no cached hash matches and all items re-evaluate.
		itemUnchanged := false
		if existed && !driftCorrection {
			cachedHash := prevItemHashes[id]
			if cachedHash != "" {
				itemUnchanged = forEachItemUnchangedCached(cachedHash, item, contextHash)
			} else {
				itemUnchanged = forEachItemUnchanged(prevItems[id], item)
			}
		}
		if itemUnchanged {
			if prevKeys, ok := prevItemKeys[id]; ok {
				keys = append(keys, prevKeys...)
			}
			if prevScope, ok := prevItemScope[id]; ok {
				allApplied = append(allApplied, prevScope)
				// Carry forward to new state.
				newItemScope[id] = prevScope
				newItemKeys[id] = prevItemKeys[id]
				// Carry forward cached hash or bootstrap the cache for
				// future reconciles. The cached hash is already in
				// composite format (contextHash/itemHash) — carry it
				// forward as-is. Re-prefixing would double the context
				// prefix, causing alternating cache hit/miss on every
				// subsequent reconcile.
				if h := prevItemHashes[id]; h != "" {
					newItemHashes[id] = h
				} else if currMap, ok := item.(map[string]any); ok {
					if h, err := hashDesiredState(currMap); err == nil {
						newItemHashes[id] = contextHash + "/" + h
					}
				}
				logger.V(2).Info("forEach item unchanged, skipping", "node", node.ID, "item", id)
				// Re-stamp __updated from generation label. Within the
				// same generation, the label matches → true. Across a
				// generation boundary (recompilation that preserved
				// forEach state), the label won't match → false.
				if m, ok := prevScope.(map[string]any); ok {
					m["__updated"] = isForEachItemUpdated(m, node.ID, graph.GetName(), graph.GetNamespace(), eval.effectiveGeneration)
				}
				// Inline readyWhen stamp: carried-forward items preserve
				// their previous __ready. Re-stamp so the gate expression
				// sees current readiness if readyWhen depends on cross-node
				// state that changed since the last reconcile.
				if hasPerItemGate {
					stampSingleItemReady(eval, node.ID, prevScope, node.ReadyWhen)
				}
				continue
			}
			// No previous scope — fall through to evaluate.
		}

		if !node.HasBody() {
			continue
		}
		innerScope := copyScope(eval.scope)
		innerScope[varName] = item
		innerEval := eval.withScope(innerScope)

		// Definition forEach: evaluate the template per item, collect
		// values into []any. No resource is created or tracked.
		if node.Type() == NodeTypeDef {
			evalMap, err := innerEval.toMapNode(node)
			if err != nil {
				childErrors = append(childErrors, fmt.Errorf("forEach defines %s item: %w", node.ID, err))
				logger.V(1).Info("forEach definition item error", "node", node.ID, "item", id, "error", err)
				continue
			}
			// Definitions are always re-evaluated — vacuously updated.
			evalMap["__updated"] = true
			allApplied = append(allApplied, evalMap)
			newItemScope[id] = evalMap
			// Cache content hash for the collection item.
			if currMap, ok := item.(map[string]any); ok {
				if h, err := hashDesiredState(currMap); err == nil {
					newItemHashes[id] = contextHash + "/" + h
				}
			}
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
			childObj.SetNamespace(graph.GetNamespace())
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

		stampForEachChildLabels(childObj, node.ID, graph.GetName(), graph.GetNamespace(), eval.effectiveGeneration, childNodeType)
		evalMap = childObj.Object

		var applied *unstructured.Unstructured
		if childNodeType == NodeTypePatch {
			applied, err = r.applySSA(ctx, graph, evalMap, watcher, node.ID, NodeTypePatch, eval.effectiveGeneration, driftCorrection, node.Lifecycle.ForceApply())
		} else {
			applied, err = r.applySSA(ctx, graph, evalMap, watcher, node.ID, NodeTypeTemplate, eval.effectiveGeneration, driftCorrection, node.Lifecycle.ForceApply())
		}
		if err != nil {
			// Per 005-reconciliation.md § Parent State: track per-child errors
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
		// Just applied with effectiveGeneration — child is on the latest generation.
		applied.Object["__updated"] = true
		var itemKeys []string
		if childNodeType == NodeTypePatch {
			hasStatus := evalMap["status"] != nil
			itemKeys = []string{patchKey(applied, hasStatus)}
		} else {
			itemKeys = []string{resourceKey(applied)}
		}
		keys = append(keys, itemKeys...)

		// Record per-item state.
		newItemScope[id] = applied.Object
		newItemKeys[id] = itemKeys

		// Cache the content hash of the current collection item (not the
		// applied object). This is the hash used for unchanged detection
		// on the next reconcile — it reflects the collection item, not
		// the template output.
		if currMap, ok := item.(map[string]any); ok {
			if h, err := hashDesiredState(currMap); err == nil {
				newItemHashes[id] = contextHash + "/" + h
			}
		}

		// Inline readyWhen stamp: required when per-item
		// propagateWhen is active so the gate expression sees
		// __ready on items processed earlier in this cycle.
		if hasPerItemGate {
			stampSingleItemReady(eval, node.ID, applied.Object, node.ReadyWhen)
		}
	}

	// Record updated state for coordinator to merge back.
	if eval.forEachNewScope == nil {
		eval.forEachNewScope = map[string]map[string]any{}
	}
	eval.forEachNewScope[node.ID] = newItemScope
	if eval.forEachNewKeys == nil {
		eval.forEachNewKeys = map[string]map[string][]string{}
	}
	eval.forEachNewKeys[node.ID] = newItemKeys
	if eval.forEachNewHashes == nil {
		eval.forEachNewHashes = map[string]map[string]string{}
	}
	eval.forEachNewHashes[node.ID] = newItemHashes

	// Record updated collection for next reconcile's diff.
	if eval.forEachNewItems == nil {
		eval.forEachNewItems = map[string][]any{}
	}
	eval.forEachNewItems[cacheKey] = items

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
		return keys, highestPriorityChildError(childErrors)
	}

	eval.scope[node.ID] = allApplied

	// Per-item propagateWhen halted expansion — the collection is
	// incomplete. Signal NotReady so the controller requeues.
	if halted {
		return keys, ErrWaitingForReadiness
	}

	// Check readyWhen per-item and stamp __ready on each item.
	// When per-item propagateWhen is active, readyWhen was already
	// stamped inline during the loop — skip the post-loop pass.
	if !hasPerItemGate {
		if err := forEachStampReadyWhen(eval.scope, node.ID, node.ReadyWhen, eval); err != nil {
			return keys, err
		}
		if len(node.ReadyWhen) > 0 {
			logger.V(1).Info("all forEach items ready", "node", node.ID)
		}
	}

	return keys, nil
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
			savedSelf := scope["self"]
			scope[nodeID] = applied
			scope["self"] = applied
			err := eval.evalReadinessConditions(readyWhen, nodeID)
			scope[nodeID] = saved
			scope["self"] = savedSelf
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
	// Temporarily point scope[nodeID] and scope["self"] at this single
	// item so readyWhen expressions resolve against the item under
	// evaluation. Per 001-graph.md § readyWhen: "The expression runs in
	// a scope containing the node's current value as self."
	saved := eval.scope[nodeID]
	savedSelf := eval.scope["self"]
	eval.scope[nodeID] = item
	eval.scope["self"] = item
	err := eval.evalReadinessConditions(readyWhen, nodeID)
	eval.scope[nodeID] = saved
	eval.scope["self"] = savedSelf
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
	// Scalar item — use string representation.
	// %v on Go maps and slices is deterministic (sorts map keys as of Go 1.12),
	// and %v on numeric types stringifies float64(5) and int64(5) to the same
	// "5", so CEL type drift does not cause phantom child churn.
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

// forEachItemUnchangedCached returns true if a collection item is unchanged
// from the previous reconcile, using a cached hash for the previous item.
// The cached hash includes a context prefix (shared dependency scope) so
// that when shared dependencies change, no cached hash matches.
func forEachItemUnchangedCached(cachedPrevHash string, current any, contextHash string) bool {
	currMap, currOk := current.(map[string]any)
	if !currOk {
		return strings.HasPrefix(cachedPrevHash, contextHash+"/")
	}
	currHash, err := hashDesiredState(currMap)
	if err != nil {
		return false
	}
	return cachedPrevHash == contextHash+"/"+currHash
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
