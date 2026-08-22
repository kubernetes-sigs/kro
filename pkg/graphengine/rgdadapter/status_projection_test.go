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

package rgdadapter

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

func projectionInstance() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1alpha1",
		"kind":       "StatusTest",
		"metadata":   map[string]any{"name": "demo", "namespace": "default"},
		"spec":       map[string]any{"name": "myapp"},
	}}
}

func TestProjectInstanceStatus_Guards(t *testing.T) {
	rgd := buildRGDWithStatus(map[string]any{"ready": "${cm1.data.key}"})

	_, err := ProjectInstanceStatus(nil, rgd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runtime is required")

	rt := compileAndSeedRuntime(t, rgd, projectionInstance(), nil)
	_, err = ProjectInstanceStatus(rt, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rgd is required")
}

// A status field whose dependency is not observable this cycle is dropped, and
// its resolved siblings still project. This is invariant #3 at field
// granularity: without it, one not-ready resource would blank an instance's
// entire status instead of just the field that depends on it.
func TestProjectInstanceStatus_DataPendingFieldDroppedSiblingsSurvive(t *testing.T) {
	rgd := buildRGDWithStatus(map[string]any{
		"ready":   "${cm1.data.key}",
		"pending": "${cm1.data.absent}",
	})
	rt := compileAndSeedRuntime(t, rgd, projectionInstance(), map[string]map[string]any{
		"cm1": {
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "cm1", "namespace": "default"},
			"data":     map[string]any{"key": "val"},
		},
	})

	status, err := ProjectInstanceStatus(rt, rgd)
	require.NoError(t, err, "a data-pending field must not fail the whole projection")
	assert.Equal(t, "val", status["ready"], "the resolvable sibling must still project")
	assert.NotContains(t, status, "pending",
		"a field whose dependency is unavailable must be absent, not empty")
}

// A genuine expression bug is not data-pending and must fail loudly, naming the
// offending field so the author can find it.
func TestProjectInstanceStatus_HardErrorFailsProjection(t *testing.T) {
	rgd := buildRGDWithStatus(map[string]any{
		"broken": "${cm1.data.key + 1}",
	})
	rt := compileAndSeedRuntime(t, rgd, projectionInstance(), map[string]map[string]any{
		"cm1": {
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "cm1", "namespace": "default"},
			"data":     map[string]any{"key": "val"},
		},
	})

	_, err := ProjectInstanceStatus(rt, rgd)
	require.Error(t, err, "a type error must fail rather than being treated as pending")
	assert.Contains(t, err.Error(), "broken", "the error must name the offending field path")
}

// Expression fields addressed by a dotted path build nested maps, and
// expression-free literals copy through untouched.
func TestProjectInstanceStatus_NestedPathsAndLiterals(t *testing.T) {
	rgd := buildRGDWithStatus(map[string]any{
		"net":  map[string]any{"vpc": "${cm1.data.key}"},
		"note": "set-by-author",
	})
	rt := compileAndSeedRuntime(t, rgd, projectionInstance(), map[string]map[string]any{
		"cm1": {
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "cm1", "namespace": "default"},
			"data":     map[string]any{"key": "vpc-123"},
		},
	})

	status, err := ProjectInstanceStatus(rt, rgd)
	require.NoError(t, err)

	net, ok := status["net"].(map[string]any)
	require.True(t, ok, "a dotted field path must build an intermediate map")
	assert.Equal(t, "vpc-123", net["vpc"])
	assert.Equal(t, "set-by-author", status["note"],
		"an expression-free literal must copy through unchanged")
}

