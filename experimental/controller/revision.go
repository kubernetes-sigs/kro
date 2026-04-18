// revision.go implements GraphRevision — an immutable, namespace-scoped
// snapshot of a Graph's spec.
//
// Every structural mutation to a Graph (spec generation bump) produces a new
// GraphRevision. The revision contains the Graph's node declarations as
// authored — CEL ${...} expressions are preserved as-is. Ownership labels and
// template hashes are injected at apply time, not at snapshot time.
//
// A revision can only be created if the spec compiles successfully (CEL +
// DAG). Its existence proves the spec was structurally valid at creation time.
//
// The controller reconciles managed resources from the active revision, not
// the Graph spec directly. This decouples the operational truth (what the
// controller is converging toward) from the authoring surface (what the user
// wrote).
package graphcontroller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// GraphRevisionGVK is the GVK for the GraphRevision custom resource.
var GraphRevisionGVK = schema.GroupVersionKind{
	Group:   "experimental.kro.run",
	Version: "v1alpha1",
	Kind:    "GraphRevision",
}

// NOTE: Identity labels for managed resources are defined in labels.go.
// The constants below are for revision objects only (flat labels for selection).

// RevisionConditionType identifies a condition on a GraphRevision.
type RevisionConditionType string

// Revision condition types.
const (
	RevisionConditionReady  RevisionConditionType = "Ready"
	RevisionConditionActive RevisionConditionType = "Active"
)

// ---------------------------------------------------------------------------
// Naming
// ---------------------------------------------------------------------------

// revisionName builds the name for a GraphRevision from its parent Graph
// name and generation. Format: {graph-name}-g{generation:05d}
func revisionName(graphName string, generation int64) string {
	return fmt.Sprintf("%s-g%05d", graphName, generation)
}

// ---------------------------------------------------------------------------
// Materialization
// ---------------------------------------------------------------------------

// materialize produces a GraphRevision from a Graph and its parsed spec.
// The revision is a spec snapshot — CEL ${...} expressions are preserved
// as-is. Ownership labels and template hashes are injected at apply time,
// not at materialization time.
func materialize(graph *unstructured.Unstructured, spec *GraphSpec) *unstructured.Unstructured {
	graphName := graph.GetName()
	graphNamespace := graph.GetNamespace()
	generation := graph.GetGeneration()
	generationStr := strconv.FormatInt(generation, 10)

	// Build node list — a direct snapshot of the Graph spec nodes.
	nodes := make([]any, len(spec.Nodes))
	for i, node := range spec.Nodes {
		nodes[i] = snapshotNode(node)
	}

	revision := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "GraphRevision",
			"metadata": map[string]any{
				"name":      revisionName(graphName, generation),
				"namespace": graphNamespace,
				"labels": map[string]any{
					LabelRevisionGraphName: labelSafeGraphName(graphName),
					LabelGraphGeneration:   generationStr,
				},
				"ownerReferences": []any{
					map[string]any{
						"apiVersion": graph.GetAPIVersion(),
						"kind":       graph.GetKind(),
						"name":       graphName,
						"uid":        string(graph.GetUID()),
					},
				},
			},
			"spec": map[string]any{
				"nodes": nodes,
			},
		},
	}

	return revision
}

