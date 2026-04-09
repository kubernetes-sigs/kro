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
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
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

	// AnnotationAppliedSet stores the set of resource keys that this revision
	// has written to the cluster.
	AnnotationAppliedSet = "internal.kro.run/applied-set"
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

	// Build materialized node list
	nodes := make([]any, len(spec.Nodes))
	for i, node := range spec.Nodes {
		nodes[i] = materializeNode(node, graphName, generationStr)
	}

	// Compute content hash over the materialized nodes
	contentHash, err := hashDesiredState(map[string]any{"nodes": nodes})
	if err != nil {
		// Non-fatal: empty hash means the first reconcile won't elide the apply.
		contentHash = ""
	}

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
func materializeNode(node Node, graphName string, generation string) map[string]any {
	entry := map[string]any{
		"id": node.ID,
	}

	if node.Template != nil {
		tmpl := deepCopyMap(node.Template)
		shape := DetectShape(tmpl)
		if shape == ShapeOwns {
			injectNodeLabels(tmpl, graphName, generation, node.ID)
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

// injectNodeLabels stamps ownership labels into a template's metadata.
// Also computes and sets the template-hash annotation.
func injectNodeLabels(tmpl map[string]any, graphName, generation, nodeID string) {
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
// Applied set — annotation-based tracking of what keys a revision wrote
// ---------------------------------------------------------------------------

// setAppliedSet writes the applied key set as a JSON annotation on the revision.
func setAppliedSet(ctx context.Context, c client.Client, revision *unstructured.Unstructured, keys []string) error {
	sorted := make([]string, len(keys))
	copy(sorted, keys)
	sort.Strings(sorted)

	data, err := json.Marshal(sorted)
	if err != nil {
		return fmt.Errorf("marshaling applied set: %w", err)
	}

	newValue := string(data)

	latest, err := getRevision(ctx, c, revision.GetName(), revision.GetNamespace())
	if err != nil {
		return fmt.Errorf("re-fetching revision for applied set: %w", err)
	}

	annotations := latest.GetAnnotations()
	if annotations != nil && annotations[AnnotationAppliedSet] == newValue {
		return nil
	}

	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[AnnotationAppliedSet] = newValue
	latest.SetAnnotations(annotations)
	return c.Update(ctx, latest)
}

func getAppliedSet(revision *unstructured.Unstructured) []string {
	annotations := revision.GetAnnotations()
	if annotations == nil {
		return nil
	}
	raw, ok := annotations[AnnotationAppliedSet]
	if !ok || raw == "" {
		return nil
	}
	var keys []string
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		return nil
	}
	return keys
}

// ---------------------------------------------------------------------------
// Skeleton apply — release field ownership without deleting the object
// ---------------------------------------------------------------------------

func skeletonApply(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, namespace, name string, fieldOwner client.FieldOwner, hasStatus bool) error {
	apiVersion := gvk.Group + "/" + gvk.Version
	if gvk.Group == "" {
		apiVersion = gvk.Version
	}
	skeleton := map[string]any{
		"apiVersion": apiVersion,
		"kind":       gvk.Kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
	}

	data, err := json.Marshal(skeleton)
	if err != nil {
		return fmt.Errorf("marshaling skeleton: %w", err)
	}

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(gvk)
	target.SetName(name)
	target.SetNamespace(namespace)
	if err := c.Patch(ctx, target, client.RawPatch(types.ApplyPatchType, data), fieldOwner, client.ForceOwnership); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("skeleton apply for %s/%s: %w", namespace, name, err)
	}

	if hasStatus {
		statusData, err := json.Marshal(skeleton)
		if err != nil {
			return fmt.Errorf("marshaling status skeleton: %w", err)
		}
		statusTarget := &unstructured.Unstructured{}
		statusTarget.SetGroupVersionKind(gvk)
		statusTarget.SetName(name)
		statusTarget.SetNamespace(namespace)
		if err := c.Status().Patch(ctx, statusTarget, client.RawPatch(types.ApplyPatchType, statusData), fieldOwner, client.ForceOwnership); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("skeleton status apply for %s/%s: %w", namespace, name, err)
			}
		}
	}

	return nil
}
