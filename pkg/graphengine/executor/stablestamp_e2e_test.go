// Copyright 2026 The Kubernetes Authors.
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

package executor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/cel/library"
	"github.com/kubernetes-sigs/kro/pkg/cel/sentinels"
	krotruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// stableStampGraph models the motivating example: a DB whose
// lastUpgradedTime moves only when the version changes while active, and a
// createdAt that is written exactly once. `db` is a Def node standing in
// for the observed database (version/state are the inputs under test).
func stableStampGraph(version, state string) *expv1alpha1.Graph {
	return generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithDef("db", map[string]any{"version": version, "state": state}),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "db-status"},
			"data": map[string]any{
				"version":          "${db.version}",
				"createdAt":        "${time.now().stableString(once())}",
				"lastUpgradedTime": `${time.now().stableString(whenChanged(db.version).onlyWhen(db.state == "active"))}`,
			},
		}),
	)
}

// reconcileAt runs one executor pass with time.now() fixed at now.
func reconcileAt(t *testing.T, cl client.Client, g *expv1alpha1.Graph, now time.Time) {
	t.Helper()
	p, err := newCompiler(t).Compile(g)
	require.NoError(t, err)
	rt := krotruntime.New(p, g, krotruntime.WithSeedScope(map[string]any{
		library.TimeVarName: library.NewTimeValue(now),
	}))
	_, err = NewSimple(cl).Apply(context.Background(), rt, watchrouter.NoopWatcher{})
	require.NoError(t, err)
}

func getStampCM(t *testing.T, cl client.Client) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind("ConfigMap")
	require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "db-status"}, obj))
	return obj
}

// snapshot captures the stamp-relevant content of the object: the data map
// and every stable-stamp baseline annotation.
func snapshot(obj *unstructured.Unstructured) map[string]any {
	out := map[string]any{}
	d, _, _ := unstructured.NestedStringMap(obj.Object, "data")
	out["data"] = d
	annos := map[string]string{}
	for _, k := range sentinels.StableStampAnnotationKeys(obj.Object) {
		annos[k] = obj.GetAnnotations()[k]
	}
	out["baselines"] = annos
	return out
}

func cmData(obj *unstructured.Unstructured, key string) string {
	v, _, _ := unstructured.NestedString(obj.Object, "data", key)
	return v
}

func TestSimple_StableString_E2E(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	t0 := time.Date(2026, 10, 13, 20, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Hour)
	t2 := t0.Add(2 * time.Hour)
	t3 := t0.Add(3 * time.Hour)
	rfc := func(tm time.Time) string { return tm.Format(time.RFC3339) }

	// 1. Create at v10.1, active: both stamps written at t0.
	reconcileAt(t, cl, stableStampGraph("10.1", "active"), t0)
	cm := getStampCM(t, cl)
	assert.Equal(t, rfc(t0), cmData(cm, "createdAt"))
	assert.Equal(t, rfc(t0), cmData(cm, "lastUpgradedTime"))
	assert.Equal(t, "10.1", cmData(cm, "version"))
	assert.Len(t, sentinels.StableStampAnnotationKeys(cm.Object), 2, "one baseline annotation per stamped field")
	snap1 := snapshot(cm)

	// 2. No-op reconcile at t1: nothing moves — not even resourceVersion.
	reconcileAt(t, cl, stableStampGraph("10.1", "active"), t1)
	cm = getStampCM(t, cl)
	assert.Equal(t, rfc(t0), cmData(cm, "createdAt"))
	assert.Equal(t, rfc(t0), cmData(cm, "lastUpgradedTime"))
	// The fake client bumps resourceVersion on every SSA call even when the
	// payload is identical (unlike a real apiserver), so assert on content:
	// a steady-state reconcile must render byte-identical data + baselines.
	assert.Equal(t, snap1, snapshot(cm), "steady state must be byte-identical")

	// 3. Upgrade in flight at t2: version already reports 10.2 but state is
	//    upgrading, so the gate is closed. Version flows through (it is a
	//    plain field) but the timestamp AND its baseline stay frozen.
	reconcileAt(t, cl, stableStampGraph("10.2", "upgrading"), t2)
	cm = getStampCM(t, cl)
	assert.Equal(t, "10.2", cmData(cm, "version"))
	assert.Equal(t, rfc(t0), cmData(cm, "lastUpgradedTime"), "gated: must not stamp mid-upgrade")
	assert.Equal(t, rfc(t0), cmData(cm, "createdAt"))

	// 4. Upgrade landed at t3: active at 10.2, baseline still says 10.1 ->
	//    exactly one new stamp. createdAt is untouched forever.
	reconcileAt(t, cl, stableStampGraph("10.2", "active"), t3)
	cm = getStampCM(t, cl)
	assert.Equal(t, rfc(t3), cmData(cm, "lastUpgradedTime"), "upgrade completed: stamp once at t3")
	assert.Equal(t, rfc(t0), cmData(cm, "createdAt"), "once(): never restamped")
	snap4 := snapshot(cm)

	// 5. Another no-op at t3+1h: still t3, still no write.
	reconcileAt(t, cl, stableStampGraph("10.2", "active"), t3.Add(time.Hour))
	cm = getStampCM(t, cl)
	assert.Equal(t, rfc(t3), cmData(cm, "lastUpgradedTime"))
	assert.Equal(t, snap4, snapshot(cm))
}

// A gated field on first create is omitted entirely rather than stamped
// provisionally: "never change the timestamp while gated" includes create.
func TestSimple_StableString_GatedCreateOmitsField(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	t0 := time.Date(2026, 10, 13, 20, 0, 0, 0, time.UTC)

	reconcileAt(t, cl, stableStampGraph("10.1", "provisioning"), t0)
	cm := getStampCM(t, cl)
	_, found, _ := unstructured.NestedString(cm.Object, "data", "lastUpgradedTime")
	assert.False(t, found, "gated create must omit lastUpgradedTime")
	assert.Equal(t, t0.Format(time.RFC3339), cmData(cm, "createdAt"), "once() is ungated and still stamps")

	// Becomes active later: first stamp lands then.
	t1 := t0.Add(time.Hour)
	reconcileAt(t, cl, stableStampGraph("10.1", "active"), t1)
	cm = getStampCM(t, cl)
	assert.Equal(t, t1.Format(time.RFC3339), cmData(cm, "lastUpgradedTime"))
}