// snapshotNode copies a node's declaration into the revision spec.
// No labels or hashes are injected — the revision is a pure spec snapshot.
// Writes the keyword-specific key (template / patch / ref / watch /
// def); the value may be a map (static body) or a string (CEL expression
// for the body-producing classifications). Ref/Watch are always maps.
// The revision re-parses with the same classification.
func snapshotNode(node Node) map[string]any {
	entry := map[string]any{
		"id": node.ID,
	}

	if node.TemplateExpr != "" {
		// CEL-as-whole-body: the value under the classification keyword
		// is a string. ExprKeyword tells us which keyword to use.
		switch node.ExprKeyword {
		case NodeTypeTemplate:
			entry["template"] = node.TemplateExpr
		case NodeTypePatch:
			entry["patch"] = node.TemplateExpr
		case NodeTypeDef:
			entry["def"] = node.TemplateExpr
		default:
			// Parser constrains ExprKeyword to exactly {Template, Patch, Def}
			// when TemplateExpr is set. Reaching here means an invariant violation.
			panic(fmt.Sprintf("snapshotNode: unexpected ExprKeyword %d for node %q", node.ExprKeyword, node.ID))
		}
	} else {
		switch node.Type() {
		case NodeTypeTemplate:
			if node.Template != nil {
				entry["template"] = deepCopyMap(node.Template)
			}
		case NodeTypePatch:
			if node.Patch != nil {
				entry["patch"] = deepCopyMap(node.Patch)
			}
		case NodeTypeRef:
			if node.Ref != nil {
				entry["ref"] = deepCopyMap(node.Ref)
			}
		case NodeTypeWatch:
			if node.Watch != nil {
				entry["watch"] = deepCopyMap(node.Watch)
			}
		case NodeTypeDef:
			if node.Def != nil {
				entry["def"] = deepCopyMap(node.Def)
			}
		}
	}
	if node.ForEach != nil {
		entry["forEach"] = map[string]any{node.ForEach.VarName: node.ForEach.Expr}
	}
	if node.Finalizes != "" {
		entry["finalizes"] = node.Finalizes
	}
	if len(node.IncludeWhen) > 0 {
		entry["includeWhen"] = stringsToAny(node.IncludeWhen)
	}
	if len(node.ReadyWhen) > 0 {
		entry["readyWhen"] = stringsToAny(node.ReadyWhen)
	}
	if len(node.PropagateWhen) > 0 {
		entry["propagateWhen"] = stringsToAny(node.PropagateWhen)
	}

	return entry
}

// ---------------------------------------------------------------------------
// Parsing — extract GraphSpec from a revision's spec
// ---------------------------------------------------------------------------

// extractRevisionSpec parses a GraphSpec from a GraphRevision object.
// The revision's spec.nodes has the same structure as Graph spec.nodes.
func extractRevisionSpec(revision *unstructured.Unstructured) (*GraphSpec, error) {
	spec, ok := revision.Object["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("revision %s: missing spec", revision.GetName())
	}
	rawNodes, ok := spec["nodes"]
	if !ok {
		return nil, fmt.Errorf("revision %s: missing spec.nodes", revision.GetName())
	}
	nodes, err := parseNodeList(rawNodes)
	if err != nil {
		return nil, fmt.Errorf("revision %s: %w", revision.GetName(), err)
	}
	return &GraphSpec{Nodes: nodes}, nil
}

// ---------------------------------------------------------------------------
// CRUD helpers
// ---------------------------------------------------------------------------

// createRevision creates a GraphRevision in the cluster.
// The spec is immutable — enforced by CEL validation (self == oldSelf) on
// the GraphRevision CRD. See: experimental/docs/design/002-revisions.md
//
// Revisions are freely deletable. On controller startup, hydrateWatchCaches
// pre-populates watch informers from all existing revisions, so the prune
// phase can reconstruct the applied set for cross-GVR transitions even
// after a controller restart — without requiring revisions to be pinned in
// the API server via a finalizer.
func createRevision(ctx context.Context, c client.Client, revision *unstructured.Unstructured) error {
	return c.Create(ctx, revision)
}

// deleteRevision removes a GraphRevision from the cluster. It is a direct
// delete — revisions carry no finalizer and GC will handle ownerReference
// cleanup if the parent Graph is already gone.
func deleteRevision(ctx context.Context, c client.Client, revision *unstructured.Unstructured) error {
	// Ignore NotFound: GC may have already deleted the revision via
	// ownerReference cascade when the parent Graph was deleted.
	return client.IgnoreNotFound(c.Delete(ctx, revision))
}

// getRevision fetches a specific GraphRevision by name.
func getRevision(ctx context.Context, c client.Client, name, namespace string) (*unstructured.Unstructured, error) {
	rev := &unstructured.Unstructured{}
	rev.SetGroupVersionKind(GraphRevisionGVK)
	err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, rev)
	if err != nil {
		return nil, err
	}
	return rev, nil
}

