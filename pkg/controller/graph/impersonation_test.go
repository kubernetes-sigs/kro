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

package graph

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
)

func graphWithSA(namespace, sa string) *expv1alpha1.Graph {
	return &expv1alpha1.Graph{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: namespace},
		Spec:       expv1alpha1.GraphSpec{ServiceAccountName: sa},
	}
}

// TestServiceAccountUsername pins the identity kro resolves for a Graph's
// resources: the default ServiceAccount of the Graph's namespace by default, an
// explicit override when set, always resolved in the Graph's own namespace.
func TestServiceAccountUsername(t *testing.T) {
	tests := []struct {
		name string
		g    *expv1alpha1.Graph
		want string
	}{
		{
			name: "default service account confines to graph namespace",
			g:    graphWithSA("team-a", ""),
			want: "system:serviceaccount:team-a:default",
		},
		{
			name: "explicit service account override",
			g:    graphWithSA("team-a", "deployer"),
			want: "system:serviceaccount:team-a:deployer",
		},
		{
			name: "override is resolved in the graph namespace, not elsewhere",
			g:    graphWithSA("team-b", "deployer"),
			want: "system:serviceaccount:team-b:deployer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, serviceAccountUsername(tt.g))
		})
	}
}

// TestExecutorFor_ImpersonationOverride verifies that the ServiceAccount
// override drives which impersonated executor a Graph resolves, that the
// executor is cached per username, and that a distinct namespace/SA builds a
// distinct executor.
func TestExecutorFor_ImpersonationOverride(t *testing.T) {
	base := executor.NewSimple(fake.NewClientBuilder().Build())

	var builtFor []string
	r := &Reconciler{
		Executor: base,
		Impersonation: NewImpersonation(base, func(user string) (client.Client, error) {
			builtFor = append(builtFor, user)
			return fake.NewClientBuilder().Build(), nil
		}, nil),
	}

	// Override ServiceAccount → impersonate system:serviceaccount:team-a:deployer.
	ex1, err := r.executorFor(graphWithSA("team-a", "deployer"))
	require.NoError(t, err)
	assert.NotSame(t, base, ex1, "impersonated executor must not be the base executor")
	require.Equal(t, []string{"system:serviceaccount:team-a:deployer"}, builtFor)

	// Same Graph identity → cached, no new client built.
	ex1b, err := r.executorFor(graphWithSA("team-a", "deployer"))
	require.NoError(t, err)
	assert.Same(t, ex1, ex1b, "same username must return the cached executor")
	require.Len(t, builtFor, 1, "cached username must not rebuild the client")

	// Different namespace → distinct impersonated executor.
	ex2, err := r.executorFor(graphWithSA("team-b", "deployer"))
	require.NoError(t, err)
	assert.NotSame(t, ex1, ex2)
	require.Equal(t, []string{
		"system:serviceaccount:team-a:deployer",
		"system:serviceaccount:team-b:deployer",
	}, builtFor)
}

// TestExecutorFor_DefaultServiceAccount verifies that without an override, a
// Graph impersonates the default ServiceAccount of its namespace.
func TestExecutorFor_DefaultServiceAccount(t *testing.T) {
	base := executor.NewSimple(fake.NewClientBuilder().Build())
	var capturedUser string
	r := &Reconciler{
		Executor: base,
		Impersonation: NewImpersonation(base, func(user string) (client.Client, error) {
			capturedUser = user
			return fake.NewClientBuilder().Build(), nil
		}, nil),
	}

	_, err := r.executorFor(graphWithSA("team-a", ""))
	require.NoError(t, err)
	assert.Equal(t, "system:serviceaccount:team-a:default", capturedUser)
}

// TestTeardownExecutorFor_UsesPersistedIdentity is the #5 regression: teardown
// must resolve the executor from the identity the Graph ACTUALLY applied under
// (Status.AppliedServiceAccount), not the current spec.serviceAccountName —
// otherwise editing that field between apply and delete runs teardown as an
// identity that can no longer see the resources, orphaning them.
func TestTeardownExecutorFor_UsesPersistedIdentity(t *testing.T) {
	base := executor.NewSimple(fake.NewClientBuilder().Build())
	var builtFor []string
	r := &Reconciler{
		Executor: base,
		Impersonation: NewImpersonation(base, func(user string) (client.Client, error) {
			builtFor = append(builtFor, user)
			return fake.NewClientBuilder().Build(), nil
		}, nil),
	}

	// Applied under SA-X, but spec now names SA-Y (edited after apply).
	g := graphWithSA("team-a", "sa-y")
	g.Status.AppliedServiceAccount = "system:serviceaccount:team-a:sa-x"

	_, err := r.teardownExecutorFor(g)
	require.NoError(t, err)
	require.Equal(t, []string{"system:serviceaccount:team-a:sa-x"}, builtFor,
		"teardown must impersonate the persisted applied identity, not the current spec")
}

// TestTeardownExecutorFor_FallsBackToSpec verifies that a Graph with no recorded
// applied identity (never applied, or a pre-field kro version) tears down using
// the current spec identity.
func TestTeardownExecutorFor_FallsBackToSpec(t *testing.T) {
	base := executor.NewSimple(fake.NewClientBuilder().Build())
	var builtFor []string
	r := &Reconciler{
		Executor: base,
		Impersonation: NewImpersonation(base, func(user string) (client.Client, error) {
			builtFor = append(builtFor, user)
			return fake.NewClientBuilder().Build(), nil
		}, nil),
	}

	g := graphWithSA("team-a", "deployer") // no Status.AppliedServiceAccount
	_, err := r.teardownExecutorFor(g)
	require.NoError(t, err)
	require.Equal(t, []string{"system:serviceaccount:team-a:deployer"}, builtFor,
		"with no persisted identity, teardown falls back to the current spec")
}

