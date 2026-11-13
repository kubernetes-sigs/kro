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

package instance

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
)

func TestResolveTargetRevision(t *testing.T) {
	tests := []struct {
		name       string
		annotation string
		strategy   v1alpha1.RolloutStrategy
		current    int64
		wantTarget int64
		wantFollow bool
		wantErr    bool
	}{
		{name: "empty/Latest/current 0", annotation: "", strategy: v1alpha1.RolloutStrategyLatest, current: 0, wantFollow: true},
		{name: "empty/Latest/current 2", annotation: "", strategy: v1alpha1.RolloutStrategyLatest, current: 2, wantFollow: true},
		{name: "empty/Manual/current 0", annotation: "", strategy: v1alpha1.RolloutStrategyManual, current: 0, wantFollow: true},
		{name: "empty/Manual/current 2 pins to current", annotation: "", strategy: v1alpha1.RolloutStrategyManual, current: 2, wantTarget: 2},

		{name: "latest/Latest/current 0", annotation: v1alpha1.GraphRevisionLatest, strategy: v1alpha1.RolloutStrategyLatest, current: 0, wantFollow: true},
		{name: "latest/Manual/current 2 still follows latest", annotation: v1alpha1.GraphRevisionLatest, strategy: v1alpha1.RolloutStrategyManual, current: 2, wantFollow: true},

		{name: "pin 3/Latest/current 0", annotation: "3", strategy: v1alpha1.RolloutStrategyLatest, current: 0, wantTarget: 3},
		{name: "pin 3/Manual/current 2 overrides current", annotation: "3", strategy: v1alpha1.RolloutStrategyManual, current: 2, wantTarget: 3},

		{name: "abc/Latest/current 0", annotation: "abc", strategy: v1alpha1.RolloutStrategyLatest, current: 0, wantErr: true},
		{name: "abc/Manual/current 2", annotation: "abc", strategy: v1alpha1.RolloutStrategyManual, current: 2, wantErr: true},
		{name: "zero/Latest/current 0", annotation: "0", strategy: v1alpha1.RolloutStrategyLatest, current: 0, wantErr: true},
		{name: "zero/Manual/current 2", annotation: "0", strategy: v1alpha1.RolloutStrategyManual, current: 2, wantErr: true},
		{name: "negative", annotation: "-1", strategy: v1alpha1.RolloutStrategyLatest, current: 2, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, followLatest, err := resolveTargetRevision(tt.annotation, tt.strategy, tt.current)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), v1alpha1.InstanceGraphRevisionAnnotation)
				assert.False(t, followLatest)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantFollow, followLatest)
			if !tt.wantFollow {
				assert.Equal(t, tt.wantTarget, target)
			}
		})
	}
}

func TestCurrentGraphRevision(t *testing.T) {
	tests := []struct {
		name   string
		status any
		want   int64
	}{
		{name: "no status", status: nil, want: 0},
		{name: "status without the field", status: map[string]any{"state": "Active"}, want: 0},
		{name: "int64", status: map[string]any{"graphRevision": int64(4)}, want: 4},
		{name: "float64 (JSON round trip)", status: map[string]any{"graphRevision": float64(7)}, want: 7},
		{name: "int", status: map[string]any{"graphRevision": 2}, want: 2},
		{name: "string is ignored", status: map[string]any{"graphRevision": "3"}, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := newInstanceObject("demo", "default")
			if tt.status != nil {
				inst.Object["status"] = tt.status
			}
			assert.Equal(t, tt.want, currentGraphRevision(inst))
		})
	}
}

// storedConfigMapNames lists the ConfigMaps the engine created, which
// identifies the revision the instance reconciled against.
func storedConfigMapNames(t *testing.T, rc client.Client) []string {
	t.Helper()

	var list corev1.ConfigMapList
	require.NoError(t, rc.List(context.Background(), &list))
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.Name)
	}
	return names
}

func statusInt64(t *testing.T, obj *unstructured.Unstructured, field string) (int64, bool) {
	t.Helper()

	val, found, err := unstructured.NestedInt64(obj.Object, "status", field)
	require.NoError(t, err)
	return val, found
}

// twoRevisionController wires a controller whose registry holds revision 1
// (ConfigMap cm-rev1) and revision 2 (ConfigMap cm-rev2), both Active.
func twoRevisionController(t *testing.T, inst *unstructured.Unstructured) (*Controller, client.Client, func() *unstructured.Unstructured) {
	t.Helper()

	raw := newControllerTestDynamicClient(t, inst.DeepCopy())
	rc := newFakeRuntimeClient(t)
	c, _ := newGraphEngineControllerUnderTest(
		t,
		raw,
		testRGDSpecWithConfigMap("cm-rev1", ""),
		revisions.RevisionStateActive,
		newTestRealCompiler(t),
		rc,
		revisions.Entry{
			Revision: 2,
			State:    revisions.RevisionStateActive,
			RGDSpec:  testRGDSpecWithConfigMap("cm-rev2", ""),
		},
	)
	return c, rc, func() *unstructured.Unstructured { return getStoredParentObject(t, raw) }
}

