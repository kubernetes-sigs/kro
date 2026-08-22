// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package graph

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	krotruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/schemawatcher"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// fakeExecutor lets reconciler tests opt into apply/delete failures.
// Default zero value is a noop, matching the previous noopExecutor.
// applyResult is returned alongside applyErr so tests can inspect the
// tracking diff that drives prune candidacy.
type fakeExecutor struct {
	applyErr     error
	applyResult  executor.ApplyResult
	deleteErr    error
	deleteCalls  [][]expv1alpha1.ManagedResource // captures every Delete invocation in order
	releaseErr   error
	releaseCalls [][]executor.Contribution // captures every Release invocation in order
}

func (f *fakeExecutor) Apply(context.Context, *krotruntime.Runtime, watchrouter.Watcher) (executor.ApplyResult, error) {
	return f.applyResult, f.applyErr
}
func (f *fakeExecutor) Delete(_ context.Context, resources []expv1alpha1.ManagedResource) error {
	f.deleteCalls = append(f.deleteCalls, resources)
	return f.deleteErr
}
func (f *fakeExecutor) Release(_ context.Context, contributions []executor.Contribution) error {
	f.releaseCalls = append(f.releaseCalls, contributions)
	return f.releaseErr
}

// patchErrClient is a fake client that fails Patch (and optionally Get)
// with the supplied errors. Other calls delegate to the embedded client.
type patchErrClient struct {
	client.Client
	patchErr  error
	getErr    error
	statusErr error
}

func (c *patchErrClient) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
	if c.patchErr != nil {
		return c.patchErr
	}
	return c.Client.Patch(ctx, obj, p, opts...)
}
func (c *patchErrClient) Get(ctx context.Context, key types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
	if c.getErr != nil {
		return c.getErr
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
func (c *patchErrClient) Status() client.StatusWriter {
	if c.statusErr != nil {
		return &statusErrWriter{err: c.statusErr}
	}
	return c.Client.Status()
}

type statusErrWriter struct{ err error }

func (s *statusErrWriter) Create(context.Context, client.Object, client.Object, ...client.SubResourceCreateOption) error {
	return s.err
}
func (s *statusErrWriter) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return s.err
}
func (s *statusErrWriter) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return s.err
}
func (s *statusErrWriter) Apply(context.Context, runtime.ApplyConfiguration, ...client.SubResourceApplyOption) error {
	return s.err
}

// fakeCompiler is a deterministic stand-in for compiler.Compile. The Compile
// method returns the canned (program, err) on every call.
type fakeCompiler struct {
	program *compiler.Program
	err     error
	calls   int
}

func (f *fakeCompiler) Compile(*expv1alpha1.Graph) (*compiler.Program, error) {
	f.calls++
	return f.program, f.err
}

func newClient(t *testing.T, g *expv1alpha1.Graph) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, expv1alpha1.AddToScheme(s))
	return fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&expv1alpha1.Graph{}).
		WithObjects(g).
		Build()
}

func graph(name string, mut ...func(*expv1alpha1.Graph)) *expv1alpha1.Graph {
	g := &expv1alpha1.Graph{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "default",
			Generation: 1,
		},
		Spec: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{{ID: "n"}}},
	}
	for _, m := range mut {
		m(g)
	}
	return g
}

func withFinalizer(g *expv1alpha1.Graph) { g.Finalizers = []string{metadata.GraphFinalizer} }

// withManagedResource appends a single ManagedResource so the deletion
// path actually invokes Executor.Delete. Useful for tests that exercise
// the delete-time error path.
func withManagedResource(g *expv1alpha1.Graph) {
	g.Status.ManagedResources = append(g.Status.ManagedResources, expv1alpha1.ManagedResource{
		NodeID:     "n",
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Namespace:  "default",
		Name:       "cm",
	})
}
func withDeletionTimestamp(g *expv1alpha1.Graph) {
	now := metav1.Now()
	g.DeletionTimestamp = &now
}

func withMalformedContributions(g *expv1alpha1.Graph) {
	if g.Annotations == nil {
		g.Annotations = map[string]string{}
	}
	g.Annotations[metadata.PatchContributionsAnnotation] = "not-json"
}