// listRevisions returns all GraphRevisions for a given Graph, ordered by
// generation (ascending). Uses the graph-name label for selection.
func listRevisions(ctx context.Context, c client.Client, graphName, namespace string) ([]*unstructured.Unstructured, error) {
	req, err := labels.NewRequirement(LabelRevisionGraphName, selection.Equals, []string{labelSafeGraphName(graphName)})
	if err != nil {
		return nil, fmt.Errorf("building label selector: %w", err)
	}
	selector := labels.NewSelector().Add(*req)

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(GraphRevisionGVK)
	if err := c.List(ctx, list, &client.ListOptions{
		Namespace:     namespace,
		LabelSelector: selector,
	}); err != nil {
		return nil, fmt.Errorf("listing revisions: %w", err)
	}

	result := make([]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		result[i] = &list.Items[i]
	}

	// Sort by generation (from label)
	sort.Slice(result, func(i, j int) bool {
		gi := revisionGeneration(result[i])
		gj := revisionGeneration(result[j])
		return gi < gj
	})

	return result, nil
}

// ---------------------------------------------------------------------------
// Revision status helpers
// ---------------------------------------------------------------------------

// setRevisionCondition sets a condition on a GraphRevision's status.
// lastTransitionTime is set to now when the condition status changes;
// it is preserved when the status is unchanged — matching the Kubernetes
// condition convention (design 001-graph § Conditions).
func setRevisionCondition(ctx context.Context, c client.Client, revision *unstructured.Unstructured, condType RevisionConditionType, status ConditionStatus, reason, message string) error {
	// Get fresh copy to avoid conflicts
	latest, err := getRevision(ctx, c, revision.GetName(), revision.GetNamespace())
	if err != nil {
		return err
	}

	existingStatus, _ := latest.Object["status"].(map[string]any)
	if existingStatus == nil {
		existingStatus = map[string]any{}
	}

	conditions, _ := existingStatus["conditions"].([]any)
	newCondition := buildCondition(string(condType), status, reason, message)
	preserveTransitionTime(conditions, newCondition, status)

	// Replace or append the condition.
	found := false
	for i, cond := range conditions {
		cMap, ok := cond.(map[string]any)
		if !ok {
			continue
		}
		if cMap["type"] == string(condType) {
			conditions[i] = newCondition
			found = true
			break
		}
	}
	if !found {
		conditions = append(conditions, newCondition)
	}

	existingStatus["conditions"] = conditions
	latest.Object["status"] = existingStatus

	return c.Status().Update(ctx, latest)
}

// revisionConditionStatus reads a condition's status from a revision.
// Returns empty string if the condition is not found.
func revisionConditionStatus(revision *unstructured.Unstructured, condType RevisionConditionType) ConditionStatus {
	status, _ := revision.Object["status"].(map[string]any)
	if status == nil {
		return ""
	}
	conditions, _ := status["conditions"].([]any)
	for _, c := range conditions {
		cMap, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cMap["type"] == string(condType) {
			s, _ := cMap["status"].(string)
			return ConditionStatus(s)
		}
	}
	return ""
}

// revisionGeneration extracts the graph generation from a revision's labels.
func revisionGeneration(revision *unstructured.Unstructured) int64 {
	lbls := revision.GetLabels()
	if lbls == nil {
		return 0
	}
	genStr, ok := lbls[LabelGraphGeneration]
	if !ok {
		return 0
	}
	gen, err := strconv.ParseInt(genStr, 10, 64)
	if err != nil {
		return 0
	}
	return gen
}

// pickEffectiveGeneration returns the generation to stamp on identity labels
// during this reconcile. When the current-generation spec compiled cleanly,
// the graph's live generation is what's being converged — stamp that.
// When compilation failed and the reconciler fell back to the prior revision,
// the labels must reflect that revision's generation, not the failed one —
// otherwise labels lie about which generation materialized the resource.
//
// Per 004-graph-reconciliation.md § API Server Interaction: identity labels
// are "the receipt of what was applied." On a fallback reconcile, what's
// being applied is the active revision; the receipt must match.
func pickEffectiveGeneration(graph, activeRevision *unstructured.Unstructured, compilationErr error) int64 {
	if compilationErr != nil && activeRevision != nil {
		return revisionGeneration(activeRevision)
	}
	return graph.GetGeneration()
}

