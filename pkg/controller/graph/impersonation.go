// Copyright 2025 The Kube Resource Orchestrator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// impersonation.go — resolves the identity kro uses to apply a Graph's
// resources. Unlike ResourceGraphDefinition instances (which apply children
// with the kro controller identity), a namespaced Graph applies its resources
// while impersonating a ServiceAccount in the Graph's own namespace. By default
// this is the namespace's default ServiceAccount, confining resource access to
// that namespace; GraphSpec.ServiceAccountName overrides which ServiceAccount
// is used. This keeps a namespaced Graph from escalating beyond a ServiceAccount
// it could already use.

package graph

import (
	"context"
	"fmt"
	"sync"

	lru "github.com/hashicorp/golang-lru/v2"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
)

// defaultServiceAccountName is impersonated when a Graph does not set
// spec.serviceAccountName, confining resource access to the Graph's namespace
// by default.
const defaultServiceAccountName = "default"

// serviceAccountUsername returns the impersonation username for the
// ServiceAccount that should apply g's resources. The ServiceAccount is always
// resolved in the Graph's own namespace, so a Graph can only ever impersonate a
// ServiceAccount it could already use.
func serviceAccountUsername(g *expv1alpha1.Graph) string {
	name := defaultServiceAccountName
	if sa := g.Spec.ServiceAccountName; sa != "" {
		name = sa
	}
	return fmt.Sprintf("system:serviceaccount:%s:%s", g.GetNamespace(), name)
}

// maxImpersonationCacheEntries bounds the impersonated-executor cache. The key
// is a user-controlled ServiceAccount username (system:serviceaccount:<ns>:<sa>),
// so an unbounded map could accrete an entry per distinct SA name a namespace
// ever churns through and never release them. A bounded LRU caps that; an
// evicted executor is cheaply rebuilt on the next reconcile that needs it (a
// REST-client shadow of the base executor). Sized generously so steady-state
// fleets never evict a live identity.
const maxImpersonationCacheEntries = 1024

// impersonationCache memoizes an impersonated executor per username so a
// reconcile does not rebuild a REST client on every loop. Bounded by a
// size-limited LRU (maxImpersonationCacheEntries) so a namespace churning
// ServiceAccount names cannot grow it without limit; an evicted entry is
// rebuilt on demand.
type impersonationCache struct {
	mu      sync.Mutex
	byUser  *lru.Cache[string, executor.Interface]
	newExec func(user string) (executor.Interface, error)
}

// executorFor returns an executor bound to the impersonated identity for g. It
// is derived from the base executor via the reconciler's client factory so it
// inherits the base configuration (apply concurrency). Results are cached per
// impersonation username.
func (r *Reconciler) executorFor(g *expv1alpha1.Graph) (executor.Interface, error) {
	if r.Impersonation == nil || r.Impersonation.newExec == nil {
		// Impersonation not wired (e.g. unit tests): fall back to the base
		// executor and the kro controller identity. Only reachable when
		// RequireImpersonation is false — reconcileGraph refuses the Graph
		// before this point when impersonation is required but not wired.
		return r.Executor, nil
	}
	return r.executorForUser(serviceAccountUsername(g))
}

// appliedIdentity returns the impersonation username the Graph applies under
// this cycle, or "" when impersonation is inactive (the base controller
// identity). Recorded in Status.AppliedServiceAccount on a successful apply so
// teardown can resolve the same identity; empty leaves teardown to fall back to
// the current spec.
func (r *Reconciler) appliedIdentity(g *expv1alpha1.Graph) string {
	if r.Impersonation == nil || r.Impersonation.newExec == nil {
		return ""
	}
	return serviceAccountUsername(g)
}

// teardownExecutorFor resolves the executor for deleting a Graph's resources.
// It uses the identity the Graph ACTUALLY applied under (Status
// .AppliedServiceAccount) so teardown is unaffected by a later edit to
// spec.serviceAccountName. When no applied identity was recorded (the Graph
// never applied, or was last applied by a kro version predating the field), it
// falls back to the current spec via executorFor.
func (r *Reconciler) teardownExecutorFor(g *expv1alpha1.Graph) (executor.Interface, error) {
	if user := g.Status.AppliedServiceAccount; user != "" {
		return r.executorForUser(user)
	}
	return r.executorFor(g)
}

