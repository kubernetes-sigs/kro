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
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// GraphRevisionGVK is the GVK for the GraphRevision custom resource.
var GraphRevisionGVK = schema.GroupVersionKind{
	Group:   "internal.kro.run",
	Version: "v1alpha1",
	Kind:    "GraphRevision",
}

// Label and annotation keys for revisions and managed resources.
const (
	LabelGraphName       = "internal.kro.run/graph-name"
	LabelGraphNamespace  = "internal.kro.run/graph-namespace"
	LabelGraphGeneration = "internal.kro.run/graph-generation"
	LabelNodeID          = "internal.kro.run/node-id"
	LabelRevisionHash    = "internal.kro.run/hash"
)

// Revision condition types.
const (
	RevisionConditionReady      = "Ready"
	RevisionConditionPropagated = "Propagated"
	RevisionConditionActive     = "Active"
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

	// Build materialized resource list
	resources := make([]any, len(spec.Resources))
	for i, res := range spec.Resources {
		resources[i] = materializeResource(res, graphName, generationStr)
	}

	// Compute content hash over the materialized resources
	contentHash := hashDesiredState(map[string]any{"resources": resources})

	revision := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "internal.kro.run/v1alpha1",
			"kind":       "GraphRevision",
			"metadata": map[string]any{
				"name":      revisionName(graphName, generation),
				"namespace": graphNamespace,
				"labels": map[string]any{
					LabelGraphName:       graphName,
					LabelGraphGeneration: generationStr,
					LabelRevisionHash:    contentHash,
				},
			},
			"spec": map[string]any{
				"resources": resources,
			},
		},
	}

	return revision
}

// materializeResource injects ownership labels and template hash into a
// single resource's template metadata. Returns the resource as a map
// suitable for inclusion in the revision spec.
func materializeResource(res Resource, graphName string, generation string) map[string]any {
	entry := map[string]any{
		"id": res.ID,
	}

	if res.Template != nil {
		tmpl := deepCopyMap(res.Template)
		// Don't inject ownership labels on contribution templates — contributions
		// write to objects someone else owns, and shouldn't claim them with
		// management labels.
		if !isContributionTemplate(tmpl) {
			injectResourceLabels(tmpl, graphName, generation, res.ID)
		}
		entry["template"] = tmpl
	}
	if res.ExternalRef != nil {
		entry["externalRef"] = deepCopyMap(res.ExternalRef)
	}
	if res.ForEach != nil {
		fe := make(map[string]any, len(res.ForEach))
		for k, v := range res.ForEach {
			fe[k] = v
		}
		entry["forEach"] = fe
	}
	if len(res.IncludeWhen) > 0 {
		iw := make([]any, len(res.IncludeWhen))
		for i, s := range res.IncludeWhen {
			iw[i] = s
		}
		entry["includeWhen"] = iw
	}
	if len(res.ReadyWhen) > 0 {
		rw := make([]any, len(res.ReadyWhen))
		for i, s := range res.ReadyWhen {
			rw[i] = s
		}
		entry["readyWhen"] = rw
	}

	return entry
}