func TestReconcile(t *testing.T) {
	t.Parallel()

	prog := func(nodes int) *compiler.Program {
		m := make(map[string]*compiler.Node, nodes)
		for i := 0; i < nodes; i++ {
			m[string(rune('a'+i))] = &compiler.Node{}
		}
		return &compiler.Program{Nodes: m}
	}

	cases := []struct {
		name       string
		initial    *expv1alpha1.Graph                // graph state preloaded into the fake client
		compile    *fakeCompiler                     // canned compiler reply
		exec       *fakeExecutor                     // canned executor; zero value is a noop
		wrapClient func(client.Client) client.Client // optional client wrapper for error injection
		runs       int                               // reconcile passes to execute (defaults to 1)
		reqName    string                            // request name (defaults to initial.Name)
		wantErr    string                            // substring expected in the final reconcile error
		wantCmp    int                               // expected total fakeCompiler.calls
		wantGone   bool                              // expect a NotFound on refetch after reconcile
		// wantRequeue, when > 0, asserts the reconcile returned no error
		// and ctrl.Result.RequeueAfter equals this value.
		wantRequeue time.Duration
		after       func(t *testing.T, g *expv1alpha1.Graph)
	}{
		{
			name:    "not found is a no-op",
			initial: graph("other"),
			reqName: "missing",
			compile: &fakeCompiler{program: prog(1)},
		},
		{
			name:    "first pass adds the finalizer",
			initial: graph("g"),
			compile: &fakeCompiler{program: prog(1)},
			after: func(t *testing.T, g *expv1alpha1.Graph) {
				assert.True(t, metadata.HasGraphFinalizer(g))
			},
		},
		{
			name:    "finalizer is idempotent",
			initial: graph("g", withFinalizer),
			compile: &fakeCompiler{program: prog(1)},
			wantCmp: 1,
			after: func(t *testing.T, g *expv1alpha1.Graph) {
				assert.Equal(t, 1, countFinalizer(g.Finalizers, metadata.GraphFinalizer))
			},
		},
		{
			name:     "deletion releases the finalizer",
			initial:  graph("g", withFinalizer, withDeletionTimestamp),
			compile:  &fakeCompiler{program: prog(1)},
			wantGone: true,
			wantCmp:  0,
		},
		{
			name:    "successful compile sets Accepted=True and Ready=True",
			initial: graph("g", withFinalizer),
			compile: &fakeCompiler{program: prog(2)},
			wantCmp: 1,
			after: func(t *testing.T, g *expv1alpha1.Graph) {
				acc := findCondition(g.Status.Conditions, GraphAccepted)
				require.NotNil(t, acc)
				assert.Equal(t, metav1.ConditionTrue, acc.Status)
				require.NotNil(t, acc.Reason)
				assert.Equal(t, "Compiled", *acc.Reason)
				ready := findCondition(g.Status.Conditions, Ready)
				require.NotNil(t, ready)
				assert.Equal(t, metav1.ConditionTrue, ready.Status)
			},
		},
		{
			name:    "second pass with identical spec hits the registry cache",
			initial: graph("g", withFinalizer),
			compile: &fakeCompiler{program: prog(1)},
			runs:    2,
			wantCmp: 1, // cache hit on the second pass
			after: func(t *testing.T, g *expv1alpha1.Graph) {
				acc := findCondition(g.Status.Conditions, GraphAccepted)
				require.NotNil(t, acc)
				assert.Equal(t, metav1.ConditionTrue, acc.Status)
			},
		},
		{
			name:    "compile failure sets Accepted=False and Ready=False",
			initial: graph("g", withFinalizer),
			compile: &fakeCompiler{err: errors.New("bad graph: missing input node")},
			wantErr: "bad graph",
			wantCmp: 1,
			after: func(t *testing.T, g *expv1alpha1.Graph) {
				acc := findCondition(g.Status.Conditions, GraphAccepted)
				require.NotNil(t, acc)
				assert.Equal(t, metav1.ConditionFalse, acc.Status)
				require.NotNil(t, acc.Reason)
				assert.Equal(t, "InvalidGraph", *acc.Reason)
				require.NotNil(t, acc.Message)
				assert.Contains(t, *acc.Message, "missing input node")
				ready := findCondition(g.Status.Conditions, Ready)
				require.NotNil(t, ready)
				assert.Equal(t, metav1.ConditionFalse, ready.Status)
			},
		},
		{
			name:    "executor apply failure surfaces a wrapped error",
			initial: graph("g", withFinalizer),
			compile: &fakeCompiler{program: prog(1)},
			exec:    &fakeExecutor{applyErr: errors.New("ssa boom")},
			wantErr: "ssa boom",
		},
		{
			name:    "executor delete failure during deletion surfaces a wrapped error",
			initial: graph("g", withFinalizer, withDeletionTimestamp, withManagedResource),
			compile: &fakeCompiler{program: prog(1)},
			exec:    &fakeExecutor{deleteErr: errors.New("delete boom")},
			wantErr: "executor delete",
		},
		{
			// Delete no longer consults the compiler — identities come
			// from Status.ManagedResources. So a Graph with no tracking
			// (e.g. one that never had a successful apply) just drops
			// its finalizer immediately. This row pins that.
			name:     "deletion with no managed resources drops the finalizer without calling executor",
			initial:  graph("g", withFinalizer, withDeletionTimestamp),
			compile:  &fakeCompiler{err: errors.New("won't compile — irrelevant; delete skips compile")},
			wantGone: true,
		},
		{
			name:    "client.Get other than NotFound returns a wrapped error",
			initial: graph("g"),
			compile: &fakeCompiler{program: prog(1)},
			wrapClient: func(c client.Client) client.Client {
				return &patchErrClient{Client: c, getErr: errors.New("get boom")}
			},
			wantErr: "get Graph",
		},
		{
			name:    "setManaged Patch failure surfaces a wrapped error",
			initial: graph("g"),
			compile: &fakeCompiler{program: prog(1)},
			wrapClient: func(c client.Client) client.Client {
				return &patchErrClient{Client: c, patchErr: errors.New("patch boom")}
			},
			wantErr: "set managed",
		},
		{
			name:    "setUnmanaged Patch failure surfaces a wrapped error",
			initial: graph("g", withFinalizer, withDeletionTimestamp),
			compile: &fakeCompiler{program: prog(1)},
			wrapClient: func(c client.Client) client.Client {
				return &patchErrClient{Client: c, patchErr: errors.New("unmanage boom")}
			},
			wantErr: "set unmanaged",
		},
		{
			name:        "executor ErrNotReady becomes a timed requeue, not a hard error",
			initial:     graph("g", withFinalizer),
			compile:     &fakeCompiler{program: prog(1)},
			exec:        &fakeExecutor{applyErr: fmt.Errorf("apply %q: %w", "n", executor.ErrNotReady)},
			wantRequeue: notReadyRequeueAfter,
		},
		{
			name:    "updateStatus Patch failure is joined with the reconcile result",
			initial: graph("g", withFinalizer),
			compile: &fakeCompiler{program: prog(1)},
			wrapClient: func(c client.Client) client.Client {
				return &patchErrClient{Client: c, statusErr: errors.New("status boom")}
			},
			wantErr: "status boom",
		},
		{
			name:     "deletion with malformed patch contributions retains finalizer and errors",
			initial:  graph("g", withFinalizer, withDeletionTimestamp, withMalformedContributions),
			wantErr:  "read patch contributions",
			wantGone: false,
			after: func(t *testing.T, g *expv1alpha1.Graph) {
				assert.Equal(t, 1, countFinalizer(g.Finalizers, metadata.GraphFinalizer))
			},
		},
		{
			name:    "reconcile with malformed patch contributions returns error",
			initial: graph("g", withFinalizer, withMalformedContributions),
			compile: &fakeCompiler{program: prog(1)},
			wantErr: "read patch contributions",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cl := newClient(t, tc.initial)
			var clientForReconciler client.Client = cl
			if tc.wrapClient != nil {
				clientForReconciler = tc.wrapClient(cl)
			}
			exec := tc.exec
			if exec == nil {
				exec = &fakeExecutor{}
			}
			r := &Reconciler{Client: clientForReconciler, Compiler: tc.compile, Registry: registry.New(), Executor: exec}
			req := ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: "default",
				Name:      pick(tc.reqName, tc.initial.Name),
			}}
			runs := tc.runs
			if runs == 0 {
				runs = 1
			}
			var (
				err    error
				result ctrl.Result
			)
			for i := 0; i < runs; i++ {
				result, err = r.Reconcile(context.Background(), req)
			}
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			if tc.wantRequeue > 0 {
				assert.Equal(t, tc.wantRequeue, result.RequeueAfter)
			}
			if tc.wantCmp != 0 {
				assert.Equal(t, tc.wantCmp, tc.compile.calls)
			}
			got := &expv1alpha1.Graph{}
			getErr := cl.Get(context.Background(), req.NamespacedName, got)
			if tc.wantGone {
				require.Error(t, getErr)
				assert.True(t, apierrors.IsNotFound(getErr))
				return
			}
			if apierrors.IsNotFound(getErr) {
				// Request targeted a name we never preloaded — nothing more
				// to assert beyond the reconcile result above.
				return
			}
			require.NoError(t, getErr)
			if tc.after != nil {
				tc.after(t, got)
			}
		})
	}
}

