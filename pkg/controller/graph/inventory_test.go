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
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	krotruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

func statusPatchFields(t *testing.T, obj client.Object, patch client.Patch) map[string]json.RawMessage {
	t.Helper()
	data, err := patch.Data(obj)
	require.NoError(t, err)
	var body struct {
		Status map[string]json.RawMessage `json:"status"`
	}
	require.NoError(t, json.Unmarshal(data, &body))
	return body.Status
}

func assertConvergenceFailure(t *testing.T, g *expv1alpha1.Graph, reason string) {
	t.Helper()
	converged := findCondition(g.Status.Conditions, ResourcesConverged)
	require.NotNil(t, converged)
	assert.Equal(t, metav1.ConditionFalse, converged.Status)
	assert.Equal(t, &reason, converged.Reason)
	ready := findCondition(g.Status.Conditions, Ready)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
}

func TestReconcile_ProjectsOnlyIncludedNodes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		live            bool
		nested          bool
		included        bool
		expressionError bool
	}{
		{name: "live ref excludes leaves", live: true},
		{name: "live ref includes leaves", live: true, included: true},
		{name: "ignored leaves"},
		{name: "included leaves", included: true},
		{name: "ignored subgraph", nested: true},
		{name: "included subgraph", nested: true, included: true},
		{name: "live ref excludes subgraph", live: true, nested: true},
		{name: "invalid leaf inclusion", expressionError: true},
		{name: "invalid subgraph inclusion", nested: true, expressionError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			condition := fmt.Sprintf("${%t}", tc.included)
			if tc.live {
				condition = `${source.data.enabled == "true"}`
			}
			if tc.expressionError {
				condition = "${1 / 0 > 0}"
			}
			leafCondition := condition
			if tc.nested {
				leafCondition = "${true}"
			}
			children := generator.NewGraph("children",
				generator.WithTemplate("tmpl", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "owned"},
				}),
				generator.WithIncludeWhen(leafCondition),
				generator.WithPatch("p", "v1", "ConfigMap", "target", map[string]any{"data": map[string]any{"added": "yes"}}),
				generator.WithIncludeWhen(leafCondition),
			)
			g := generator.NewGraph("g", generator.WithNamespace("default"),
				generator.WithRef("source", &expv1alpha1.ExternalRef{
					APIVersion: "v1", Kind: "ConfigMap", Metadata: expv1alpha1.ExternalRefMetadata{Name: "flag"},
				}),
			)
			prefix, guardedNode := "", "p"
			if tc.nested {
				// Capturing gate makes the entire subgraph inherit its inclusion state.
				generator.WithDef("capture", map[string]any{"value": "${gate.value}"})(children)
				generator.WithDef("gate", map[string]any{"value": true})(g)
				generator.WithIncludeWhen(condition)(g)
				generator.WithSubgraph("sub", children)(g)
				prefix, guardedNode = "sub/", "sub"
			} else {
				g.Spec.Nodes = append(g.Spec.Nodes, children.Spec.Nodes...)
			}
			g.UID, g.Generation = "uid-inclusion", 1
			withFinalizer(g)
			resolver, disco := testk8s.NewFakeResolver()
			cmp := compiler.NewCompilerWithDependencies(resolver, restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco)))
			prog, err := cmp.Compile(g)
			require.NoError(t, err)
			rt := krotruntime.New(prog, g)
			if tc.live {
				_, err := rt.Node(guardedNode).IsIgnored()
				require.ErrorIs(t, err, krotruntime.ErrDataPending)
			}

			var wantManaged []expv1alpha1.ManagedResource
			var wantContribs []executor.Contribution
			if tc.included {
				wantManaged = []expv1alpha1.ManagedResource{{NodeID: prefix + "tmpl", APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "owned"}}
				wantContribs = []executor.Contribution{{APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "target", FieldManager: executor.PatchFieldManager(g.UID, prefix+"p")}}
			}
			if tc.included && !tc.live {
				assert.Equal(t, wantManaged, intendedManagedResources(rt))
				assert.Equal(t, wantContribs, intendedContributions(rt))
			} else {
				assert.Empty(t, intendedManagedResources(rt))
				assert.Empty(t, intendedContributions(rt))
			}

			base := newClient(t, g)
			ctx := context.Background()
			for _, name := range []string{"flag", "target"} {
				require.NoError(t, base.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": name, "namespace": "default"},
					"data":     map[string]any{"enabled": fmt.Sprint(tc.included)},
				}}))
			}
			var managedWrites, ledgerWrites, resourceWrites int
			cl := interceptor.NewClient(base, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if patch.Type() == types.ApplyPatchType {
						// Stub successful SSA; the real executor evaluates inclusion and records identities.
						resourceWrites++
						return nil
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					fields := statusPatchFields(t, obj, patch)
					if _, present := fields["managedResources"]; sub == "status" && present {
						managedWrites++
					}
					if _, present := fields["contributions"]; sub == "status" && present {
						ledgerWrites++
					}
					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			})
			r := &Reconciler{Client: cl, Compiler: cmp, Registry: registry.New(), Executor: executor.NewSimple(cl)}
			key := client.ObjectKeyFromObject(g)
			for range 2 {
				res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
				if tc.expressionError {
					require.ErrorContains(t, err, "division by zero")
					assert.NotErrorIs(t, err, executor.ErrNotReady)
				} else {
					require.NoError(t, err)
				}
				assert.Zero(t, res.RequeueAfter)
				got := &expv1alpha1.Graph{}
				require.NoError(t, cl.Get(ctx, key, got))
				assert.ElementsMatch(t, wantManaged, got.Status.ManagedResources)
				assert.Equal(t, toAPIContributions(wantContribs), got.Status.Contributions)
				if tc.expressionError {
					assertConvergenceFailure(t, got, "ApplyFailed")
				} else {
					ready := findCondition(got.Status.Conditions, Ready)
					require.NotNil(t, ready)
					assert.Equal(t, metav1.ConditionTrue, ready.Status)
				}
			}
			assert.Equal(t, len(wantManaged), managedWrites, "managed inventory must not grow and shrink each cycle")
			assert.Equal(t, len(wantContribs), ledgerWrites, "contribution inventory must not grow and shrink each cycle")
			assert.Equal(t, 2*(len(wantManaged)+len(wantContribs)), resourceWrites)
		})
	}
}