// executorForUser returns the executor bound to a specific impersonation
// username, caching one entry per distinct identity. Teardown uses this with
// the persisted Status.AppliedServiceAccount so it runs under the identity that
// actually applied the resources, not the current spec. Falls back to the base
// executor when impersonation is not wired.
func (r *Reconciler) executorForUser(user string) (executor.Interface, error) {
	if r.Impersonation == nil || r.Impersonation.newExec == nil {
		return r.Executor, nil
	}

	r.Impersonation.mu.Lock()
	defer r.Impersonation.mu.Unlock()
	if r.Impersonation.byUser == nil {
		cache, err := lru.New[string, executor.Interface](maxImpersonationCacheEntries)
		if err != nil {
			return nil, fmt.Errorf("init impersonation cache: %w", err)
		}
		r.Impersonation.byUser = cache
	}
	if ex, ok := r.Impersonation.byUser.Get(user); ok {
		return ex, nil
	}
	ex, err := r.Impersonation.newExec(user)
	if err != nil {
		return nil, fmt.Errorf("build impersonated executor for %q: %w", user, err)
	}
	r.Impersonation.byUser.Add(user, ex)
	return ex, nil
}

// NewImpersonation builds an impersonation cache whose executors are shadows of
// base with an impersonated controller-runtime client. newClient builds a
// controller-runtime client for the given impersonation username (typically via
// client.New over a rest.Config carrying rest.ImpersonationConfig); it is a
// parameter so tests can supply an in-memory client instead of a real REST
// stack.
//
// newAuthz, when non-nil, builds an AuthorizationV1 client from the SAME
// impersonated rest.Config so each shadow executor also gets a CanWatch gate
// bound to that identity. CanWatch issues a SelfSubjectAccessReview (correct
// because the config already impersonates the target ServiceAccount, so "self"
// IS the Graph's SA) for verb=watch on the target GVR; a denied review skips
// the drift watch so a Graph cannot make the controller open a watch on a GVR
// its own SA may not read. newAuthz may be nil (tests / no authz stack), in
// which case CanWatch is left nil and every watch is attempted.
func NewImpersonation(
	base *executor.Simple,
	newClient func(user string) (client.Client, error),
	newAuthz func(user string) (authorizationv1client.AuthorizationV1Interface, error),
) *impersonationCache {
	cache, err := lru.New[string, executor.Interface](maxImpersonationCacheEntries)
	if err != nil {
		// New only errors on a non-positive size, which is a compile-time
		// constant here, so this is unreachable — panic rather than thread an
		// error through every caller.
		panic(fmt.Sprintf("impersonation cache init: %v", err))
	}
	return &impersonationCache{
		byUser: cache,
		newExec: func(user string) (executor.Interface, error) {
			cl, err := newClient(user)
			if err != nil {
				return nil, err
			}
			// Copy the base executor so the shared instance is untouched, then
			// point the copy at the impersonated client.
			shadow := *base
			shadow.Client = cl
			if newAuthz != nil {
				authz, err := newAuthz(user)
				if err != nil {
					return nil, err
				}
				shadow.CanWatch = canWatchFor(authz)
			}
			return &shadow, nil
		},
	}
}

// canWatchFor builds a CanWatch gate that asks whether the identity behind authz
// (already impersonating the Graph's ServiceAccount) may watch the target GVR,
// via a SelfSubjectAccessReview. A review error is returned as-is so the caller
// treats it as an inconclusive skip.
func canWatchFor(authz authorizationv1client.AuthorizationV1Interface) func(context.Context, schema.GroupVersionResource, string) (bool, error) {
	return func(ctx context.Context, gvr schema.GroupVersionResource, namespace string) (bool, error) {
		ssar := &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Verb:      "watch",
					Group:     gvr.Group,
					Version:   gvr.Version,
					Resource:  gvr.Resource,
					Namespace: namespace,
				},
			},
		}
		resp, err := authz.SelfSubjectAccessReviews().Create(ctx, ssar, metav1.CreateOptions{})
		if err != nil {
			return false, err
		}
		return resp.Status.Allowed, nil
	}
}
