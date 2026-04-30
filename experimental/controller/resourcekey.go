// resourcekey.go defines the applied-set key format for managed resources.
//
// Keys in the applied set identify resources the controller has written to.
// Two formats:
//
//	Template:   group/version/Kind/namespace/name
//	Patch:      patch:group/version/Kind/namespace/name[+status]
//
// The "patch:" prefix distinguishes resources where cleanup means
// release apply (release field ownership) from resources where cleanup
// means delete. The "+status" suffix marks patches that included
// status subresource fields, so release apply must release both the
// main resource and the status subresource.
//
// Per 003-ownership.md § Applied Set.
package graphcontroller

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
)

// patchKeyPrefix marks applied-set entries whose fields are released (not
// deleted) on prune, corresponding to patch: nodes in the graph spec.
const patchKeyPrefix = "patch:"

// patchStatusSuffix marks that the patch included status subresource fields,
// so release apply must target both the main resource and status subresource.
const patchStatusSuffix = "+status"

func resourceKey(obj *unstructured.Unstructured) string {
	gvk := obj.GroupVersionKind()
	return strings.Join([]string{gvk.Group, gvk.Version, gvk.Kind, obj.GetNamespace(), obj.GetName()}, "/")
}

// staticResourceKey builds a resource key from an unevaluated template's
// metadata fields. Skips templates with CEL expressions in the name
// (can't determine key statically). Uses the template's literal
// metadata.namespace when present; falls back to fallbackNamespace when
// the namespace is absent, empty, or contains ${...} expressions.
// This is the spec-time equivalent of resourceKey — used during prune
// diffing and revision spec scanning where templates haven't been evaluated.
//
// scopeResolver (if non-nil) is consulted to determine whether the kind is
// cluster-scoped. For cluster-scoped kinds the namespace segment is ""
// regardless of fallbackNamespace — matching what resourceKey(obj) produces
// post-apply, where the API server strips the namespace from cluster-scoped
// responses. Without this, cluster-scoped resource keys produced by
// staticResourceKey never match keys produced by resourceKey(liveObj), and
// prune diffing / finalizer lookups silently miss cluster-scoped resources.
// Per 003-ownership.md § Priority Resolution: "Cluster-scoped resources use
// empty string for the namespace component."
//
// When scopeResolver is nil, the old heuristic is preserved for backward
// compat with callers that don't have access to a RESTMapper.
func staticResourceKey(tmpl map[string]any, fallbackNamespace string, scope GVKScopeResolver) string {
	gvk := graphpkg.GVKFromMap(tmpl)
	md, _ := tmpl["metadata"].(map[string]any)
	if md == nil {
		return ""
	}
	name, _ := md["name"].(string)
	if name == "" || strings.Contains(name, "${") {
		return "" // dynamic name — can't determine key statically
	}
	ns, _ := md["namespace"].(string)
	hasDynamicNS := strings.Contains(ns, "${")

	// Scope-aware namespace resolution. For cluster-scoped kinds the
	// namespace segment is always "", matching resourceKey(liveObj).
	if scope != nil && gvk.Kind != "" {
		if isNS, known := scope.IsNamespaced(gvk); known {
			if !isNS {
				ns = ""
			} else if ns == "" || hasDynamicNS {
				ns = fallbackNamespace
			}
			return strings.Join([]string{gvk.Group, gvk.Version, gvk.Kind, ns, name}, "/")
		}
	}
	// Fallback heuristic: substitute fallbackNamespace for empty or dynamic
	// namespace. Correct for namespaced kinds; produces a mismatched key for
	// cluster-scoped kinds when scope is unknown.
	if ns == "" || hasDynamicNS {
		ns = fallbackNamespace
	}
	return strings.Join([]string{gvk.Group, gvk.Version, gvk.Kind, ns, name}, "/")
}

func parseResourceKey(key string) (schema.GroupVersionKind, types.NamespacedName) {
	parts := strings.SplitN(key, "/", 5)
	if len(parts) != 5 {
		return schema.GroupVersionKind{}, types.NamespacedName{}
	}
	return schema.GroupVersionKind{Group: parts[0], Version: parts[1], Kind: parts[2]},
		types.NamespacedName{Namespace: parts[3], Name: parts[4]}
}

// unstructuredFromKey parses a resource key and returns a typed stub suitable
// for Get/Delete. Returns false if the key doesn't parse (empty Kind).
func unstructuredFromKey(key string) (*unstructured.Unstructured, types.NamespacedName, bool) {
	gvk, nn := parseResourceKey(key)
	if gvk.Kind == "" {
		return nil, nn, false
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(nn.Name)
	obj.SetNamespace(nn.Namespace)
	return obj, nn, true
}

// patchKey builds a Patch applied set key.
func patchKey(obj *unstructured.Unstructured, hasStatus bool) string {
	key := patchKeyPrefix + resourceKey(obj)
	if hasStatus {
		key += patchStatusSuffix
	}
	return key
}

// parsePatchKey extracts the resource key and status flag from a
// patch applied set key. Returns ("", false) if not a patch key.
func parsePatchKey(key string) (resKey string, hasStatus bool) {
	if !strings.HasPrefix(key, patchKeyPrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(key, patchKeyPrefix)
	if strings.HasSuffix(rest, patchStatusSuffix) {
		return strings.TrimSuffix(rest, patchStatusSuffix), true
	}
	return rest, false
}

// templateHasStatus returns true if a template map contains a non-nil
// status field. Used during teardown to determine whether release apply
// must also release the status subresource.
func templateHasStatus(tmpl map[string]any) bool {
	s, ok := tmpl["status"]
	return ok && s != nil
}
