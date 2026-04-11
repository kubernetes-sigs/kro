// labels.go defines the identity label scheme for managed resources.
//
// Each managed resource carries two labels per Graph-node pair. The label
// key is a DNS subdomain that encodes the node ID, graph name, and namespace:
//
//	<nodeID>.<graphName>.<namespace>.internal.kro.run/role        → "owns" | "contributes"
//	<nodeID>.<graphName>.<namespace>.internal.kro.run/generation  → graph.metadata.generation
//
// The identity label key is unique per node-graph-namespace triple. Multiple
// Graphs targeting the same resource coexist without collision — each Graph's
// labels use its own key prefix. See 004-graph-execution.md § Storage model.
//
// DNS subdomain format (253-character limit) requires that graph names, node
// IDs, and namespaces are DNS labels (no dots). Parsing is unambiguous.
package graphcontroller

import (
	"strings"
)

const (
	// identityLabelSuffix is the fixed suffix for all identity labels.
	// The full key is: <nodeID>.<graphName>.<namespace>.internal.kro.run/role
	identityLabelSuffix = ".internal.kro.run/role"

	// generationLabelSuffix is the fixed suffix for generation labels.
	generationLabelSuffix = ".internal.kro.run/generation"

	// RoleOwns indicates the Graph creates and manages the resource.
	RoleOwns = "owns"

	// RoleContributes indicates the Graph writes partial state to the resource.
	RoleContributes = "contributes"

	// Flat labels for revision objects (not managed resources).
	// Revisions are namespace-scoped alongside their parent Graph and use
	// flat labels for simple selection. These are NOT the identity labels.
	LabelRevisionGraphName = "internal.kro.run/graph-name"
	LabelGraphGeneration   = "internal.kro.run/graph-generation"
	LabelRevisionHash      = "internal.kro.run/hash"
)

// identityLabelKey returns the identity label key for a node in a graph.
// Format: <nodeID>.<graphName>.<namespace>.internal.kro.run/role
// All segments are lowercased to satisfy the RFC 1123 subdomain requirement
// for Kubernetes label key prefixes.
func identityLabelKey(nodeID, graphName, namespace string) string {
	return strings.ToLower(nodeID) + "." + strings.ToLower(graphName) + "." + strings.ToLower(namespace) + identityLabelSuffix
}

// generationLabelKey returns the generation label key for a node in a graph.
// Format: <nodeID>.<graphName>.<namespace>.internal.kro.run/generation
func generationLabelKey(nodeID, graphName, namespace string) string {
	return strings.ToLower(nodeID) + "." + strings.ToLower(graphName) + "." + strings.ToLower(namespace) + generationLabelSuffix
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

// hasOtherGraphIdentityLabel checks if a resource's labels contain any
// identity labels from a DIFFERENT graph than the specified one. Used by
// the kro label check before applying an Owns template — if present,
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
			if ok && (val == RoleOwns || val == RoleContributes) {
				return gName, true
			}
		}
	}
	return "", false
}

// setIdentityLabels stamps identity and generation labels onto a resource's
// metadata labels map. Called during apply for Owns and Contribute templates.
func setIdentityLabels(labels map[string]string, nodeID, graphName, namespace, generation, role string) map[string]string {
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[identityLabelKey(nodeID, graphName, namespace)] = role
	labels[generationLabelKey(nodeID, graphName, namespace)] = generation
	return labels
}

// appliedEntry represents a resource in the applied set, derived from the
// watch cache by scanning identity labels.
type appliedEntry struct {
	NodeID    string
	Role      string // RoleOwns or RoleContributes
	Key       string // resource key (group/version/Kind/namespace/name)
	HasStatus bool   // for contributes: whether status subresource was applied
}