func TestReconcile_ContributionWriteAheadRefusalBlocksApply(t *testing.T) {
	t.Parallel()
	g := graph("g", withFinalizer)
	g.UID = "uid-inventory"
	want := []executor.Contribution{{APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "target", FieldManager: executor.PatchFieldManager(g.UID, "p")}}
	refused := errors.New("ledger refused")
	reject := true
	cl := interceptor.NewClient(newClient(t, g), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if _, present := statusPatchFields(t, obj, patch)["contributions"]; sub == "status" && present && reject {
				return refused
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	key := client.ObjectKeyFromObject(g)
	obs := &applyObservingExecutor{cl: cl, key: key, fakeExecutor: fakeExecutor{applyResult: executor.ApplyResult{Contributions: want}}}
	r := &Reconciler{Client: cl, Compiler: &fakeCompiler{program: patchProgram("p", "v1", "ConfigMap", "default", "target")}, Registry: registry.New(), Executor: obs}
	ctx, req := context.Background(), ctrl.Request{NamespacedName: key}
	res, err := r.Reconcile(ctx, req)
	require.ErrorIs(t, err, refused)
	assert.Zero(t, res.RequeueAfter)
	assert.False(t, obs.observed, "Apply must wait for durable intent")
	got := &expv1alpha1.Graph{}
	require.NoError(t, cl.Get(ctx, key, got))
	assert.Empty(t, got.Status.Contributions)
	assertConvergenceFailure(t, got, "ApplyFailed")

	reject = false
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	require.True(t, obs.observed)
	assert.Equal(t, toAPIContributions(want), obs.contribsAtApply)
}

func TestReconcile_ObservedContributionPersistenceFailure(t *testing.T) {
	t.Parallel()
	hardErr := errors.New("patch apply failed")
	for _, tc := range []struct {
		name     string
		applyErr error
	}{
		{name: "clean apply"},
		{name: "soft apply", applyErr: executor.ErrNotReady},
		{name: "hard apply", applyErr: hardErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := graph("g", withFinalizer)
			prior := executor.Contribution{APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "prior", FieldManager: "prior-manager"}
			g.Status.Contributions = toAPIContributions([]executor.Contribution{prior})
			observed := prior
			observed.Name = "observed"
			applied := []expv1alpha1.ManagedResource{rUID("owned", "ConfigMap", "owned", "uid-owned")}
			g.Status.ManagedResources = []expv1alpha1.ManagedResource{rUID("owned", "ConfigMap", "owned", "uid-replaced")}
			refused := errors.New("observed ledger refused")
			cl := interceptor.NewClient(newClient(t, g), interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if _, present := statusPatchFields(t, obj, patch)["contributions"]; sub == "status" && present {
						return refused
					}
					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			})
			key := client.ObjectKeyFromObject(g)
			obs := &applyObservingExecutor{cl: cl, key: key, fakeExecutor: fakeExecutor{
				applyErr: tc.applyErr, applyResult: executor.ApplyResult{Contributions: []executor.Contribution{observed}, Applied: applied},
			}}
			r := &Reconciler{Client: cl, Compiler: &fakeCompiler{program: emptyNodeProgram("p")}, Registry: registry.New(), Executor: obs}
			res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			require.ErrorIs(t, err, refused)
			assert.NotErrorIs(t, err, executor.ErrNotReady, "ledger refusal must not become a soft requeue")
			if tc.applyErr == hardErr {
				assert.ErrorIs(t, err, hardErr)
			}
			assert.Zero(t, res.RequeueAfter)
			require.True(t, obs.observed)
			assert.Equal(t, g.Status.Contributions, obs.contribsAtApply, "the observed target was unavailable to projection")
			got := &expv1alpha1.Graph{}
			require.NoError(t, cl.Get(context.Background(), key, got))
			assert.Equal(t, g.Status.Contributions, got.Status.Contributions)
			assert.Equal(t, applied, got.Status.ManagedResources, "ledger refusal must retain observed managed resources")
			assertConvergenceFailure(t, got, "ApplyFailed")
		})
	}
}

