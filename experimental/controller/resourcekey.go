// resourcekey.go defines the applied-set key format for managed resources.
//
// Resource keys are identity-only strings: group/version/Kind/namespace/name.
// Dispatch metadata (NodeType, HasStatus) travels in the Applied struct
// alongside the key, separating resource identity from cleanup semantics.
//
// Per 003-ownership.md § Applied Set.
package graphcontroller

import (
	"strings"
	"sync"

	"github.com/gobuffalo/flect"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
)

// scopeResolver reports whether a GVK is namespace-scoped, using a RESTMapper.
// Results are cached: RESTMapping is stable for the lifetime of a GVK's
// registration, so repeat lookups per reconcile are wasteful. A miss (e.g.,
// CRD not yet installed) is NOT cached — the next reconcile may succeed.
//
// A nil *scopeResolver is safe — IsNamespaced returns (false, false),
// preserving the pre-fix substitution heuristic for callers that don't
// have access to a RESTMapper.
type scopeResolver struct {
	mapper meta.RESTMapper
	mu     sync.RWMutex
	cache  map[schema.GroupVersionKind]bool // value = isNamespaced; absence = unknown
}

// newScopeResolver wraps a RESTMapper. Returns nil if mapper is nil —
// callers should treat nil as "no scope info available" and fall back
// to the substitution heuristic.
func newScopeResolver(mapper meta.RESTMapper) *scopeResolver {
	if mapper == nil {
		return nil
	}
	return &scopeResolver{
		mapper: mapper,
		cache:  map[schema.GroupVersionKind]bool{},
	}
}

func (r *scopeResolver) IsNamespaced(gvk schema.GroupVersionKind) (bool, bool) {
	if r == nil {
		return false, false
	}
	r.mu.RLock()
	v, ok := r.cache[gvk]
	r.mu.RUnlock()
	if ok {
		return v, true
	}
	if r.mapper == nil {
		return false, false
	}
	mapping, err := r.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return false, false
	}
	isNamespaced := mapping.Scope != nil && mapping.Scope.Name() == meta.RESTScopeNameNamespace
	r.mu.Lock()
	r.cache[gvk] = isNamespaced
	r.mu.Unlock()
	return isNamespaced, true
}

// defaultNamespace returns the namespace for a resource. For cluster-scoped
// kinds it returns "". For namespaced kinds with no namespace specified, it
// returns fallbackNamespace. When scope is nil or the kind is unknown, the
// fallback is used when ns is empty (preserving the pre-fix substitution heuristic).
func defaultNamespace(gvk schema.GroupVersionKind, ns string, fallback string, scope *scopeResolver) string {
	if scope != nil {
		if isNS, known := scope.IsNamespaced(gvk); known {
			if !isNS {
				return "" // cluster-scoped — always empty
			}
			if ns == "" {
				return fallback
			}
			return ns
		}
	}
	// Scope unknown — fall back to substitution heuristic.
	if ns == "" {
		return fallback
	}
	return ns
}

// Applied identifies a resource the controller has written to.
// Key is identity (group/version/Kind/namespace/name); NodeType and HasStatus
// are cleanup dispatch metadata — NodeType determines delete vs release,
// HasStatus determines whether release must also target the status subresource.
type Applied struct {
	Key       string
	NodeType  graphpkg.NodeType
	HasStatus bool
}

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
func staticResourceKey(tmpl map[string]any, fallbackNamespace string, scope *scopeResolver) string {
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
	// Treat dynamic namespace expressions as "no namespace specified" —
	// they can't be resolved statically.
	if strings.Contains(ns, "${") {
		ns = ""
	}
	ns = defaultNamespace(gvk, ns, fallbackNamespace, scope)
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

// extractStaticKeysFromRevisions collects static resource keys from revision specs.
// Each revision's nodes are iterated, filtering out finalizer, Ref, and Watch nodes.
// Used by both teardown (delete.go) and prune (controller.go) to discover
// managed resources from revision specs.
func extractStaticKeysFromRevisions(revisions []*unstructured.Unstructured, namespace string, scope *scopeResolver) map[string]Applied {
	keys := map[string]Applied{}
	for _, rev := range revisions {
		spec, err := extractRevisionSpec(rev)
		if err != nil {
			continue
		}
		for _, node := range spec.Nodes {
			if node.Finalizes != "" {
				continue
			}
			nodeType := node.Type()
			if nodeType == graphpkg.NodeTypeRef || nodeType == graphpkg.NodeTypeWatch {
				continue
			}
			if node.Identity() == nil {
				continue
			}
			if key := staticResourceKey(node.Identity(), namespace, scope); key != "" {
				keys[key] = Applied{
					Key:       key,
					NodeType:  nodeType,
					HasStatus: templateHasStatus(node.Payload()),
				}
			}
		}
	}
	return keys
}

// nodeHasStatusSubresource reports whether a graph node's body declares a
// non-nil status field. This is an apply-time concern: the controller uses
// it to decide whether to split SSA applies into main + status subresource
// patches.
func nodeHasStatusSubresource(n *graphpkg.Node) bool {
	return templateHasStatus(n.Payload())
}

// templateHasStatus returns true if a template map contains a non-nil
// status field. Used during teardown to determine whether release apply
// must also release the status subresource.
func templateHasStatus(tmpl map[string]any) bool {
	s, ok := tmpl["status"]
	return ok && s != nil
}

// gvkToGVR converts a GVK to a GVR using English pluralization rules.
// Uses flect.Pluralize for correct handling of irregular plurals
// (e.g., NetworkPolicy → networkpolicies, Ingress → ingresses).
func gvkToGVR(gvk schema.GroupVersionKind) schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: flect.Pluralize(strings.ToLower(gvk.Kind)),
	}
}