func TestReconcile_RecoversFromCompileError(t *testing.T) {
	// Multi-pass behavior is awkward to express in the main table because
	// the fake compiler's state has to flip between calls. The cache means
	// only the fail→succeed direction is observable from a single Graph
	// spec: same-spec resubmits hit the cache and never re-invoke compile.
	t.Parallel()
	cases := []struct {
		name           string
		first, second  error
		secondProgram  *compiler.Program
		wantFinalAcc   metav1.ConditionStatus
		wantFinalReady metav1.ConditionStatus
	}{
		{
			name:           "fail then succeed recompiles because failure isn't cached",
			first:          errors.New("bad"),
			second:         nil,
			secondProgram:  &compiler.Program{Nodes: map[string]*compiler.Node{"n": {}}},
			wantFinalAcc:   metav1.ConditionTrue,
			wantFinalReady: metav1.ConditionTrue,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := graph("g", withFinalizer)
			cl := newClient(t, g)
			fc := &fakeCompiler{err: tc.first, program: &compiler.Program{Nodes: map[string]*compiler.Node{"n": {}}}}
			r := &Reconciler{Client: cl, Compiler: fc, Registry: registry.New(), Executor: &fakeExecutor{}}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "g"}}
			_, _ = r.Reconcile(context.Background(), req)

			fc.err = tc.second
			fc.program = tc.secondProgram
			_, _ = r.Reconcile(context.Background(), req)

			got := &expv1alpha1.Graph{}
			require.NoError(t, cl.Get(context.Background(), req.NamespacedName, got))
			assert.Equal(t, tc.wantFinalAcc, findCondition(got.Status.Conditions, GraphAccepted).Status)
			assert.Equal(t, tc.wantFinalReady, findCondition(got.Status.Conditions, Ready).Status)
		})
	}
}