// ListRevisionsForTest exports listRevisions for the test package
// (package graphcontroller_test). Necessary because the tests are in
// a separate package for black-box testing.
func ListRevisionsForTest(ctx context.Context, c client.Client, graphName, namespace string) ([]*unstructured.Unstructured, error) {
	return listRevisions(ctx, c, graphName, namespace)
}

// ---------------------------------------------------------------------------
// Deep copy utilities
// ---------------------------------------------------------------------------

// deepCopyMap creates a deep copy of a map[string]any.
func deepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	result := make(map[string]any, len(m))
	for k, v := range m {
		result[k] = deepCopyValue(v)
	}
	return result
}

func deepCopyValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return deepCopyMap(val)
	case []any:
		result := make([]any, len(val))
		for i, item := range val {
			result[i] = deepCopyValue(item)
		}
		return result
	default:
		return v // primitives are immutable
	}
}

// ---------------------------------------------------------------------------
// Phase 1: Revision management
// ---------------------------------------------------------------------------

// ensureRevision guarantees that a GraphRevision exists for the current Graph
// generation. Returns the active revision to reconcile from, and all
// superseded revisions for prune diffing.
//
// Per the design (004-graph-reconciliation): the prune candidate set is the union
// of all superseded revisions' applied sets minus the active revision's
// applied set. Returning all superseded revisions (not just the most recent)
// prevents multi-hop transitions from orphaning resources.
//
// On first reconcile (no revisions exist): creates revision, returns it as active.
// On spec change (new generation): creates new revision, returns it as active
// and all older revisions as superseded.
// On steady state: returns existing active revision, nil superseded.
func (r *GraphReconciler) ensureRevision(ctx context.Context, graph *unstructured.Unstructured) (active *unstructured.Unstructured, superseded []*unstructured.Unstructured, err error) {
	logger := log.FromContext(ctx)
	graphName := graph.GetName()
	namespace := graph.GetNamespace()
	generation := graph.GetGeneration()

	// Check if a revision already exists for this generation
	revName := revisionName(graphName, generation)
	existing, err := getRevision(ctx, r.Client, revName, namespace)
	if err == nil {
		// Revision exists for this generation. Collect superseded revisions.
		superseded = r.findSupersededRevisions(ctx, graphName, namespace, generation)
		return existing, superseded, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, nil, fmt.Errorf("checking revision %s: %w", revName, err)
	}

	// No revision for this generation. Parse, compile, and create one.
	// If compilation fails, no revision is created — the failure is reported
	// on the Graph. A revision can only exist if processing succeeded.
	graphSpec, err := extractGraphSpec(graph.Object)
	if err != nil {
		return nil, nil, fmt.Errorf("extracting graph spec: %w", err)
	}

	// Compile to verify validity before creating the revision.
	// Phase 1-2: resolve types (I/O) before pure compilation.
	typeInfo := resolveNodeTypes(graphSpec.Nodes, r.SchemaResolver)
	_, err = compileGraphSpec(graphSpec, typeInfo)
	if err != nil {
		return nil, nil, err
	}

	// Validate that identity label keys won't exceed the DNS subdomain limit.
	// This is a compile-time check — same inputs always produce the same length.
	for _, node := range graphSpec.Nodes {
		if err := validateIdentityLabelKey(node.ID, graphName, namespace); err != nil {
			return nil, nil, fmt.Errorf("label key too long: %w", err)
		}
	}

	// Materialize the revision
	revision := materialize(graph, graphSpec)
	if err := createRevision(ctx, r.Client, revision); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Race: another reconcile created it. Fetch and use it.
			existing, getErr := getRevision(ctx, r.Client, revName, namespace)
			if getErr != nil {
				return nil, nil, fmt.Errorf("fetching existing revision after race: %w", getErr)
			}
			superseded = r.findSupersededRevisions(ctx, graphName, namespace, generation)
			return existing, superseded, nil
		}
		return nil, nil, fmt.Errorf("creating revision %s: %w", revName, err)
	}
	logger.Info("created revision", "revision", revName, "generation", generation)

	// Collect superseded revisions for prune diffing
	superseded = r.findSupersededRevisions(ctx, graphName, namespace, generation)

	// Re-fetch the revision to get the server-assigned metadata
	active, err = getRevision(ctx, r.Client, revName, namespace)
	if err != nil {
		return nil, nil, fmt.Errorf("re-fetching revision %s: %w", revName, err)
	}

	return active, superseded, nil
}

