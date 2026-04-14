// labels.go defines the identity label scheme for managed resources.
//
// Each managed resource carries two labels per Graph-node pair. The label
// key is a DNS subdomain that encodes the node ID, graph name, and namespace:
//
//	<nodeID>.<graphName>.<namespace>.internal.kro.run/reference  → "own" | "contribute"
//	<nodeID>.<graphName>.<namespace>.internal.kro.run/generation → graph.metadata.generation
//
// The identity label key is unique per node-graph-namespace triple. Multiple
// Graphs targeting the same resource coexist without collision — each Graph's
// labels use its own key prefix. See 004-graph-execution.md § Storage model.
//
// DNS subdomain format (253-character limit) requires that graph names, node
// IDs, and namespaces are DNS labels (no dots). Parsing is unambiguous.
package graphcontroller

import (
	"fmt"
	"strings"
)

// maxLabelPrefixLength is the Kubernetes limit for the prefix part of a
// label key (the DNS subdomain before the /). Per RFC 1123: max 253.
const maxLabelPrefixLength = 253

const (
	// identityLabelSuffix is the fixed suffix for all identity labels.
	// The full key is: <nodeID>.<graphName>.<namespace>.internal.kro.run/reference
	identityLabelSuffix = ".internal.kro.run/reference"

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
// Format: <nodeID>.<graphName>.<namespace>.internal.kro.run/reference
// All segments are lowercased to satisfy the RFC 1123 subdomain requirement
// for Kubernetes label key prefixes.
func identityLabelKey(nodeID, graphName, namespace string) string {
	return nodeLabelPrefix(nodeID, graphName, namespace) + identityLabelSuffix
}

// validateIdentityLabelKey checks that the identity label key for a node
// doesn't exceed the Kubernetes label key prefix limit (253 characters).
// Returns an error describing the violation if the key is too long.
func validateIdentityLabelKey(nodeID, graphName, namespace string) error {
	key := identityLabelKey(nodeID, graphName, namespace)
	// The prefix is everything before the /
	if idx := strings.Index(key, "/"); idx > maxLabelPrefixLength {
		return fmt.Errorf("identity label key prefix for node %q exceeds the %d-character DNS subdomain limit (%d characters: %s)",
			nodeID, maxLabelPrefixLength, idx, key[:idx])
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

// parseIdentityLabel extracts the node ID, graph name, and namespace from an
// identity label key. Returns ok=false if the key doesn't match the format.
func parseIdentityLabel(key string) (nodeID, graphName, namespace string, ok bool) {
	if !strings.HasSuffix(key, identityLabelSuffix) {
		return "", "", "", false
	}
	prefix := strings.TrimSuffix(key, identityLabelSuffix)
	parts := strings.SplitN(prefix, ".", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
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
func hasOtherGraphIdentityLabel(labels map[string]string, myGraphName, myNamespace string) (otherGraph string, found bool) {
	mySuffix := graphLabelSuffix(myGraphName, myNamespace)
	for key, val := range labels {
		if !strings.HasSuffix(key, identityLabelSuffix) {
			continue
		}
		// This is an identity label. Check if it belongs to a different graph.
		if !strings.HasSuffix(key, mySuffix) {
			// Different graph. Extract graph name for the error message.
			_, gName, _, ok := parseIdentityLabel(key)
			if ok && (val == ReferenceOwn.String() || val == ReferenceContribute.String()) {
				return gName, true
			}
		}
	}
	return "", false
}

// setIdentityLabels stamps identity and generation labels onto a resource's
// metadata labels map. Called during apply for Own and Contribute references.
// Panics if ref does not have a label value — this is an invariant violation,
// as all call sites pass ReferenceOwn or ReferenceContribute directly.
func setIdentityLabels(labels map[string]string, nodeID, graphName, namespace, generation string, ref Reference) map[string]string {
	lv, ok := ref.LabelValue()
	if !ok {
		panic(fmt.Sprintf("setIdentityLabels called with non-writable reference %s", ref))
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
// Per 004-graph-execution.md § Child Identity:
//
//	<parentID>.<name>.<namespace>.<kind>.<group>.<graph>.<graphns>.internal.kro.run/reference
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
// Panics if ref does not have a label value — this is an invariant violation,
// as all call sites pass ReferenceOwn or ReferenceContribute directly.
func setForEachChildIdentityLabels(labels map[string]string, parentID, resName, resNamespace, kind, group, graphName, graphNamespace, generation string, ref Reference) map[string]string {
	lv, ok := ref.LabelValue()
	if !ok {
		panic(fmt.Sprintf("setForEachChildIdentityLabels called with non-writable reference %s", ref))
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
	NodeID    string
	Reference Reference // ReferenceOwn or ReferenceContribute
	Key       string    // resource key (group/version/Kind/namespace/name)
}
