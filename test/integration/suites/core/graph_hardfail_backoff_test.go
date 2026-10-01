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

package core_test

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/tools/cache"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/environment"
)

// rejectedConfigMapApplies counts, on the isolated envtest apiserver, the
// server-side-apply requests on ConfigMaps that were rejected with a 4xx. The
// hard-failing node issues exactly one such request per reconcile (its
// namespace does not exist), so the delta over a window is a per-reconcile
// cadence signal owned entirely by this spec's control plane. (controller-
// runtime's reconcile_total counter is process-global and shared with the
// suite's manager, so it cannot be attributed to one manager.)
func rejectedConfigMapApplies(t environment.TestingT, ctx context.Context, env *environment.Environment) float64 {
	t.Helper()
	raw, err := env.ClientSet.Kubernetes().CoreV1().RESTClient().Get().AbsPath("/metrics").DoRaw(ctx)
	if err != nil {
		t.Fatalf("read apiserver metrics: %v", err)
	}
	var total float64
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "apiserver_request_total{") ||
			!strings.Contains(line, `resource="configmaps"`) ||
			!(strings.Contains(line, `verb="APPLY"`) || strings.Contains(line, `verb="PATCH"`)) ||
			!strings.Contains(line, `code="4`) {
			continue
		}
		fields := strings.Fields(line)
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			t.Fatalf("parse metric line %q: %v", line, err)
		}
		total += v
	}
	return total
}

var _ = Describe("Graph hard apply failure", func() {
	// A Graph whose apply hard-fails every reconcile (one node targets a
	// namespace that does not exist) must not churn its watches or hot-loop.
	// Before the fix the reconciler aborted the watch set on every hard
	// failure: the sole-owner informer was torn down, the next reconcile
	// rebuilt it (a fresh full LIST/WatchList of the GVR), and the initial
	// events for the sibling ConfigMap re-enqueued the Graph immediately,
	// bypassing controller-runtime's error backoff.
	It("keeps the informer alive and backs off instead of hot-looping", func() {
		t := GinkgoT()
		// Isolated environment: this spec inspects the Router's watch Manager
		// (informer creations, active-watch count) and the process-global
		// controller-runtime reconcile counter, so it must not share a manager
		// with other specs.
		const graphName = "hardfail"
		testEnv, err := environment.New(context.Background(), environment.ControllerConfig{
			AllowCRDDeletion: true,
			LogWriter:        GinkgoWriter,
		})
		if err != nil {
			t.Fatalf("failed to create isolated env: %v", err)
		}
		t.Cleanup(func() { _ = testEnv.Stop() })

		// Count informer constructions per GVR. Each construction is a fresh
		// initial LIST (WatchList) against the apiserver.
		configMapGVR := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
		var cmInformerCreates atomic.Int64
		metaClient := testEnv.ClientSet.Metadata()
		testEnv.Router.Manager().SetInformerFactory(func(gvr schema.GroupVersionResource) cache.SharedIndexInformer {
			if gvr == configMapGVR {
				cmInformerCreates.Add(1)
			}
			return metadatainformer.NewFilteredMetadataInformer(
				metaClient, gvr, metav1.NamespaceAll, 0,
				cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
				nil,
			).Informer()
		})

		ns := testEnv.CreateNamespace(t)
		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: graphName, Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{
					{
						// Applies cleanly and exists in the cluster, so a rebuilt
						// informer's initial events match this Graph's watch.
						ID: "ok",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "hardfail-ok"},
							"data":       map[string]any{"k": "v"},
						}),
					},
					{
						// Permanent hard apply failure: the namespace does not exist.
						ID: "bad",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name":      "hardfail-bad",
								"namespace": "does-not-exist-" + ns,
							},
							"data": map[string]any{"k": "v"},
						}),
					},
				},
			},
		}
		testEnv.CreateGraph(t, g)
		graphKey := types.NamespacedName{Namespace: ns, Name: graphName}

		// The Graph must surface the hard failure.
		cond := testEnv.AwaitCondition(t, graphKey, "ResourcesConverged", metav1.ConditionFalse, 20*time.Second)
		if cond.Reason == nil || *cond.Reason != "ApplyFailed" {
			t.Fatalf("ResourcesConverged=False reason=%v want ApplyFailed (msg=%v)", cond.Reason, cond.Message)
		}
		// The healthy sibling still materialized.
		testEnv.AwaitObject(t, configMapGVK, types.NamespacedName{Namespace: ns, Name: "hardfail-ok"}, nil, 10*time.Second)

		// Let the first few (legitimately fast) error retries drain, then
		// sample a steady-state window.
		time.Sleep(3 * time.Second)

		createsBefore := cmInformerCreates.Load()
		ctx := testEnv.Context()
		reconcilesBefore := rejectedConfigMapApplies(t, ctx, testEnv)
		minActive, maxActive := int(^uint(0)>>1), -1
		const window = 5 * time.Second
		deadline := time.Now().Add(window)
		for time.Now().Before(deadline) {
			n := testEnv.Router.Manager().ActiveWatchCount()
			minActive = min(minActive, n)
			maxActive = max(maxActive, n)
			time.Sleep(20 * time.Millisecond)
		}
		creates := cmInformerCreates.Load() - createsBefore
		reconciles := rejectedConfigMapApplies(t, ctx, testEnv) - reconcilesBefore
		GinkgoWriter.Printf("hard-fail window=%s: failed graph reconciles=%v (cumulative before window=%v), configmap informer creates=%d, active watches min=%d max=%d, coordinator graphs=%d\n",
			window, reconciles, reconcilesBefore, creates, minActive, maxActive, testEnv.Router.Coordinator().GraphCount())

		// 1. The watch set is committed: the Graph stays a known owner and the
		//    ConfigMap informer is never torn down and rebuilt.
		if got := testEnv.Router.Coordinator().GraphCount(); got != 1 {
			t.Fatalf("coordinator graph count=%d want 1 (watch set must be committed on hard failure)", got)
		}
		if creates != 0 {
			t.Fatalf("configmap informer was rebuilt %d times in %s; a hard apply failure must retain the informer", creates, window)
		}
		if minActive < 1 {
			t.Fatalf("active watch count dropped to %d during the window; informer must stay alive", minActive)
		}
		// 2. Retries are paced by controller-runtime's exponential error
		//    backoff. By the time the window opens the per-item delay is well
		//    past a second, so a handful of reconciles is the ceiling; the
		//    pre-fix hot loop produced ~30 in this window (bounded only by
		//    apply latency).
		if reconciles > 10 {
			t.Fatalf("%v failed graph reconciles in %s; hard apply failures must back off, not hot-loop", reconciles, window)
		}

		// A later successful reconcile still clears the failure: create the
		// missing namespace and the Graph converges (proves the retained
		// watch/backoff path does not wedge recovery).
		missingNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "does-not-exist-" + ns}}
		if err := testEnv.Client.Create(ctx, missingNS); err != nil {
			t.Fatalf("create missing namespace: %v", err)
		}
		t.Cleanup(func() { _ = testEnv.Client.Delete(context.Background(), missingNS) })
		testEnv.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 60*time.Second)
	})
})