// findSupersededRevisions returns all revisions for a Graph with generation
// less than the current generation. These are the revisions whose applied
// sets must be unioned to compute the prune candidate set.
func (r *GraphReconciler) findSupersededRevisions(ctx context.Context, graphName, namespace string, currentGen int64) []*unstructured.Unstructured {
	revisions, err := listRevisions(ctx, r.Client, graphName, namespace)
	if err != nil {
		return nil
	}

	var result []*unstructured.Unstructured
	for _, rev := range revisions {
		if revisionGeneration(rev) < currentGen {
			result = append(result, rev)
		}
	}
	return result
}

// diffRevisionNodes compares the node specs of two revisions and returns the
// set of node IDs that changed (or are new). Unchanged nodes retain their
// previous state and are not triggered on revision transition.
//
// Per 004-graph-reconciliation.md § Revision transition: "Nodes that differ are
// triggered." The comparison covers all fields that affect a node's behavior:
// Template, IncludeWhen, ReadyWhen, PropagateWhen, ForEach, Finalizes.
//
// Returns nil if the old revision's spec cannot be parsed (fall back to
// triggering all nodes).
func diffRevisionNodes(active *GraphSpec, superseded []*unstructured.Unstructured) map[string]bool {
	if len(superseded) == 0 {
		return nil
	}
	// Use the most recent superseded revision as the baseline.
	// listRevisions returns sorted by generation ascending, so the last
	// superseded element is the one immediately before the active.
	baseline := superseded[len(superseded)-1]
	oldSpec, err := extractRevisionSpec(baseline)
	if err != nil {
		return nil // parse failure — fall back to trigger all
	}

	// Build a map of old node ID → snapshot hash.
	oldHashes := make(map[string]string, len(oldSpec.Nodes))
	for _, node := range oldSpec.Nodes {
		snap := snapshotNode(node)
		h, err := hashDesiredState(snap)
		if err != nil {
			return nil // hash failure — fall back to trigger all
		}
		oldHashes[node.ID] = h
	}

	// Compare each active node against the old snapshot.
	changed := make(map[string]bool)
	for _, node := range active.Nodes {
		snap := snapshotNode(node)
		h, err := hashDesiredState(snap)
		if err != nil {
			return nil // hash failure — fall back to trigger all
		}
		oldHash, existed := oldHashes[node.ID]
		if !existed || h != oldHash {
			// New node or changed node — must be triggered.
			changed[node.ID] = true
		}
	}
	return changed
}

//   - Instance state: keyed by namespace/revision-name (per-Graph mutable state)
//   - Compiled graph: keyed by spec content hash (shared across identical specs)
//
// For N identical child graphs (common in nested graph patterns with forEach),
// this means 1 compilation + N-1 hash lookups instead of N compilations.
func (r *GraphReconciler) compileRevision(revision *unstructured.Unstructured) (*GraphSpec, *instanceState, error) {
	instanceKey := revision.GetNamespace() + "/" + revision.GetName()

	// Fast path: instance state already exists with a valid compilation
	// (steady-state reconcile).
	if existing := r.Caches.get(instanceKey); existing != nil && existing.compiled != nil {
		return existing.compiled.spec, existing, nil
	}

	// Parse the spec.
	spec, err := extractRevisionSpec(revision)
	if err != nil {
		return nil, nil, err
	}

	// Check for a shared compiled graph by spec hash.
	specHash := spec.Hash()
	compiled := r.Caches.getCompiled(specHash)
	if compiled == nil {
		// No shared compiled graph — compile from scratch.
		compiled, err = compileGraphSpec(spec, resolveNodeTypes(spec.Nodes, r.SchemaResolver))
		if err != nil {
			return nil, nil, err
		}
	}

	// Check if this is an evicted instance (compiled was nil'd by
	// evictUnresolved). The per-node mutable state is valid across
	// type-resolution recompilation — only the compiled pointer needs
	// to be replaced and runtime caches reset.
	//
	// Safety invariant: schema resolution does not affect template evaluation.
	// CEL expressions produce identical output for identical inputs regardless
	// of whether the target GVK schema is resolved. If recompilation ever
	// changes template output semantics (not just validation), this in-place
	// update must be gated on a compilation fingerprint that detects the change.
	if existing := r.Caches.get(instanceKey); existing != nil {
		// existing.compiled is nil (evicted). Recompile in-place.
		existing.compiled = compiled
		// Reset runtime caches that should not survive recompilation.
		// Per-node state (hashes, scopes, references, drift timers,
		// applied keys) is preserved — node structure is unchanged.
		existing.forEachItems = map[string][]any{}
		existing.forEachItemScope = map[string]map[string]any{}
		existing.forEachItemKeys = map[string]map[string][]string{}
		existing.collectionCache = make(map[string][]any)
		existing.collectionDirty = make(map[string]bool)
		existing.nodeReady = make(map[string]bool)
		existing.systemErrorBackoff = make(map[string]time.Duration)
		existing.deferredPruneKeys = nil
		// Ensure the compiled graph is tracked in the content-addressed cache.
		r.Caches.set(instanceKey, existing)
		return spec, existing, nil
	}

	// New instance — create fresh mutable state.
	state := newInstanceState(compiled)
	r.Caches.set(instanceKey, state)
	return spec, state, nil
}