func TestReconcileViaGraphEngine_RevisionPinning(t *testing.T) {
	t.Run("annotation pins the instance to an older revision", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, true)

		inst := newInstanceObject("demo", "default")
		inst.SetAnnotations(map[string]string{v1alpha1.InstanceGraphRevisionAnnotation: "1"})
		c, rc, stored := twoRevisionController(t, inst)

		require.NoError(t, c.reconcileViaGraphEngine(context.Background(), inst, &fakeInstanceWatcher{}))

		assert.Equal(t, []string{"cm-rev1"}, storedConfigMapNames(t, rc))

		obj := stored()
		cur, found := statusInt64(t, obj, "graphRevision")
		require.True(t, found, "status.graphRevision must be written")
		assert.Equal(t, int64(1), cur)
		target, found := statusInt64(t, obj, "targetGraphRevision")
		require.True(t, found, "status.targetGraphRevision must be written")
		assert.Equal(t, int64(1), target)
	})

	t.Run("Manual strategy holds the instance on its recorded revision", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, true)

		inst := newInstanceObject("demo", "default")
		inst.Object["status"] = map[string]any{"graphRevision": int64(1)}
		c, rc, stored := twoRevisionController(t, inst)
		c.reconcileConfig.RolloutStrategy = v1alpha1.RolloutStrategyManual

		require.NoError(t, c.reconcileViaGraphEngine(context.Background(), inst, &fakeInstanceWatcher{}))

		assert.Equal(t, []string{"cm-rev1"}, storedConfigMapNames(t, rc))
		cur, _ := statusInt64(t, stored(), "graphRevision")
		assert.Equal(t, int64(1), cur)
	})

	t.Run("Latest strategy without a pin moves to the latest revision", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, true)

		inst := newInstanceObject("demo", "default")
		inst.Object["status"] = map[string]any{"graphRevision": int64(1)}
		c, rc, stored := twoRevisionController(t, inst)

		require.NoError(t, c.reconcileViaGraphEngine(context.Background(), inst, &fakeInstanceWatcher{}))

		assert.Equal(t, []string{"cm-rev2"}, storedConfigMapNames(t, rc))
		cur, _ := statusInt64(t, stored(), "graphRevision")
		assert.Equal(t, int64(2), cur)
	})

	t.Run("pin to a missing revision reports GraphRevisionUnavailable and applies nothing", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, true)

		inst := newInstanceObject("demo", "default")
		inst.SetAnnotations(map[string]string{v1alpha1.InstanceGraphRevisionAnnotation: "7"})
		c, rc, stored := twoRevisionController(t, inst)

		err := c.reconcileViaGraphEngine(context.Background(), inst, &fakeInstanceWatcher{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "revision 7 not available")

		assert.Empty(t, storedConfigMapNames(t, rc))

		obj := stored()
		cond := conditionByType(t, obj, GraphResolved)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		require.NotNil(t, cond.Reason)
		assert.Equal(t, "GraphRevisionUnavailable", *cond.Reason)
		target, found := statusInt64(t, obj, "targetGraphRevision")
		require.True(t, found)
		assert.Equal(t, int64(7), target)
	})

	t.Run("invalid annotation is terminal and names the annotation", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, true)

		inst := newInstanceObject("demo", "default")
		inst.SetAnnotations(map[string]string{v1alpha1.InstanceGraphRevisionAnnotation: "abc"})
		c, rc, stored := twoRevisionController(t, inst)

		err := c.reconcileViaGraphEngine(context.Background(), inst, &fakeInstanceWatcher{})
		require.Error(t, err)
		assert.Empty(t, storedConfigMapNames(t, rc))

		cond := conditionByType(t, stored(), GraphResolved)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		require.NotNil(t, cond.Reason)
		assert.Equal(t, "GraphRevisionUnavailable", *cond.Reason)
	})

	t.Run("gate off ignores the annotation and writes no revision fields", func(t *testing.T) {
		inst := newInstanceObject("demo", "default")
		inst.SetAnnotations(map[string]string{v1alpha1.InstanceGraphRevisionAnnotation: "1"})
		c, rc, stored := twoRevisionController(t, inst)

		require.NoError(t, c.reconcileViaGraphEngine(context.Background(), inst, &fakeInstanceWatcher{}))

		assert.Equal(t, []string{"cm-rev2"}, storedConfigMapNames(t, rc))

		obj := stored()
		_, found := statusInt64(t, obj, "graphRevision")
		assert.False(t, found, "status.graphRevision must not be written with the gate off")
		_, found = statusInt64(t, obj, "targetGraphRevision")
		assert.False(t, found, "status.targetGraphRevision must not be written with the gate off")
	})
}