// A malformed status block is a decode failure, not an empty status: silently
// projecting nothing would drop an instance's entire status on a typo.
func TestProjectInstanceStatus_MalformedStatusBlockIsAnError(t *testing.T) {
	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "statustest"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Resources: []*v1alpha1.Resource{{
				ID: "cm1",
				Template: rawResource(map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "cm1", "namespace": "default"},
				}),
			}},
			Schema: &v1alpha1.Schema{
				Spec:   apimachineryruntime.RawExtension{Raw: []byte(`{"name":{"type":"string"}}`)},
				Status: apimachineryruntime.RawExtension{Raw: []byte(`{"ready": `)},
			},
		},
	}
	// A runtime built from a valid sibling RGD is enough; the decode fails
	// before the scope is consulted.
	valid := buildRGDWithStatus(map[string]any{"ready": "${cm1.data.key}"})
	rt := compileAndSeedRuntime(t, valid, projectionInstance(), nil)

	_, err := ProjectInstanceStatus(rt, rgd)
	require.Error(t, err)
}

func TestProjectInstanceConditions_Guards(t *testing.T) {
	rgd := buildRGDWithStatus(map[string]any{
		"conditions": []any{
			"${runtime.newCondition({type: 'Ready', status: 'True'})}",
		},
	})

	_, _, err := ProjectInstanceConditions(nil, rgd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runtime is required")

	rt := compileAndSeedRuntime(t, rgd, projectionInstance(), nil)
	_, _, err = ProjectInstanceConditions(rt, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rgd is required")
}

// A status block with no conditions key yields no conditions and no error, so
// an RGD that only projects plain status fields keeps kro's built-in
// conditions rather than having them replaced by an empty author set.
func TestProjectInstanceConditions_NoConditionsBlock(t *testing.T) {
	rgd := buildRGDWithStatus(map[string]any{"ready": "${cm1.data.key}"})
	rt := compileAndSeedRuntime(t, rgd, projectionInstance(), nil)

	conditions, incomplete, err := ProjectInstanceConditions(rt, rgd, nil)
	require.NoError(t, err)
	assert.False(t, incomplete)
	assert.Empty(t, conditions)
}

// Two expressions producing the same condition type is an authoring conflict.
// Every occurrence is dropped and the caller is told the projection degraded,
// so it preserves whatever was previously persisted for that type instead of
// picking a winner by evaluation order.
func TestProjectInstanceConditions_DuplicateTypesDegrade(t *testing.T) {
	rgd := buildRGDWithStatus(map[string]any{
		"conditions": []any{
			"${runtime.newCondition({type: 'Ready', status: 'True'})}",
			"${runtime.newCondition({type: 'Ready', status: 'False'})}",
			"${runtime.newCondition({type: 'Synced', status: 'True'})}",
		},
	})
	rt := compileAndSeedRuntime(t, rgd, projectionInstance(), nil)

	conditions, _, err := ProjectInstanceConditions(rt, rgd, nil)
	require.Error(t, err, "a duplicate condition type must be reported")
	assert.True(t, errors.Is(err, ErrConditionProjectionDegraded),
		"the caller distinguishes a degraded projection from a hard failure, got %v", err)
	assert.Contains(t, err.Error(), "Ready")

	for _, c := range conditions {
		assert.NotEqual(t, "Ready", c.ConditionType,
			"both conflicting Ready entries must be dropped, not resolved by order")
	}
}

// Pending data in a condition expression is skipped silently and reported via
// incomplete, so the caller keeps the previously persisted condition instead of
// flapping it while the cluster converges.
func TestProjectInstanceConditions_DataPendingIsIncompleteNotAnError(t *testing.T) {
	rgd := buildRGDWithStatus(map[string]any{
		"conditions": []any{
			"${runtime.newCondition({type: 'Ready', status: string(cm1.data.absent)})}",
		},
	})
	rt := compileAndSeedRuntime(t, rgd, projectionInstance(), map[string]map[string]any{
		"cm1": {
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "cm1", "namespace": "default"},
			"data":     map[string]any{"key": "val"},
		},
	})

	conditions, incomplete, err := ProjectInstanceConditions(rt, rgd, nil)
	require.NoError(t, err, "pending data must not be a hard failure")
	assert.True(t, incomplete, "the caller needs to know a condition was skipped")
	assert.Empty(t, conditions)
}