// injectResourceLabels stamps ownership labels into a template's metadata.
// Also computes and sets the template-hash annotation.
func injectResourceLabels(tmpl map[string]any, graphName, generation, nodeID string) {
	md, _ := tmpl["metadata"].(map[string]any)
	if md == nil {
		md = map[string]any{}
		tmpl["metadata"] = md
	}

	lbls, _ := md["labels"].(map[string]any)
	if lbls == nil {
		lbls = map[string]any{}
	}
	lbls[LabelGraphName] = graphName
	lbls[LabelGraphGeneration] = generation
	lbls[LabelNodeID] = nodeID
	md["labels"] = lbls

	// Compute template hash from the template content (before adding the
	// hash itself — same pattern as cache.go's hashDesiredState).
	hash := hashDesiredState(tmpl)

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
// The revision's spec.resources has the same structure as Graph spec.resources,
// just with labels/hashes already injected into templates.
func extractRevisionSpec(revision *unstructured.Unstructured) (*GraphSpec, error) {
	spec, ok := revision.Object["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("revision %s: missing spec", revision.GetName())
	}
	rawResources, ok := spec["resources"]
	if !ok {
		return nil, fmt.Errorf("revision %s: missing spec.resources", revision.GetName())
	}
	resources, err := parseResourceList(rawResources)
	if err != nil {
		return nil, fmt.Errorf("revision %s: %w", revision.GetName(), err)
	}
	return &GraphSpec{Resources: resources}, nil
}

// ---------------------------------------------------------------------------
// CRUD helpers
// ---------------------------------------------------------------------------

// createRevision creates a GraphRevision in the cluster with a finalizer.
//
// TODO: The design specifies immutability via CEL validation (self == oldSelf)
// on the spec. This isn't enforced yet because the CRD uses
// XPreserveUnknownFields without validation rules. The controller is the only
// writer, so immutability is de facto enforced, but a kubectl edit could
// mutate a revision. Add CEL validation when switching to a typed CRD.
// See: experimental/docs/design/graph/002-revisions.md § Spec
func createRevision(ctx context.Context, c client.Client, revision *unstructured.Unstructured) error {
	controllerutil.AddFinalizer(revision, finalizer)
	return c.Create(ctx, revision)
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
	req, err := labels.NewRequirement(LabelGraphName, selection.Equals, []string{graphName})
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

// getActiveRevision finds the currently active revision for a Graph.
// Returns nil if no active revision exists.
func getActiveRevision(ctx context.Context, c client.Client, graphName, namespace string) (*unstructured.Unstructured, error) {
	revisions, err := listRevisions(ctx, c, graphName, namespace)
	if err != nil {
		return nil, err
	}
	for _, rev := range revisions {
		if revisionConditionStatus(rev, RevisionConditionActive) == ConditionTrue {
			return rev, nil
		}
	}
	return nil, nil
}

// deleteRevision removes a GraphRevision after removing its finalizer.
func deleteRevision(ctx context.Context, c client.Client, revision *unstructured.Unstructured) error {
	if controllerutil.ContainsFinalizer(revision, finalizer) {
		controllerutil.RemoveFinalizer(revision, finalizer)
		if err := c.Update(ctx, revision); err != nil {
			return fmt.Errorf("removing finalizer from revision %s: %w", revision.GetName(), err)
		}
	}
	return c.Delete(ctx, revision)
}

// ---------------------------------------------------------------------------
// Revision status helpers
// ---------------------------------------------------------------------------

// setRevisionCondition sets a condition on a GraphRevision's status.
func setRevisionCondition(ctx context.Context, c client.Client, revision *unstructured.Unstructured, condType, status, reason, message string) error {
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
	newCondition := map[string]any{
		"type":    condType,
		"status":  status,
		"reason":  reason,
		"message": message,
	}

	// Replace or append the condition
	found := false
	for i, c := range conditions {
		cMap, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cMap["type"] == condType {
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
func revisionConditionStatus(revision *unstructured.Unstructured, condType string) string {
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
		if cMap["type"] == condType {
			s, _ := cMap["status"].(string)
			return s
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

// ---------------------------------------------------------------------------
// Resource diff — for prune tracking across revisions
// ---------------------------------------------------------------------------

// revisionResourceIDs extracts the set of static resource keys from a
// revision's spec. For templates with static names, this returns
// "group/version/kind/name" strings. For templates with CEL expressions in
// names or forEach resources, the key includes the node ID since the actual
// names aren't known until evaluation time.
//
// This is used for diffing resource sets across revisions to determine
// what to prune. Dynamically-named resources (forEach, CEL in name) are
// tracked by the resource keys collected during the DAG walk, not by this
// function.
func revisionResourceIDs(spec *GraphSpec) map[string]bool {
	ids := make(map[string]bool, len(spec.Resources))
	for _, res := range spec.Resources {
		ids[res.ID] = true
	}
	return ids
}

// diffResourceIDs returns resource IDs present in oldIDs but not in newIDs.
func diffResourceIDs(oldIDs, newIDs map[string]bool) []string {
	var removed []string
	for id := range oldIDs {
		if !newIDs[id] {
			removed = append(removed, id)
		}
	}
	sort.Strings(removed)
	return removed
}

// ListRevisionsForTest is a test-facing export of listRevisions.
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
// Resource key tracking for prune
// ---------------------------------------------------------------------------

// resourceKeysByNodeID returns a map from node ID to the resource keys
// (group/version/kind/namespace/name) that were applied for that node.
// This is populated during the DAG walk.
func resourceKeysByNodeID(appliedKeys []string) map[string][]string {
	// Resource keys are "group/version/kind/namespace/name"
	// We can't directly map back to node ID from the key without the
	// node-id label on the resource. This helper exists for documentation;
	// the actual mapping is done during reconciliation using labels.
	result := make(map[string][]string)
	for _, key := range appliedKeys {
		parts := strings.SplitN(key, "/", 5)
		if len(parts) == 5 {
			// Use kind/name as a rough grouping key
			result[parts[2]+"/"+parts[4]] = append(result[parts[2]+"/"+parts[4]], key)
		}
	}
	return result
}
