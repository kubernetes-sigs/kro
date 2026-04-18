// labels.go defines the identity label scheme for managed resources.
//
// Each managed resource carries two labels per Graph-node pair. The label
// key is a DNS subdomain that encodes the node ID, graph name, and namespace:
//
//	<nodeID>.<graphName>.<namespace>.internal.kro.run/type  → "template" | "patch"
//	<nodeID>.<graphName>.<namespace>.internal.kro.run/generation → graph.metadata.generation
//
// The identity label key is unique per node-graph-namespace triple. Multiple
// Graphs targeting the same resource coexist without collision — each Graph's
// labels use its own key prefix. See 004-graph-reconciliation.md § API Server Interaction.
//
// DNS subdomain format (253-character limit) requires that graph names, node
// IDs, and namespaces are DNS labels (no dots). Parsing is unambiguous.
package graphcontroller

import (
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

// maxLabelPrefixLength is the Kubernetes limit for the prefix part of a
// label key (the DNS subdomain before the /). Per RFC 1123: max 253.
const maxLabelPrefixLength = 253

// maxLabelValueLength is the Kubernetes limit for label values (63 bytes).
const maxLabelValueLength = 63

// labelSafeGraphName returns a graph name suitable for use as a label
// value. If the name is ≤63 bytes it's returned unchanged. Otherwise it's
// truncated and suffixed with an fnv32a hex hash — the same hash family
// pkg/controller/resourcegraphdefinition uses for GraphRevision names —
// to preserve a human-readable prefix while guaranteeing uniqueness.
func labelSafeGraphName(name string) string {
	if len(name) <= maxLabelValueLength {
		return name
	}
	h := fnv.New32a()
	h.Write([]byte(name))
	hash := hex.EncodeToString(h.Sum(nil)) // 8 hex chars
	// Reserve 9 chars for "-" + 8-char hash.
	return name[:maxLabelValueLength-9] + "-" + hash
}

const (
	// identityLabelSuffix is the fixed suffix for all identity labels.
	// The full key is: <nodeID>.<graphName>.<namespace>.internal.kro.run/type
	identityLabelSuffix = ".internal.kro.run/type"

	// generationLabelSuffix is the fixed suffix for generation labels.
	generationLabelSuffix = ".internal.kro.run/generation"

	// Flat labels for revision objects (not managed resources).
	// Revisions are namespace-scoped alongside their parent Graph and use
	// flat labels for simple selection. These are NOT the identity labels.
	LabelRevisionGraphName = "internal.kro.run/graph-name"
	LabelGraphGeneration   = "internal.kro.run/graph-generation"
)

// nodeLabelPrefix returns the DNS subdomain prefix shared by identity and
// generation labels for a node in a graph.
// Format: <nodeID>.<graphName>.<namespace>
func nodeLabelPrefix(nodeID, graphName, namespace string) string {
	return strings.ToLower(nodeID) + "." + strings.ToLower(graphName) + "." + strings.ToLower(namespace)
}

// identityLabelKey returns the identity label key for a node in a graph.
// Format: <nodeID>.<graphName>.<namespace>.internal.kro.run/type
// All segments are lowercased to satisfy the RFC 1123 subdomain requirement
// for Kubernetes label key prefixes.
func identityLabelKey(nodeID, graphName, namespace string) string {
	return nodeLabelPrefix(nodeID, graphName, namespace) + identityLabelSuffix
}

// validateIdentityLabelKey checks that the identity label key for a node
// produces a valid Kubernetes label key. The prefix (everything before the /)
// must be a valid DNS-1123 subdomain: [a-z0-9.-], max 253 characters.
// Returns an error if the prefix exceeds the length limit or contains
// characters invalid in DNS subdomains (e.g., underscores, spaces).
func validateIdentityLabelKey(nodeID, graphName, namespace string) error {
	prefix := nodeLabelPrefix(nodeID, graphName, namespace)
	if len(prefix) > maxLabelPrefixLength {
		return fmt.Errorf("identity label key prefix for node %q exceeds the %d-character DNS subdomain limit (%d characters: %s)",
			nodeID, maxLabelPrefixLength, len(prefix), prefix)
	}
	if errs := validation.IsDNS1123Subdomain(prefix); len(errs) > 0 {
		return fmt.Errorf("node %q produces invalid DNS subdomain in label key prefix %q: %s",
			nodeID, prefix, strings.Join(errs, "; "))
	}
	return nil
}

// generationLabelKey returns the generation label key for a node in a graph.
// Format: <nodeID>.<graphName>.<namespace>.internal.kro.run/generation
func generationLabelKey(nodeID, graphName, namespace string) string {
	return nodeLabelPrefix(nodeID, graphName, namespace) + generationLabelSuffix
}

// graphLabelSuffix returns the suffix shared by all identity labels for a graph.
// Used to scan informer caches for the applied set.
func graphLabelSuffix(graphName, namespace string) string {
	return "." + strings.ToLower(graphName) + "." + strings.ToLower(namespace) + identityLabelSuffix
}

// parseNodeIDFromLabel extracts the leading node ID segment from an identity
// label key. For non-forEach nodes (nodeID.graphName.namespace.internal.kro.run/type),
// the node ID is the first dot-separated segment. For forEach children
// (parentID.name.namespace.kind.group.graphName.graphNamespace.internal.kro.run/type),
// the first segment is the parent ID — which is the correct routing target.
//
// This function intentionally does NOT return graphName or namespace because
// the variable-length prefix makes those fields ambiguous for forEach children.
// Callers needing graph identity should use isGraphIdentityLabel or
// graphLabelSuffix matching instead.
func parseNodeIDFromLabel(key string) (nodeID string, ok bool) {
	if !strings.HasSuffix(key, identityLabelSuffix) {
		return "", false
	}
	prefix := strings.TrimSuffix(key, identityLabelSuffix)
	// The node ID (or parent ID for forEach) is the first dot-separated segment.
	dot := strings.IndexByte(prefix, '.')
	if dot <= 0 {
		return "", false // no dot or empty first segment
	}
	return prefix[:dot], true
}

// graphNameFromLabel extracts the graph name from an identity label key
// by parsing from the right side of the prefix (the graph identity suffix).
// The suffix structure is always .<graphName>.<namespace>.internal.kro.run/*,
// regardless of whether the label is for a regular node or a forEach child.
func graphNameFromLabel(key string) string {
	if !strings.HasSuffix(key, identityLabelSuffix) {
		return ""
	}
	prefix := strings.TrimSuffix(key, identityLabelSuffix)
	// The last two segments are graphName and namespace (right to left).
	// Split and count from the end.
	parts := strings.Split(prefix, ".")
	if len(parts) < 3 {
		return "" // need at least nodeID.graphName.namespace
	}
	return parts[len(parts)-2] // second-to-last is graphName
}

// isGraphIdentityLabel checks if a label key is an identity label for the
// specified graph. Used by the kro label check and applied set derivation.
func isGraphIdentityLabel(key, graphName, namespace string) bool {
	return strings.HasSuffix(strings.ToLower(key), graphLabelSuffix(graphName, namespace))
}

// hasGraphIdentityLabels checks if a label map already contains any identity
// labels for the specified graph. Used by applySSA to skip identity
// label stamping when the caller (e.g., forEach) has already set them.
func hasGraphIdentityLabels(labels map[string]string, graphName, namespace string) bool {
	suffix := graphLabelSuffix(graphName, namespace)
	for key := range labels {
		if strings.HasSuffix(strings.ToLower(key), suffix) {
			return true
		}
	}
	return false
}

// hasOtherGraphIdentityLabel checks if a resource's labels contain any
// identity labels from a DIFFERENT graph than the specified one. Used by
// the kro label check before applying an Own template — if present,
// another kro Graph manages this resource.
//
// Label keys are compared case-insensitively. stamping writes lowercase keys
// (DNS-1123 requires lowercase), but the Kubernetes API preserves whatever
// case the client sent — an externally-authored label with mixed case would
// escape detection if we relied on exact-match. isGraphIdentityLabel and
// isIdentityLabel both lowercase; this function matches that invariant.
func hasOtherGraphIdentityLabel(labels map[string]string, myGraphName, myNamespace string) (otherGraph string, found bool) {
	mySuffix := graphLabelSuffix(myGraphName, myNamespace)
	for key, val := range labels {
		lowerKey := strings.ToLower(key)
		if !strings.HasSuffix(lowerKey, identityLabelSuffix) {
			continue
		}
		// This is an identity label. Check if it belongs to a different graph.
		if !strings.HasSuffix(lowerKey, mySuffix) {
			// Different graph. Extract graph name for the error message.
			if val == NodeTypeTemplate.String() || val == NodeTypePatch.String() {
				return graphNameFromLabel(lowerKey), true
			}
		}
	}
	return "", false
}

// setIdentityLabels stamps identity and generation labels onto a resource's
// metadata labels map. Called during apply for Template and Patch node types.
// Panics if nodeType does not have a label value — this is an invariant
// violation, as all call sites pass NodeTypeTemplate or NodeTypePatch
// directly.
func setIdentityLabels(labels map[string]string, nodeID, graphName, namespace, generation string, nodeType NodeType) map[string]string {
	lv, ok := nodeType.LabelValue()
	if !ok {
		panic(fmt.Sprintf("setIdentityLabels called with non-writable node type %s", nodeType))
	}
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[identityLabelKey(nodeID, graphName, namespace)] = lv
	labels[generationLabelKey(nodeID, graphName, namespace)] = generation
	return labels
}

// forEachChildLabelPrefix returns the DNS subdomain prefix shared by forEach
// child identity and generation labels.
// Format: <parentID>.<name>.<namespace>.<kind>[.<group>].<graph>.<graphns>
func forEachChildLabelPrefix(parentID, resName, resNamespace, kind, group, graphName, graphNamespace string) string {
	segments := []string{
		strings.ToLower(parentID),
		strings.ToLower(resName),
		strings.ToLower(resNamespace),
		strings.ToLower(kind),
	}
	if group != "" {
		segments = append(segments, strings.ToLower(group))
	}
	segments = append(segments, strings.ToLower(graphName), strings.ToLower(graphNamespace))
	return strings.Join(segments, ".")
}

// forEachChildIdentityLabelKey returns the identity label key for a forEach child.
// Per 004-graph-reconciliation.md § Child Identity:
//
//	<parentID>.<name>.<namespace>.<kind>.<group>.<graph>.<graphns>.internal.kro.run/type
//
// This encodes the full resource key as DNS subdomain labels within the label key,
// making each child uniquely identifiable in the applied set.
func forEachChildIdentityLabelKey(parentID, resName, resNamespace, kind, group, graphName, graphNamespace string) string {
	return forEachChildLabelPrefix(parentID, resName, resNamespace, kind, group, graphName, graphNamespace) + identityLabelSuffix
}

// forEachChildGenerationLabelKey returns the generation label key for a forEach child.
func forEachChildGenerationLabelKey(parentID, resName, resNamespace, kind, group, graphName, graphNamespace string) string {
	return forEachChildLabelPrefix(parentID, resName, resNamespace, kind, group, graphName, graphNamespace) + generationLabelSuffix
}

// setForEachChildIdentityLabels stamps forEach child identity and generation labels.
// Panics if nodeType does not have a label value — this is an invariant
// violation, as all call sites pass NodeTypeTemplate or NodeTypePatch
// directly.
func setForEachChildIdentityLabels(labels map[string]string, parentID, resName, resNamespace, kind, group, graphName, graphNamespace, generation string, nodeType NodeType) map[string]string {
	lv, ok := nodeType.LabelValue()
	if !ok {
		panic(fmt.Sprintf("setForEachChildIdentityLabels called with non-writable node type %s", nodeType))
	}
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[forEachChildIdentityLabelKey(parentID, resName, resNamespace, kind, group, graphName, graphNamespace)] = lv
	labels[forEachChildGenerationLabelKey(parentID, resName, resNamespace, kind, group, graphName, graphNamespace)] = generation
	return labels
}

// appliedEntry represents a resource in the applied set, derived from the
// watch cache by scanning identity labels.
type appliedEntry struct {
	NodeID   string
	NodeType NodeType // NodeTypeTemplate or NodeTypePatch
	Key      string   // resource key (group/version/Kind/namespace/name)
}

// stampForEachChildLabels stamps identity labels on a forEach child object.
// Handles nil label maps, GVK extraction, and generation formatting.
func stampForEachChildLabels(childObj *unstructured.Unstructured, parentID, graphName, graphNamespace string, generation int64, nodeType NodeType) {
	gvk := childObj.GroupVersionKind()
	gv, _ := schema.ParseGroupVersion(childObj.GetAPIVersion())
	lbls := childObj.GetLabels()
	if lbls == nil {
		lbls = map[string]string{}
	}
	lbls = setForEachChildIdentityLabels(
		lbls, parentID,
		childObj.GetName(), childObj.GetNamespace(),
		gvk.Kind, gv.Group,
		graphName, graphNamespace,
		fmt.Sprintf("%d", generation), nodeType,
	)
	childObj.SetLabels(lbls)
}