func TestReconcile_ContributionsRetainedUntilCleanupSucceeds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		prune      bool
		shrink     bool
		applyErr   error
		wantReason string
	}{
		{name: "release failure", wantReason: "ReleaseFailed"},
		{name: "prune failure", prune: true, wantReason: "PruneFailed"},
		{name: "soft apply prune failure", prune: true, applyErr: executor.ErrNotReady, wantReason: "PruneFailed"},
		{name: "ledger shrink failure", shrink: true, wantReason: "ApplyFailed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := graph("g", withFinalizer)
			g.UID = "uid-inventory"
			prior := executor.Contribution{APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "retired", FieldManager: "prior-manager"}
			intent, observed := prior, prior
			intent.Name, intent.FieldManager = "intended", executor.PatchFieldManager(g.UID, "p")
			observed.Name, observed.FieldManager = "observed", executor.PatchFieldManager(g.UID, "live")
			g.Status.Contributions = toAPIContributions([]executor.Contribution{prior})
			if tc.prune {
				withManagedResource(g)
			}
			failed := errors.New("cleanup failed")
			rejectShrink := tc.shrink
			cl := interceptor.NewClient(newClient(t, g), interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if _, present := statusPatchFields(t, obj, patch)["contributions"]; sub == "status" && present && rejectShrink && len(obj.(*expv1alpha1.Graph).Status.Contributions) == 1 {
						return failed
					}
					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			})
			key := client.ObjectKeyFromObject(g)
			obs := &applyObservingExecutor{cl: cl, key: key, fakeExecutor: fakeExecutor{
				applyErr: tc.applyErr, applyResult: executor.ApplyResult{Contributions: []executor.Contribution{observed}},
			}}
			if tc.prune {
				obs.deleteErr = failed
			} else if !tc.shrink {
				obs.releaseErr = failed
			}
			r := &Reconciler{Client: cl, Compiler: &fakeCompiler{program: patchProgram("p", "v1", "ConfigMap", "default", "intended")}, Registry: registry.New(), Executor: obs}
			ctx, req := context.Background(), ctrl.Request{NamespacedName: key}
			retained := toAPIContributions([]executor.Contribution{prior, intent, observed})
			for cycle := range 2 {
				res, err := r.Reconcile(ctx, req)
				require.ErrorIs(t, err, failed)
				assert.NotErrorIs(t, err, executor.ErrNotReady)
				assert.Zero(t, res.RequeueAfter)
				if cycle == 0 {
					assert.Equal(t, toAPIContributions([]executor.Contribution{prior, intent}), obs.contribsAtApply)
				} else {
					assert.Equal(t, retained, obs.contribsAtApply)
				}
				got := &expv1alpha1.Graph{}
				require.NoError(t, cl.Get(ctx, key, got))
				assert.Equal(t, retained, got.Status.Contributions, "cleanup failures must retain intent and observed-only entries")
				assertConvergenceFailure(t, got, tc.wantReason)
			}
			if tc.prune {
				assert.Len(t, obs.deleteCalls, 2)
				assert.Empty(t, obs.releaseCalls)
			}

			obs.deleteErr, obs.releaseErr, obs.applyErr = nil, nil, nil
			rejectShrink = false
			_, err := r.Reconcile(ctx, req)
			require.NoError(t, err)
			got := &expv1alpha1.Graph{}
			require.NoError(t, cl.Get(ctx, key, got))
			assert.Equal(t, toAPIContributions([]executor.Contribution{observed}), got.Status.Contributions)
			require.NotEmpty(t, obs.releaseCalls)
			assert.Equal(t, []executor.Contribution{prior, intent}, obs.releaseCalls[len(obs.releaseCalls)-1])
		})
	}
}

