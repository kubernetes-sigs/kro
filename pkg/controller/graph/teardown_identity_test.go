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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

func TestReconcile_AppliedIdentity(t *testing.T) {
	t.Parallel()

	const current = "system:serviceaccount:default:deployer"
	const prior = "system:serviceaccount:default:old-sa"
	hard := errors.New("apply forbidden")
	resource := executor.ApplyResult{Applied: []expv1alpha1.ManagedResource{{
		NodeID: "cm", APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "child", UID: "child-uid",
	}}}
	contribution := executor.ApplyResult{Contributions: []executor.Contribution{{
		APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "target",
		FieldManager: executor.PatchFieldManager("graph-uid", "patch"),
	}}}

	cases := []struct {
		name            string
		previous        string
		result          executor.ApplyResult
		applyErr        error
		noImpersonation bool
		wantIdentity    string
	}{
		{
			name: "first partial resource", result: resource, applyErr: hard, wantIdentity: current,
		},
		{
			name: "first partial contribution", result: contribution, applyErr: hard, wantIdentity: current,
		},
		{
			name: "empty hard failure with intent records nothing", applyErr: hard,
		},
		{
			name: "empty hard failure preserves prior identity", previous: prior, applyErr: hard, wantIdentity: prior,
		},
		{
			name: "partial resource preserves prior identity", previous: prior,
			result: resource, applyErr: hard, wantIdentity: prior,
		},
		{
			name: "partial contribution preserves prior identity", previous: prior,
			result: contribution, applyErr: hard, wantIdentity: prior,
		},
		{
			name: "clean empty apply updates identity", previous: prior, wantIdentity: current,
		},
		{
			name: "soft empty apply updates identity", previous: prior, applyErr: executor.ErrNotReady, wantIdentity: current,
		},
		{
			name: "inactive impersonation records nothing", result: resource, applyErr: hard, noImpersonation: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			g := graph("g", withFinalizer)
			g.UID = "graph-uid"
			g.Spec.ServiceAccountName = "deployer"
			g.Status.AppliedServiceAccount = tc.previous
			cl := newClient(t, g)
			exec := &fakeExecutor{applyResult: tc.result, applyErr: tc.applyErr}
			// Intent alone must not record an identity after an empty hard failure.
			prog := templateProgram("cm", "v1", "ConfigMap", "default", "child")
			if len(tc.result.Contributions) > 0 {
				prog = patchProgram("patch", "v1", "ConfigMap", "default", "target")
			}
			r := &Reconciler{
				Client: cl, Compiler: &fakeCompiler{program: prog}, Registry: registry.New(), Executor: exec,
			}
			if !tc.noImpersonation {
				r.Impersonation = &impersonationCache{
					newExec: func(user string) (executor.Interface, error) {
						assert.Equal(t, current, user)
						return exec, nil
					},
				}
			}
			key := client.ObjectKeyFromObject(g)
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			if tc.applyErr == hard {
				require.ErrorIs(t, err, hard)
			} else {
				require.NoError(t, err)
			}

			got := &expv1alpha1.Graph{}
			require.NoError(t, cl.Get(ctx, key, got))
			assert.Equal(t, tc.wantIdentity, got.Status.AppliedServiceAccount)
			for _, mr := range tc.result.Applied {
				assert.Contains(t, got.Status.ManagedResources, mr)
			}
			assert.Equal(t, toAPIContributions(tc.result.Contributions), got.Status.Contributions)
		})
	}
}

