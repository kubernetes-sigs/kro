// revision.go implements GraphRevision — an immutable, namespace-scoped
// snapshot of a Graph's materialized resources.
//
// Every structural mutation to a Graph (spec generation bump) produces a new
// GraphRevision. The revision contains fully materialized resource templates
// with ownership labels and per-resource content hashes injected. CEL ${...}
// expressions are preserved as-is — they're evaluated at reconcile time, not
// materialization time.
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
	RevisionConditionReady      RevisionConditionType = "Ready"
	RevisionConditionPropagated RevisionConditionType = "Propagated"
	RevisionConditionActive     RevisionConditionType = "Active"
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
// The revision contains fully materialized resource templates with:
//   - internal.kro.run/graph-name label on each resource
//   - internal.kro.run/graph-generation label on each resource
//   - internal.kro.run/node-id label on each resource
//   - internal.kro.run/template-hash annotation per resource
//
// CEL ${...} expressions are preserved as-is. The revision is the unit of
// compilation — its existence proves the spec was structurally valid.
func materialize(graph *unstructured.Unstructured, spec *GraphSpec) *unstructured.Unstructured {
	graphName := graph.GetName()
	graphNamespace := graph.GetNamespace()
	generation := graph.GetGeneration()
	generationStr := strconv.FormatInt(generation, 10)

	// Build materialized node list
	nodes := make([]any, len(spec.Nodes))
	for i, node := range spec.Nodes {
		nodes[i] = materializeNode(node, graphName, graphNamespace, generationStr)
	}

	// Compute content hash over the materialized nodes
	contentHash, err := hashDesiredState(map[string]any{"nodes": nodes})
	if err != nil {
		// Non-fatal: empty hash means the first reconcile won't elide the apply.
		contentHash = ""
	}

	revision := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "GraphRevision",
			"metadata": map[string]any{
				"name":      revisionName(graphName, generation),
				"namespace": graphNamespace,
				"labels": map[string]any{
					LabelRevisionGraphName: graphName,
					LabelGraphGeneration:   generationStr,
					LabelRevisionHash:      contentHash,
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

// materializeNode injects ownership labels and template hash into a
// single node's template metadata. Returns the node as a map
// suitable for inclusion in the revision spec.
func materializeNode(node Node, graphName, graphNamespace string, generation string) map[string]any {
	entry := map[string]any{
		"id": node.ID,
	}

	if node.Template != nil {
		tmpl := deepCopyMap(node.Template)
		ref := DetectReference(tmpl)
		// Only inject ownership labels for structurally-known Owns references.
		// Unresolved references (Owns vs Contributes unknown until reconcile time)
		// get labels injected at apply time if they resolve to Owns.
		if ref == ReferenceOwns {
			injectNodeLabels(tmpl, graphName, graphNamespace, generation, node.ID)
		}
		entry["template"] = tmpl
	}
	if node.ForEach != nil {
		fe := make(map[string]any, len(node.ForEach))
		for k, v := range node.ForEach {
			fe[k] = v
		}
		entry["forEach"] = fe
	}
	if node.Finalizes != "" {
		entry["finalizes"] = node.Finalizes
	}
	if len(node.IncludeWhen) > 0 {
		iw := make([]any, len(node.IncludeWhen))
		for i, s := range node.IncludeWhen {
			iw[i] = s
		}
		entry["includeWhen"] = iw
	}
	if len(node.ReadyWhen) > 0 {
		rw := make([]any, len(node.ReadyWhen))
		for i, s := range node.ReadyWhen {
			rw[i] = s
		}
		entry["readyWhen"] = rw
	}
	if len(node.PropagateWhen) > 0 {
		pw := make([]any, len(node.PropagateWhen))
		for i, s := range node.PropagateWhen {
			pw[i] = s
		}
		entry["propagateWhen"] = pw
	}

	return entry
}

// injectNodeLabels stamps identity labels into a template's metadata.
// Uses the DNS subdomain identity label scheme from 004-graph-execution.md.
// Also computes and sets the template-hash annotation.
func injectNodeLabels(tmpl map[string]any, graphName, graphNamespace, generation, nodeID string) {
	md, _ := tmpl["metadata"].(map[string]any)
	if md == nil {
		md = map[string]any{}
		tmpl["metadata"] = md
	}

	lbls, _ := md["labels"].(map[string]any)
	if lbls == nil {
		lbls = map[string]any{}
	}
	ownsLabel, _ := ReferenceOwns.LabelValue()
	lbls[identityLabelKey(nodeID, graphName, graphNamespace)] = ownsLabel
	lbls[generationLabelKey(nodeID, graphName, graphNamespace)] = generation
	md["labels"] = lbls

	// Compute template hash from the template content (before adding the
	// hash itself — same pattern as cache.go's hashDesiredState).
	hash, err := hashDesiredState(tmpl)
	if err != nil {
		// Non-fatal: empty hash means the first reconcile won't elide the apply.
		hash = ""
	}

	anns, _ := md["annotations"].(map[string]any)
	if anns == nil {
		anns = map[string]any{}
	}
	anns[templateHashAnnotation] = hash
	md["annotations"] = anns
}

// ---------------------------------------------------------------------------
// Parsing — extract GraphSpec from a revision's spec
// ---------------------------------------------------------------------------

// extractRevisionSpec parses a GraphSpec from a GraphRevision object.
// The revision's spec.nodes has the same structure as Graph spec.nodes,
// just with labels/hashes already injected into templates.
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
	req, err := labels.NewRequirement(LabelRevisionGraphName, selection.Equals, []string{graphName})
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

	now := time.Now().UTC().Format(time.RFC3339)
	conditions, _ := existingStatus["conditions"].([]any)

	newCondition := map[string]any{
		"type":               string(condType),
		"status":             string(status),
		"reason":             reason,
		"message":            message,
		"lastTransitionTime": now,
	}

	// Preserve lastTransitionTime when the status value hasn't changed.
	for _, existing := range conditions {
		eMap, ok := existing.(map[string]any)
		if !ok {
			continue
		}
		if eMap["type"] == string(condType) {
			if eMap["status"] == string(status) {
				if ltt, ok := eMap["lastTransitionTime"].(string); ok && ltt != "" {
					newCondition["lastTransitionTime"] = ltt
				}
			}
			break
		}
	}

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
// Per the design (004-graph-execution): the prune candidate set is the union
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
	_, err = compileGraph(graphSpec)
	if err != nil {
		return nil, nil, err
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

	// Set initial conditions on the new revision
	if err := setRevisionCondition(ctx, r.Client, revision, RevisionConditionPropagated, ConditionTrue, "Propagated", "Controller is reconciling from this revision"); err != nil {
		logger.V(1).Info("failed to set initial Propagated condition", "revision", revName, "error", err)
	}

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

// compileRevision parses and compiles a revision's spec, using two cache layers:
//   - Instance state: keyed by namespace/revision-name (per-Graph mutable state)
//   - Compiled graph: keyed by spec content hash (shared across identical specs)
//
// For N identical child graphs (common in nested graph patterns with forEach),
// this means 1 compilation + N-1 hash lookups instead of N compilations.
func (r *GraphReconciler) compileRevision(revision *unstructured.Unstructured) (*GraphSpec, *instanceState, error) {
	instanceKey := revision.GetNamespace() + "/" + revision.GetName()

	// Fast path: instance state already exists (steady-state reconcile).
	if existing := r.Caches.get(instanceKey); existing != nil {
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
		compiled, err = compileGraphSpec(spec)
		if err != nil {
			return nil, nil, err
		}
	}

	// Create per-instance mutable state pointing to the shared compiled graph.
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