func TestConditionsMarker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		apply       func(*ConditionsMarker)
		wantAcc     metav1.ConditionStatus
		wantReady   metav1.ConditionStatus
		wantReason  string
		msgContains string
	}{
		{
			name: "GraphCompiled alone leaves Ready Unknown (resources still pending)",
			apply: func(m *ConditionsMarker) {
				m.GraphCompiled(3)
			},
			wantAcc:     metav1.ConditionTrue,
			wantReady:   metav1.ConditionUnknown,
			wantReason:  "Compiled",
			msgContains: "3",
		},
		{
			name: "GraphCompiled + ResourcesConverged flips Ready true",
			apply: func(m *ConditionsMarker) {
				m.GraphCompiled(3)
				m.ResourcesConverged()
			},
			wantAcc:     metav1.ConditionTrue,
			wantReady:   metav1.ConditionTrue,
			wantReason:  "Compiled",
			msgContains: "3",
		},
		{
			name: "GraphInvalid marks Accepted false; Ready follows",
			apply: func(m *ConditionsMarker) {
				m.GraphInvalid("boom")
			},
			wantAcc:     metav1.ConditionFalse,
			wantReady:   metav1.ConditionFalse,
			wantReason:  "InvalidGraph",
			msgContains: "boom",
		},
		{
			name: "ResourcesNotReady drives Ready false even with Accepted true",
			apply: func(m *ConditionsMarker) {
				m.GraphCompiled(1)
				m.ResourcesNotReady("waiting")
			},
			wantAcc:     metav1.ConditionTrue,
			wantReady:   metav1.ConditionFalse,
			wantReason:  "Compiled",
			msgContains: "1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := graph("g")
			tc.apply(NewConditionsMarkerFor(g))

			acc := findCondition(g.Status.Conditions, GraphAccepted)
			require.NotNil(t, acc)
			assert.Equal(t, tc.wantAcc, acc.Status)
			require.NotNil(t, acc.Reason)
			assert.Equal(t, tc.wantReason, *acc.Reason)
			require.NotNil(t, acc.Message)
			assert.Contains(t, *acc.Message, tc.msgContains)

			ready := findCondition(g.Status.Conditions, Ready)
			require.NotNil(t, ready)
			assert.Equal(t, tc.wantReady, ready.Status)
		})
	}
}

