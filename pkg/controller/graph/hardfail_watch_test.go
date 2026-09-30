// Copyright 2026 The Kube Resource Orchestrator Authors
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
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	metadatafake "k8s.io/client-go/metadata/fake"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	krotruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// watchingExecutor declares one scalar watch per Apply (the way the real
// executor registers a drift watch before applying an object) and then returns
// the configured apply error.
type watchingExecutor struct {
	fakeExecutor
	req watchrouter.WatchRequest
}

func (e *watchingExecutor) Apply(_ context.Context, _ *krotruntime.Runtime, w watchrouter.Watcher) (executor.ApplyResult, error) {
	if err := w.Watch(e.req); err != nil {
		return executor.ApplyResult{}, err
	}
	return e.applyResult, e.applyErr
}

// newCountingRouter builds a Router over a fake metadata client whose informer
// factory counts constructions per GVR. Every construction is a fresh initial
// LIST/WatchList against the apiserver in production.
func newCountingRouter(t *testing.T) (*watchrouter.Router, *atomic.Int64) {
	t.Helper()
	metaClient := metadatafake.NewSimpleMetadataClient(metadatafake.NewTestScheme())
	router := watchrouter.NewRouter(logr.Discard(), watchrouter.Config{}, metaClient)
	var creates atomic.Int64
	router.Manager().SetInformerFactory(func(gvr schema.GroupVersionResource) cache.SharedIndexInformer {
		creates.Add(1)
		return metadatainformer.NewFilteredMetadataInformer(
			metaClient, gvr, metav1.NamespaceAll, 0,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, nil,
		).Informer()
	})
	t.Cleanup(router.Manager().Shutdown)
	return router, &creates
}

// TestHardApplyFailureCommitsWatchSet pins the fix for a hard apply failure
// hot loop: the watch set declared during a hard-failing apply must be
// committed, so the Graph stays a known coordinator owner and the shared
// informer is retained across retries rather than torn down and rebuilt (a
// fresh full LIST per reconcile whose initial events re-enqueue the Graph
// immediately, bypassing controller-runtime's error backoff).
func TestHardApplyFailureCommitsWatchSet(t *testing.T) {
	g := graph("g", withFinalizer)
	cl := newClient(t, g)
	router, creates := newCountingRouter(t)
	cmGVR := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	exec := &watchingExecutor{
		fakeExecutor: fakeExecutor{applyErr: errors.New("ssa rejected: namespace does not exist")},
		req: watchrouter.WatchRequest{
			NodeID: "cm", GVR: cmGVR, Name: "hardfail-cm", Namespace: "default",
		},
	}
	r := &Reconciler{
		Client:   cl,
		Compiler: &fakeCompiler{program: &compiler.Program{Nodes: map[string]*compiler.Node{"cm": {}}}},
		Registry: registry.New(),
		Executor: exec,
		Router:   router,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "g"}}

	for i := range 3 {
		res, err := r.Reconcile(context.Background(), req)
		require.Errorf(t, err, "attempt %d: hard apply failure must surface as a reconcile error", i)
		assert.Zerof(t, res.RequeueAfter, "attempt %d: hard errors are paced by controller-runtime, not a timed requeue", i)

		// The declared watch is committed: the Graph is a known owner and its
		// scalar watch is in the routing index after every failed attempt.
		assert.Equalf(t, 1, router.Coordinator().GraphCount(), "attempt %d: Graph must remain a coordinator owner", i)
		scalar, _ := router.Coordinator().WatchRequestCount()
		assert.Equalf(t, 1, scalar, "attempt %d: scalar watch must stay indexed", i)
		assert.Equalf(t, 1, router.Manager().ActiveWatchCount(), "attempt %d: informer must stay running", i)
	}
	// One informer for the whole sequence — never rebuilt between failures.
	assert.Equal(t, int64(1), creates.Load(), "informer must be constructed once, not once per failed reconcile")
	assert.NotNil(t, router.Manager().GetInformer(cmGVR))
}

// TestHardApplyFailureCommitDropsRetiredWatches ensures committing on a hard
// failure still retires watches for nodes that were not re-declared this cycle,
// the same as a soft ErrNotReady commit — a hard-failing reconcile must not pin
// stale watches forever.
func TestHardApplyFailureCommitDropsRetiredWatches(t *testing.T) {
	g := graph("g", withFinalizer)
	cl := newClient(t, g)
	router, _ := newCountingRouter(t)
	cmGVR := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	secretGVR := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	exec := &watchingExecutor{
		fakeExecutor: fakeExecutor{applyErr: errors.New("boom")},
		req:          watchrouter.WatchRequest{NodeID: "old", GVR: secretGVR, Name: "s", Namespace: "default"},
	}
	r := &Reconciler{
		Client:   cl,
		Compiler: &fakeCompiler{program: &compiler.Program{Nodes: map[string]*compiler.Node{"old": {}}}},
		Registry: registry.New(),
		Executor: exec,
		Router:   router,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "g"}}

	_, err := r.Reconcile(context.Background(), req)
	require.Error(t, err)
	require.NotNil(t, router.Manager().GetInformer(secretGVR), "first cycle committed the secret watch")

	// Next cycle declares a different node/GVR and still hard-fails: the
	// secret watch is retired and its sole-owner informer released.
	exec.req = watchrouter.WatchRequest{NodeID: "new", GVR: cmGVR, Name: "c", Namespace: "default"}
	_, err = r.Reconcile(context.Background(), req)
	require.Error(t, err)
	assert.Nil(t, router.Manager().GetInformer(secretGVR), "retired watch must be released on commit")
	assert.NotNil(t, router.Manager().GetInformer(cmGVR), "newly declared watch must be committed")
	scalar, _ := router.Coordinator().WatchRequestCount()
	assert.Equal(t, 1, scalar)
}