// TestAppliedIdentity verifies appliedIdentity returns the impersonation
// username only when impersonation is wired, and "" otherwise (so teardown then
// falls back to the spec / base identity).
func TestAppliedIdentity(t *testing.T) {
	base := executor.NewSimple(fake.NewClientBuilder().Build())

	withImp := &Reconciler{
		Executor: base,
		Impersonation: NewImpersonation(base, func(string) (client.Client, error) {
			return fake.NewClientBuilder().Build(), nil
		}, nil),
	}
	assert.Equal(t, "system:serviceaccount:team-a:deployer",
		withImp.appliedIdentity(graphWithSA("team-a", "deployer")))

	noImp := &Reconciler{Executor: base} // Impersonation nil
	assert.Equal(t, "", noImp.appliedIdentity(graphWithSA("team-a", "deployer")),
		"without impersonation, no applied identity is recorded (teardown falls back to spec)")
}

// TestExecutorFor_NoImpersonationFallsBackToBase verifies that when
// impersonation is not wired (e.g. unit tests, or a build that leaves it off),
// the Graph's resources are applied with the base executor / kro identity.
func TestExecutorFor_NoImpersonationFallsBackToBase(t *testing.T) {
	base := executor.NewSimple(fake.NewClientBuilder().Build())
	r := &Reconciler{Executor: base} // Impersonation nil

	ex, err := r.executorFor(graphWithSA("team-a", "deployer"))
	require.NoError(t, err)
	assert.Same(t, base, ex, "without impersonation wired, must use the base executor unchanged")
}

// TestNewImpersonation_CanWatchBoundToIdentity verifies that when a newAuthz
// factory is supplied, each shadow executor gets a CanWatch gate bound to that
// impersonated identity, and that the gate returns the SelfSubjectAccessReview
// decision for the target GVR/verb. This covers the wiring; the real SSAR round
// trip is left to integration.
func TestNewImpersonation_CanWatchBoundToIdentity(t *testing.T) {
	base := executor.NewSimple(fake.NewClientBuilder().Build())

	var authzUser string
	imp := NewImpersonation(base,
		func(string) (client.Client, error) { return fake.NewClientBuilder().Build(), nil },
		func(user string) (authorizationv1client.AuthorizationV1Interface, error) {
			authzUser = user
			cs := k8sfake.NewSimpleClientset()
			// Allow only "watch configmaps"; deny everything else.
			cs.PrependReactor("create", "selfsubjectaccessreviews",
				func(action clienttesting.Action) (bool, runtime.Object, error) {
					ssar := action.(clienttesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
					ra := ssar.Spec.ResourceAttributes
					ssar.Status.Allowed = ra != nil && ra.Verb == "watch" && ra.Resource == "configmaps"
					return true, ssar, nil
				})
			return cs.AuthorizationV1(), nil
		})

	r := &Reconciler{Executor: base, Impersonation: imp}
	ex, err := r.executorFor(graphWithSA("team-a", "deployer"))
	require.NoError(t, err)
	assert.Equal(t, "system:serviceaccount:team-a:deployer", authzUser,
		"the authz factory must be built for the impersonated identity")

	shadow, ok := ex.(*executor.Simple)
	require.True(t, ok, "the impersonated executor must be a *Simple")
	require.NotNil(t, shadow.CanWatch, "a CanWatch gate must be bound when newAuthz is supplied")

	allowed, err := shadow.CanWatch(context.Background(),
		schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "team-a")
	require.NoError(t, err)
	assert.True(t, allowed, "watching configmaps must be permitted for this identity")

	allowed, err = shadow.CanWatch(context.Background(),
		schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}, "")
	require.NoError(t, err)
	assert.False(t, allowed, "watching a GVR the SA cannot read must be denied")
}

// TestExecutorFor_CacheIsBounded verifies the impersonated-executor cache does
// not grow without limit as distinct (user-controlled) ServiceAccount
// identities churn through it: building more than maxImpersonationCacheEntries
// distinct identities keeps the cache at its bound, and an evicted identity is
// transparently rebuilt on the next lookup.
func TestExecutorFor_CacheIsBounded(t *testing.T) {
	base := executor.NewSimple(fake.NewClientBuilder().Build())
	builds := 0
	r := &Reconciler{
		Executor: base,
		Impersonation: NewImpersonation(base, func(string) (client.Client, error) {
			builds++
			return fake.NewClientBuilder().Build(), nil
		}, nil),
	}

	// Churn more distinct identities than the cache can hold.
	n := maxImpersonationCacheEntries + 50
	for i := range n {
		_, err := r.executorFor(graphWithSA(fmt.Sprintf("ns-%d", i), "deployer"))
		require.NoError(t, err)
	}
	require.Equal(t, n, builds, "each distinct identity builds once on first use")
	assert.Equal(t, maxImpersonationCacheEntries, r.Impersonation.byUser.Len(),
		"the cache must stay bounded at maxImpersonationCacheEntries")

	// The first identity was evicted (LRU), so looking it up again rebuilds it.
	_, err := r.executorFor(graphWithSA("ns-0", "deployer"))
	require.NoError(t, err)
	assert.Equal(t, n+1, builds, "an evicted identity is transparently rebuilt on next use")
}