func TestSetManagedUnmanaged(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		initial          *expv1alpha1.Graph
		op               func(*Reconciler, *expv1alpha1.Graph) error
		wantHasFinalizer bool
	}{
		{
			name:             "setManaged adds finalizer when absent",
			initial:          graph("g"),
			op:               func(r *Reconciler, g *expv1alpha1.Graph) error { return r.setManaged(context.Background(), g) },
			wantHasFinalizer: true,
		},
		{
			name:             "setManaged is a no-op when already present",
			initial:          graph("g", withFinalizer),
			op:               func(r *Reconciler, g *expv1alpha1.Graph) error { return r.setManaged(context.Background(), g) },
			wantHasFinalizer: true,
		},
		{
			name:             "setUnmanaged is a no-op when no finalizer is present",
			initial:          graph("g"),
			op:               func(r *Reconciler, g *expv1alpha1.Graph) error { return r.setUnmanaged(context.Background(), g) },
			wantHasFinalizer: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cl := newClient(t, tc.initial)
			r := &Reconciler{Client: cl, Compiler: &fakeCompiler{program: &compiler.Program{}}, Registry: registry.New()}
			require.NoError(t, tc.op(r, tc.initial))
			assert.Equal(t, tc.wantHasFinalizer, metadata.HasGraphFinalizer(tc.initial))
		})
	}
}

// ---- helpers ----------------------------------------------------------------

func findCondition(cs expv1alpha1.Conditions, t string) *expv1alpha1.Condition {
	for i := range cs {
		if string(cs[i].Type) == t {
			return &cs[i]
		}
	}
	return nil
}

func countFinalizer(fins []string, want string) int {
	n := 0
	for _, f := range fins {
		if f == want {
			n++
		}
	}
	return n
}

func pick(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}

func TestReconcile_MissingCRDTrackedAndRecovers(t *testing.T) {
	t.Parallel()

	gk := schema.GroupKind{Group: "example.com", Kind: "Widget"}
	key := types.NamespacedName{Namespace: "default", Name: "g"}

	// Graph referencing a Kind whose CRD does not exist yet.
	g := graph("g", withFinalizer, func(g *expv1alpha1.Graph) {
		g.Spec.Nodes = []expv1alpha1.Node{{
			ID: "widgetNode",
			Template: &runtime.RawExtension{
				Raw: []byte(`{"apiVersion":"example.com/v1","kind":"Widget","metadata":{"name":"w"}}`),
			},
		}}
	})

	cl := newClient(t, g)
	sw := schemawatcher.New(logr.Discard(), schemawatcher.Config{EventBuffer: 16})
	fc := &fakeCompiler{
		err: errors.New("cannot resolve schema for example.com/v1, Kind=Widget: schema not found"),
	}
	r := &Reconciler{
		Client:        cl,
		Compiler:      fc,
		Registry:      registry.New(),
		Executor:      &fakeExecutor{},
		SchemaWatcher: sw,
	}

	// First reconcile fails to compile because CRD is missing.
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.Error(t, err)

	// BUT the SchemaWatcher MUST still track the GroupKind dependency from the spec!
	tracked := sw.GraphsForGroupKind(gk)
	require.Contains(t, tracked, key, "graph failing compilation due to missing CRD must still be tracked by schema watcher")

	// Now simulate CRD appearance: compiler succeeds.
	fc.err = nil
	fc.program = &compiler.Program{
		Nodes: map[string]*compiler.Node{"widgetNode": {}},
	}

	// Re-reconcile (triggered by schema watcher on CRD add).
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)

	// Status should now be Accepted=True and Ready=True.
	got := &expv1alpha1.Graph{}
	require.NoError(t, cl.Get(context.Background(), key, got))
	acc := findCondition(got.Status.Conditions, GraphAccepted)
	require.NotNil(t, acc)
	assert.Equal(t, metav1.ConditionTrue, acc.Status)
	rdy := findCondition(got.Status.Conditions, Ready)
	require.NotNil(t, rdy)
	assert.Equal(t, metav1.ConditionTrue, rdy.Status)
}

