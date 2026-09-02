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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func projectionInstance() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1alpha1",
		"kind":       "StatusTest",
		"metadata":   map[string]any{"name": "demo", "namespace": "default"},
		"spec":       map[string]any{"name": "myapp"},
	}}
}

func TestProjectInstanceConditions_Guards(t *testing.T) {
	rgd := buildRGDWithStatus(map[string]any{
		"conditions": []any{
			"${runtime.newCondition({type: 'Ready', status: 'True'})}",
		},
	})

	_, _, err := ProjectInstanceConditions(nil, rgd, nil, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runtime is required")

	rt := compileAndSeedRuntime(t, rgd, projectionInstance(), nil)
	_, _, err = ProjectInstanceConditions(rt, nil, nil, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rgd is required")
}

// A status block with no conditions key yields no conditions and no error, so
// an RGD that only projects plain status fields keeps kro's built-in
// conditions rather than having them replaced by an empty author set.
func TestProjectInstanceConditions_NoConditionsBlock(t *testing.T) {
	rgd := buildRGDWithStatus(map[string]any{"ready": "${cm1.data.key}"})
	rt := compileAndSeedRuntime(t, rgd, projectionInstance(), nil)

	conditions, incomplete, err := ProjectInstanceConditions(rt, rgd, nil, 0)
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

	conditions, _, err := ProjectInstanceConditions(rt, rgd, nil, 0)
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

	conditions, incomplete, err := ProjectInstanceConditions(rt, rgd, nil, 0)
	require.NoError(t, err, "pending data must not be a hard failure")
	assert.True(t, incomplete, "the caller needs to know a condition was skipped")
	assert.Empty(t, conditions)
}