func TestReconcile_ContributionWriteAheadSurvivesTerminalStatusLossAndDeletion(t *testing.T) {
	t.Parallel()
	g := graph("g", withFinalizer)
	g.UID = "uid-inventory"
	want := []executor.Contribution{{APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "target", FieldManager: executor.PatchFieldManager(g.UID, "p")}}
	lost := errors.New("terminal status write lost")
	cl := interceptor.NewClient(newClient(t, g), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if _, present := statusPatchFields(t, obj, patch)["conditions"]; sub == "status" && present {
				return lost
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	key := client.ObjectKeyFromObject(g)
	obs := &applyObservingExecutor{cl: cl, key: key, fakeExecutor: fakeExecutor{applyResult: executor.ApplyResult{Contributions: want}}}
	r := &Reconciler{Client: cl, Compiler: &fakeCompiler{program: patchProgram("p", "v1", "ConfigMap", "default", "target")}, Registry: registry.New(), Executor: obs}
	ctx, req := context.Background(), ctrl.Request{NamespacedName: key}
	_, err := r.Reconcile(ctx, req)
	require.ErrorIs(t, err, lost)
	require.True(t, obs.observed)
	assert.Equal(t, toAPIContributions(want), obs.contribsAtApply)
	got := &expv1alpha1.Graph{}
	require.NoError(t, cl.Get(ctx, key, got))
	assert.Equal(t, toAPIContributions(want), got.Status.Contributions)
	assert.Empty(t, got.Status.Conditions)

	obs.observed = false
	require.NoError(t, cl.Delete(ctx, got))
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.False(t, obs.observed, "deletion must use durable inventory without another Apply")
	assert.Equal(t, [][]executor.Contribution{want}, obs.releaseCalls)
	assert.True(t, apierrors.IsNotFound(cl.Get(ctx, key, &expv1alpha1.Graph{})))
}