func TestReconcile_MissingCRDInRefAndPatchAndSubgraphTracked(t *testing.T) {
	t.Parallel()

	gkRef := schema.GroupKind{Group: "ref.example.com", Kind: "RefKind"}
	gkPatch := schema.GroupKind{Group: "patch.example.com", Kind: "PatchKind"}
	gkSub := schema.GroupKind{Group: "sub.example.com", Kind: "SubKind"}
	key := types.NamespacedName{Namespace: "default", Name: "g-multi"}

	g := graph("g-multi", withFinalizer, func(g *expv1alpha1.Graph) {
		g.Spec.Nodes = []expv1alpha1.Node{
			{
				ID: "refNode",
				Ref: &expv1alpha1.ExternalRef{
					APIVersion: "ref.example.com/v1",
					Kind:       "RefKind",
				},
			},
			{
				ID: "patchNode",
				Patch: &expv1alpha1.PatchSpec{
					APIVersion: "patch.example.com/v1",
					Kind:       "PatchKind",
				},
			},
			{
				ID: "subgraphNode",
				Graph: &runtime.RawExtension{
					Raw: []byte(`{"nodes":[{"id":"inner","template":{"apiVersion":"sub.example.com/v1","kind":"SubKind","metadata":{"name":"sub"}}}]}`),
				},
			},
			{
				ID: "defNode",
				Def: &runtime.RawExtension{
					Raw: []byte(`{"value":"local"}`),
				},
			},
		}
	})

	cl := newClient(t, g)
	sw := schemawatcher.New(logr.Discard(), schemawatcher.Config{EventBuffer: 16})
	fc := &fakeCompiler{
		err: errors.New("cannot resolve schema: missing CRD"),
	}
	r := &Reconciler{
		Client:        cl,
		Compiler:      fc,
		Registry:      registry.New(),
		Executor:      &fakeExecutor{},
		SchemaWatcher: sw,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.Error(t, err)

	assert.Contains(t, sw.GraphsForGroupKind(gkRef), key)
	assert.Contains(t, sw.GraphsForGroupKind(gkPatch), key)
	assert.Contains(t, sw.GraphsForGroupKind(gkSub), key)
}

func TestReconcile_DynamicGVKTrackedOnCompileFailure(t *testing.T) {
	t.Parallel()

	key := types.NamespacedName{Namespace: "default", Name: "g-dyn"}

	g := graph("g-dyn", withFinalizer, func(g *expv1alpha1.Graph) {
		g.Spec.Nodes = []expv1alpha1.Node{{
			ID: "dynNode",
			Template: &runtime.RawExtension{
				Raw: []byte(`{"apiVersion":"${schema.group}/v1","kind":"Widget","metadata":{"name":"w"}}`),
			},
		}}
	})

	cl := newClient(t, g)
	sw := schemawatcher.New(logr.Discard(), schemawatcher.Config{EventBuffer: 16})
	fc := &fakeCompiler{
		err: errors.New("compile error"),
	}
	r := &Reconciler{
		Client:        cl,
		Compiler:      fc,
		Registry:      registry.New(),
		Executor:      &fakeExecutor{},
		SchemaWatcher: sw,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.Error(t, err)

	assert.Contains(t, sw.DynamicGraphs(), key)
}

func TestReconcile_SpecEvolutionMissingCRDReplacesTrackedGK(t *testing.T) {
	t.Parallel()

	gkOld := schema.GroupKind{Group: "example.com", Kind: "OldWidget"}
	gkNew := schema.GroupKind{Group: "example.com", Kind: "NewWidget"}
	key := types.NamespacedName{Namespace: "default", Name: "g-evolve"}

	// Version 1: references OldWidget (compiles fine).
	g := graph("g-evolve", withFinalizer, func(g *expv1alpha1.Graph) {
		g.Spec.Nodes = []expv1alpha1.Node{{
			ID: "oldNode",
			Template: &runtime.RawExtension{
				Raw: []byte(`{"apiVersion":"example.com/v1","kind":"OldWidget","metadata":{"name":"w"}}`),
			},
		}}
	})

	cl := newClient(t, g)
	sw := schemawatcher.New(logr.Discard(), schemawatcher.Config{EventBuffer: 16})
	fc := &fakeCompiler{
		program: &compiler.Program{Nodes: map[string]*compiler.Node{"oldNode": {}}},
	}
	r := &Reconciler{
		Client:        cl,
		Compiler:      fc,
		Registry:      registry.New(),
		Executor:      &fakeExecutor{},
		SchemaWatcher: sw,
	}

	// First reconcile succeeds.
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	assert.Contains(t, sw.GraphsForGroupKind(gkOld), key)

	// Version 2: spec is updated to reference NewWidget whose CRD does not exist yet.
	gUpdated := &expv1alpha1.Graph{}
	require.NoError(t, cl.Get(context.Background(), key, gUpdated))
	gUpdated.Spec.Nodes = []expv1alpha1.Node{{
		ID: "newNode",
		Template: &runtime.RawExtension{
			Raw: []byte(`{"apiVersion":"example.com/v1","kind":"NewWidget","metadata":{"name":"w"}}`),
		},
	}}
	require.NoError(t, cl.Update(context.Background(), gUpdated))

	// Compiler now fails because NewWidget schema is not found.
	fc.err = errors.New("cannot resolve schema for example.com/v1, Kind=NewWidget: schema not found")
	fc.program = nil

	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	require.Error(t, err)

	// OldWidget should be evicted from tracking, NewWidget must now be tracked!
	assert.NotContains(t, sw.GraphsForGroupKind(gkOld), key, "old GK must be evicted from schema watcher")
	assert.Contains(t, sw.GraphsForGroupKind(gkNew), key, "new missing GK must be tracked in schema watcher")
}

func TestTrackSchemaDependencies_DynamicGVKAndMalformedNodes(t *testing.T) {
	t.Parallel()

	sw := schemawatcher.New(logr.Discard(), schemawatcher.Config{EventBuffer: 16})
	key := types.NamespacedName{Namespace: "default", Name: "g-dynamic"}
	sub := sw.ForGraph(key)

	nodes := []expv1alpha1.Node{
		{
			ID: "staticTemplate",
			Template: &runtime.RawExtension{
				Raw: []byte(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"d"}}`),
			},
		},
		{
			ID: "dynamicVersionStaticGroup",
			Template: &runtime.RawExtension{
				Raw: []byte(`{"apiVersion":"custom.io/${schema.spec.version}","kind":"CustomApp","metadata":{"name":"c"}}`),
			},
		},
		{
			ID: "dynamicKind",
			Template: &runtime.RawExtension{
				Raw: []byte(`{"apiVersion":"apps/v1","kind":"${schema.spec.kind}","metadata":{"name":"k"}}`),
			},
		},
		{
			ID: "malformedYAML",
			Template: &runtime.RawExtension{
				Raw: []byte(`{invalid: yaml: [}`),
			},
		},
		{
			ID: "malformedAPIVersion",
			Template: &runtime.RawExtension{
				Raw: []byte(`{"apiVersion":"apps/v1/extra/segment","kind":"Invalid","metadata":{"name":"i"}}`),
			},
		},
		{
			ID: "refNodeDynamicVersionStaticGroup",
			Ref: &expv1alpha1.ExternalRef{
				APIVersion: "refgroup.io/${version}",
				Kind:       "RefApp",
			},
		},
	}

	trackSchemaDependencies(sub, nodes)
	sub.Done(true)

	// Static template should be tracked
	assert.Contains(t, sw.GraphsForGroupKind(schema.GroupKind{Group: "apps", Kind: "Deployment"}), key)

	// Dynamic version with static group and kind should be tracked under GK AND dynamic
	assert.Contains(t, sw.GraphsForGroupKind(schema.GroupKind{Group: "custom.io", Kind: "CustomApp"}), key)
	assert.Contains(t, sw.GraphsForGroupKind(schema.GroupKind{Group: "refgroup.io", Kind: "RefApp"}), key)
	assert.Contains(t, sw.DynamicGraphs(), key)
}