// ---------------------------------------------------------------------------
// Revision status
// ---------------------------------------------------------------------------

// updateRevisionStatus updates the conditions on the active and previous
// revisions based on the reconcile outcome. When the active revision is
// fully ready, superseded revisions whose unique resources have been pruned
// are garbage collected.
//
// GC predicate: a superseded revision is safe to delete when its applied set
// is a subset of the active revision's applied set. If it has resources not
// in the active set, those resources are still being pruned and the old
// revision provides ordering metadata for the prune walk.
// See: experimental/docs/design/002-revisions.md § Lifecycle
func (r *GraphReconciler) updateRevisionStatus(ctx context.Context, active *unstructured.Unstructured, superseded []*unstructured.Unstructured, allReady bool, pruneClean bool) {
	logger := log.FromContext(ctx)

	if allReady {
		// All resources are ready — activate this revision
		if err := setRevisionCondition(ctx, r.Client, active, RevisionConditionReady, ConditionTrue, "Ready", "All resources reconciled"); err != nil {
			logger.V(1).Info("failed to set revision Ready", "error", err)
		}
		if revisionConditionStatus(active, RevisionConditionActive) != ConditionTrue {
			if err := setRevisionCondition(ctx, r.Client, active, RevisionConditionActive, ConditionTrue, "Active", "This is the current revision"); err != nil {
				logger.V(1).Info("failed to set revision Active", "error", err)
			}
			// Deactivate all superseded revisions
			for _, prev := range superseded {
				if err := setRevisionCondition(ctx, r.Client, prev, RevisionConditionActive, ConditionFalse, "Superseded", "Superseded by newer revision"); err != nil {
					logger.V(1).Info("failed to deactivate superseded revision", "error", err, "revision", prev.GetName())
				}
			}
		}

		// GC superseded revisions. When allReady is true and prune
		// completed without error, all superseded revisions are safe to
		// delete: their resources have either been migrated to the active
		// revision or pruned from the cluster.
		if pruneClean {
			for _, prev := range superseded {
				if err := deleteRevision(ctx, r.Client, prev); err != nil {
					logger.V(1).Info("failed to GC superseded revision", "error", err, "revision", prev.GetName())
				} else {
					logger.Info("garbage collected superseded revision", "revision", prev.GetName())
					r.Caches.remove(prev.GetNamespace() + "/" + prev.GetName())
				}
			}
		}
	} else {
		// Resources still converging — mark as not yet ready.
		// Use Unknown (not False) to distinguish "not yet evaluated" from
		// "evaluated and failed." Per the design: Ready starts Unknown,
		// converges to True when fully propagated.
		if err := setRevisionCondition(ctx, r.Client, active, RevisionConditionReady, ConditionUnknown, "Progressing", "Resources not yet fully reconciled"); err != nil {
			logger.V(1).Info("failed to set revision Ready=Unknown", "error", err)
		}
	}
}
