// scope.go determines whether a GVK is namespace-scoped or cluster-scoped.
//
// Per 003-ownership.md § Priority Resolution: "Cluster-scoped resources use
// empty string for the namespace component." This matters for applied-set
// key equality — resourceKey(liveObj) returns "" for cluster-scoped
// resources (the API server strips namespace on cluster-scoped responses),
// so staticResourceKey(template, ...) must do the same. Without a scope
// lookup, staticResourceKey substitutes the Graph's namespace and produces
// a key that can never match the post-apply resourceKey — finalizer
// relationships silently fail and prune/teardown miss cluster-scoped
// resources.
package graphcontroller

import (
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GVKScopeResolver reports whether a GVK is namespace-scoped.
//
// IsNamespaced returns (isNamespaced, known). When known is false the caller
// must fall back to heuristics — the resolver has not observed this GVK or
// lookup failed. A resolver with no backing mapper (e.g., tests) always
// returns (false, false), preserving the pre-fix substitution heuristic.
type GVKScopeResolver interface {
	IsNamespaced(gvk schema.GroupVersionKind) (isNamespaced bool, known bool)
}

// restMapperGVKScopeResolver answers IsNamespaced queries using a RESTMapper.
// Results are cached: RESTMapping is stable for the lifetime of a GVK's
// registration, so repeat lookups per reconcile are wasteful. A miss (e.g.,
// CRD not yet installed) is NOT cached — the next reconcile may succeed.
type restMapperGVKScopeResolver struct {
	mapper meta.RESTMapper
	mu     sync.RWMutex
	cache  map[schema.GroupVersionKind]bool // value = isNamespaced; absence = unknown
}

// newRESTMapperGVKScopeResolver wraps a RESTMapper. Returns nil if mapper is
// nil — callers should treat nil as "no scope info available" and fall back
// to the substitution heuristic.
func newRESTMapperGVKScopeResolver(mapper meta.RESTMapper) *restMapperGVKScopeResolver {
	if mapper == nil {
		return nil
	}
	return &restMapperGVKScopeResolver{
		mapper: mapper,
		cache:  map[schema.GroupVersionKind]bool{},
	}
}

func (r *restMapperGVKScopeResolver) IsNamespaced(gvk schema.GroupVersionKind) (bool, bool) {
	if r == nil || r.mapper == nil {
		return false, false
	}
	r.mu.RLock()
	v, ok := r.cache[gvk]
	r.mu.RUnlock()
	if ok {
		return v, true
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