func TestReconcile_TeardownIdentityFallback(t *testing.T) {
	t.Parallel()

	const prior = "system:serviceaccount:default:prior"
	const current = "system:serviceaccount:default:current"
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "child", errors.New("prior ServiceAccount denied"))
	currentDenied := apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "child", errors.New("current ServiceAccount denied"))
	unavailable := apierrors.NewServiceUnavailable("try again")
	buildErr := errors.New("cannot build current executor")

	for _, operation := range []string{"delete", "release"} {
		t.Run(operation, func(t *testing.T) {
			for _, tc := range []struct {
				name            string
				primaryErr      error
				fallbackErr     error
				factoryErr      error
				noRecorded      bool
				sameIdentity    bool
				controllerSelf  bool
				noImpersonation bool
				wantFallback    bool
				wantErrors      []error
			}{
				{name: "recorded identity succeeds"},
				{name: "current identity handles denied cleanup", primaryErr: forbidden, wantFallback: true},
				{name: "transient error retains recorded identity", primaryErr: unavailable, wantErrors: []error{unavailable}},
				{name: "both identities denied", primaryErr: forbidden, fallbackErr: currentDenied,
					wantFallback: true, wantErrors: []error{forbidden, currentDenied}},
				{name: "executor construction fails", primaryErr: forbidden, factoryErr: buildErr,
					wantErrors: []error{forbidden, buildErr}},
				{name: "same identity is not retried", primaryErr: forbidden, sameIdentity: true, wantErrors: []error{forbidden}},
				{name: "unrecorded identity is not retried", primaryErr: forbidden, noRecorded: true, wantErrors: []error{forbidden}},
				{name: "controller identity is refused", primaryErr: forbidden, controllerSelf: true, wantErrors: []error{forbidden}},
				{name: "inactive impersonation is not retried", primaryErr: forbidden, noImpersonation: true, wantErrors: []error{forbidden}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					ctx := context.Background()
					g := graph("g", withFinalizer, withDeletionTimestamp)
					g.Spec.ServiceAccountName = "current"
					g.Status.AppliedServiceAccount = prior
					if tc.sameIdentity {
						g.Spec.ServiceAccountName = "prior"
					}
					if tc.noRecorded {
						g.Status.AppliedServiceAccount = ""
					}
					primary, fallback := &fakeExecutor{}, &fakeExecutor{}
					if operation == "delete" {
						g.Status.ManagedResources = []expv1alpha1.ManagedResource{{
							NodeID: "cm", APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "child", UID: "child-uid",
						}}
						primary.deleteErr, fallback.deleteErr = tc.primaryErr, tc.fallbackErr
					} else {
						g.Status.Contributions = toAPIContributions([]executor.Contribution{{
							APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: "target",
							FieldManager: executor.PatchFieldManager("graph-uid", "patch"),
						}})
						primary.releaseErr, fallback.releaseErr = tc.primaryErr, tc.fallbackErr
					}
					cl := newClient(t, g)
					r := &Reconciler{Client: cl, Registry: registry.New(), Executor: primary}
					if tc.controllerSelf {
						r.ControllerServiceAccount = current
					}
					if !tc.noImpersonation {
						r.Impersonation = &impersonationCache{newExec: func(user string) (executor.Interface, error) {
							if user == prior || tc.noRecorded {
								return primary, nil
							}
							require.Equal(t, current, user)
							require.False(t, tc.controllerSelf, "cleanup must not impersonate the controller")
							return fallback, tc.factoryErr
						}}
					}
					key := client.ObjectKeyFromObject(g)
					_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
					if len(tc.wantErrors) == 0 {
						require.NoError(t, err)
						assert.True(t, apierrors.IsNotFound(cl.Get(ctx, key, &expv1alpha1.Graph{})))
					} else {
						for _, want := range tc.wantErrors {
							assert.ErrorIs(t, err, want)
						}
						got := &expv1alpha1.Graph{}
						require.NoError(t, cl.Get(ctx, key, got))
						assert.True(t, metadata.HasGraphFinalizer(got))
						assert.Equal(t, g.Status.AppliedServiceAccount, got.Status.AppliedServiceAccount)
						assert.Equal(t, g.Status.ManagedResources, got.Status.ManagedResources)
						assert.Equal(t, g.Status.Contributions, got.Status.Contributions)
						condition := findCondition(got.Status.Conditions, ResourcesConverged)
						require.NotNil(t, condition)
						require.NotNil(t, condition.Reason)
						assert.Equal(t, "DeleteFailed", *condition.Reason)
					}
					if operation == "delete" {
						assert.Len(t, primary.deleteCalls, 1)
						assert.Equal(t, tc.wantFallback, len(fallback.deleteCalls) > 0)
					} else {
						assert.Len(t, primary.releaseCalls, 1)
						assert.Equal(t, tc.wantFallback, len(fallback.releaseCalls) > 0)
					}
				})
			}
		})
	}
}

func TestReconcile_TeardownContinuesAfterDeleteErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := graph("g", withFinalizer, withDeletionTimestamp)
	for _, name := range []string{"dependency", "dependent", "leaf"} {
		g.Status.ManagedResources = append(g.Status.ManagedResources, expv1alpha1.ManagedResource{
			NodeID: name, APIVersion: "v1", Kind: "ConfigMap", Namespace: "default", Name: name, UID: name + "-uid",
		})
	}
	cl := newClient(t, g)
	unavailable := apierrors.NewServiceUnavailable("try again")
	exec := &fakeExecutor{deleteErr: unavailable}
	r := &Reconciler{Client: cl, Registry: registry.New(), Executor: exec}
	key := client.ObjectKeyFromObject(g)
	req := ctrl.Request{NamespacedName: key}
	_, err := r.Reconcile(ctx, req)
	require.ErrorIs(t, err, unavailable)
	assert.Equal(t, [][]expv1alpha1.ManagedResource{
		{g.Status.ManagedResources[2]},
		{g.Status.ManagedResources[1]},
		{g.Status.ManagedResources[0]},
	}, exec.deleteCalls, "cleanup must attempt every resource in reverse order despite errors")
	got := &expv1alpha1.Graph{}
	require.NoError(t, cl.Get(ctx, key, got))
	assert.True(t, metadata.HasGraphFinalizer(got))
	assert.Equal(t, g.Status.ManagedResources, got.Status.ManagedResources)

	exec.deleteErr = nil
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.True(t, apierrors.IsNotFound(cl.Get(ctx, key, got)), "cleanup must finish when deletion succeeds")
}
