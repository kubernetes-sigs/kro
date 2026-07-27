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

package runtime

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/library"
	"github.com/kubernetes-sigs/kro/pkg/graph"
)

func compileConditionExpr(t *testing.T, expr string) *krocel.Expression {
	t.Helper()
	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs([]string{"schema"}))
	require.NoError(t, err)

	ast, iss := env.Compile(expr)
	require.NoError(t, iss.Err(), "compile of %q failed: %v", expr, iss.Err())

	prog, err := env.Program(ast)
	require.NoError(t, err)

	return &krocel.Expression{
		Original:   expr,
		Program:    prog,
		References: []string{library.RuntimeVarName, "schema"},
	}
}

func graphWithConditions(conditions []*krocel.Expression) *graph.Graph {
	return &graph.Graph{
		TopologicalOrder: []string{},
		Nodes:            map[string]*graph.Node{},
		Instance: &graph.Node{
			Meta:       graph.NodeMeta{ID: graph.InstanceNodeID, Type: graph.NodeTypeInstance},
			Conditions: conditions,
		},
	}
}

func TestEvaluateConditions_NoConditions(t *testing.T) {
	rt, err := FromGraph(graphWithConditions(nil), testInstance("demo"), graph.RGDConfig{})
	require.NoError(t, err)

	conds, _, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.NoError(t, err)
	assert.Empty(t, conds)
	assert.False(t, rt.Instance().HasConditions())
}

func TestEvaluateConditions_HappyPath(t *testing.T) {
	exprs := []*krocel.Expression{
		compileConditionExpr(t, `runtime.newCondition({type: 'PrimaryReady', status: 'True', reason: 'OK', message: 'all good'})`),
		compileConditionExpr(t, `runtime.newCondition({type: 'AppReady', status: 'False', reason: 'NotReady', message: ''})`),
	}
	rt, err := FromGraph(graphWithConditions(exprs), testInstance("demo"), graph.RGDConfig{})
	require.NoError(t, err)

	require.True(t, rt.Instance().HasConditions())
	conds, _, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.NoError(t, err)
	require.Len(t, conds, 2)

	assert.Equal(t, "PrimaryReady", conds[0].ConditionType)
	assert.Equal(t, "True", conds[0].Status)
	assert.Equal(t, "OK", conds[0].Reason)
	assert.Equal(t, "all good", conds[0].Message)

	assert.Equal(t, "AppReady", conds[1].ConditionType)
	assert.Equal(t, "False", conds[1].Status)
	assert.Equal(t, "NotReady", conds[1].Reason)
}

func TestEvaluateConditions_ReadsSchema(t *testing.T) {
	exprs := []*krocel.Expression{
		compileConditionExpr(t, `runtime.newCondition({type: 'AppReady', status: schema.spec.healthy ? 'True' : 'False', reason: 'check', message: ''})`),
	}

	inst := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "example.com/v1",
			"kind":       "Test",
			"metadata":   map[string]any{"name": "demo", "namespace": "default"},
			"spec":       map[string]any{"healthy": true},
		},
	}
	rt, err := FromGraph(graphWithConditions(exprs), inst, graph.RGDConfig{})
	require.NoError(t, err)

	conds, _, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.NoError(t, err)
	require.Len(t, conds, 1)
	assert.Equal(t, "True", conds[0].Status)
}

func TestEvaluateConditions_DataPendingOmittedSilently(t *testing.T) {
	exprs := []*krocel.Expression{
		compileConditionExpr(t, `runtime.newCondition({type: 'OK', status: 'True', reason: '', message: ''})`),
		compileConditionExpr(t, `runtime.newCondition({type: 'Pending', status: schema.spec.missing.value == 'X' ? 'True' : 'False', reason: '', message: ''})`),
	}
	inst := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "example.com/v1",
			"kind":       "Test",
			"metadata":   map[string]any{"name": "demo", "namespace": "default"},
			"spec":       map[string]any{},
		},
	}
	rt, err := FromGraph(graphWithConditions(exprs), inst, graph.RGDConfig{})
	require.NoError(t, err)

	conds, incomplete, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.NoError(t, err, "Should silently omit DataPending failures, not fail the reconcile")
	require.Len(t, conds, 1, "the OK condition should be present; Pending should be omitted")
	assert.Equal(t, "OK", conds[0].ConditionType)
	assert.True(t, incomplete, "the skipped expression must be reported as incomplete")
}

func TestEvaluateConditions_CollectionExpansion(t *testing.T) {
	expr := compileConditionExpr(t, `schema.spec.regions.map(r, runtime.newCondition({
		type: 'Region-' + r + '-Ready',
		status: 'True',
		reason: 'Running',
		message: 'region ' + r + ' is healthy',
	}))`)

	inst := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "example.com/v1",
			"kind":       "Test",
			"metadata":   map[string]any{"name": "demo", "namespace": "default"},
			"spec": map[string]any{
				"regions": []any{"us-east-1", "eu-west-1", "ap-south-1"},
			},
		},
	}
	rt, err := FromGraph(graphWithConditions([]*krocel.Expression{expr}), inst, graph.RGDConfig{})
	require.NoError(t, err)

	conds, _, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.NoError(t, err)
	require.Len(t, conds, 3, "single map() expression should flatten to 3 conditions, one per region")

	assert.Equal(t, "Region-us-east-1-Ready", conds[0].ConditionType)
	assert.Equal(t, "Region-eu-west-1-Ready", conds[1].ConditionType)
	assert.Equal(t, "Region-ap-south-1-Ready", conds[2].ConditionType)

	for _, c := range conds {
		assert.Equal(t, "True", c.Status)
		assert.Equal(t, "Running", c.Reason)
		assert.Contains(t, c.Message, "is healthy")
	}
}

func TestEvaluateConditions_CollectionExpansionMixedWithSingle(t *testing.T) {
	exprs := []*krocel.Expression{
		compileConditionExpr(t, `runtime.newCondition({type: 'Overall', status: 'True', reason: 'OK', message: ''})`),
		compileConditionExpr(t, `schema.spec.regions.map(r, runtime.newCondition({
			type: 'Region' + r + 'Ready', status: 'True', reason: '', message: '',
		}))`),
		compileConditionExpr(t, `runtime.newCondition({type: 'Final', status: 'True', reason: '', message: ''})`),
	}

	inst := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "example.com/v1",
			"kind":       "Test",
			"metadata":   map[string]any{"name": "demo", "namespace": "default"},
			"spec":       map[string]any{"regions": []any{"a", "b"}},
		},
	}
	rt, err := FromGraph(graphWithConditions(exprs), inst, graph.RGDConfig{})
	require.NoError(t, err)

	conds, _, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.NoError(t, err)
	require.Len(t, conds, 4, "Overall + 2 from map() + Final = 4")

	assert.Equal(t, "Overall", conds[0].ConditionType)
	assert.Equal(t, "RegionaReady", conds[1].ConditionType)
	assert.Equal(t, "RegionbReady", conds[2].ConditionType)
	assert.Equal(t, "Final", conds[3].ConditionType)
}

func TestEvaluateConditions_CollectionExpansionEmpty(t *testing.T) {
	expr := compileConditionExpr(t, `schema.spec.regions.map(r, runtime.newCondition({
		type: 'X' + r, status: 'True', reason: '', message: '',
	}))`)

	inst := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "example.com/v1",
			"kind":       "Test",
			"metadata":   map[string]any{"name": "demo", "namespace": "default"},
			"spec":       map[string]any{"regions": []any{}},
		},
	}
	rt, err := FromGraph(graphWithConditions([]*krocel.Expression{expr}), inst, graph.RGDConfig{})
	require.NoError(t, err)

	conds, _, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.NoError(t, err)
	assert.Empty(t, conds, "empty map() should produce zero conditions, not an error")
}

func TestEvaluateConditions_OrderPreserved(t *testing.T) {
	exprs := []*krocel.Expression{
		compileConditionExpr(t, `runtime.newCondition({type: 'A', status: 'True', reason: '', message: ''})`),
		compileConditionExpr(t, `runtime.newCondition({type: 'B', status: 'False', reason: '', message: ''})`),
		compileConditionExpr(t, `runtime.newCondition({type: 'C', status: 'Unknown', reason: '', message: ''})`),
	}
	rt, err := FromGraph(graphWithConditions(exprs), testInstance("demo"), graph.RGDConfig{})
	require.NoError(t, err)

	conds, _, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.NoError(t, err)
	require.Len(t, conds, 3)
	assert.Equal(t, "A", conds[0].ConditionType)
	assert.Equal(t, "B", conds[1].ConditionType)
	assert.Equal(t, "C", conds[2].ConditionType)
}

func TestEvaluateConditions_RuntimeConditionReadsKroBuiltins(t *testing.T) {
	expr := compileConditionExpr(t, `runtime.newCondition({
		type: 'Ready',
		status: runtime.condition(schema, 'ResourcesReady').status == 'True'
		  ? 'True' : 'False',
		reason: runtime.condition(schema, 'ResourcesReady').status == 'True'
		  ? 'AllHealthy' : 'NotReady',
		message: '',
	})`)
	rt, err := FromGraph(graphWithConditions([]*krocel.Expression{expr}), testInstance("demo"), graph.RGDConfig{})
	require.NoError(t, err)

	kroBuiltins := []v1alpha1.Condition{
		{Type: "InstanceManaged", Status: metav1.ConditionTrue},
		{Type: "GraphResolved", Status: metav1.ConditionTrue},
		{Type: "ResourcesReady", Status: metav1.ConditionTrue},
		{Type: "Ready", Status: metav1.ConditionTrue},
	}
	conds, _, err := rt.Instance().EvaluateConditions(logr.Discard(), kroBuiltins)
	require.NoError(t, err)
	require.Len(t, conds, 1)
	assert.Equal(t, "Ready", conds[0].ConditionType)
	assert.Equal(t, "True", conds[0].Status, "ResourcesReady=True must surface through runtime.condition lookup")
	assert.Equal(t, "AllHealthy", conds[0].Reason)
}

func TestEvaluateConditions_RuntimeConditionUnknownTypeReturnsEmpty(t *testing.T) {
	expr := compileConditionExpr(t, `runtime.newCondition({
		type: 'Marker',
		status: runtime.condition(schema, 'DoesNotExist').type == ''
		  ? 'True' : 'False',
		reason: '',
		message: '',
	})`)
	rt, err := FromGraph(graphWithConditions([]*krocel.Expression{expr}), testInstance("demo"), graph.RGDConfig{})
	require.NoError(t, err)

	conds, _, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.NoError(t, err)
	require.Len(t, conds, 1)
	assert.Equal(t, "True", conds[0].Status)
}

func TestEvaluateConditions_AuthorOverrideDoesNotShadowKroBuiltinLookup(t *testing.T) {
	override := compileConditionExpr(t, `runtime.newCondition({
		type: 'ResourcesReady',
		status: 'True',
		reason: 'AuthorOverride',
		message: '',
	})`)
	derived := compileConditionExpr(t, `runtime.newCondition({
		type: 'DerivedReady',
		status: runtime.condition(schema, 'ResourcesReady').status,
		reason: '',
		message: '',
	})`)

	rt, err := FromGraph(
		graphWithConditions([]*krocel.Expression{override, derived}),
		testInstance("demo"),
		graph.RGDConfig{},
	)
	require.NoError(t, err)

	kroBuiltins := []v1alpha1.Condition{
		{Type: "ResourcesReady", Status: metav1.ConditionFalse},
	}
	conds, _, err := rt.Instance().EvaluateConditions(logr.Discard(), kroBuiltins)
	require.NoError(t, err)
	require.Len(t, conds, 2)

	byType := map[string]library.Condition{}
	for _, c := range conds {
		byType[c.ConditionType] = c
	}
	require.Contains(t, byType, "ResourcesReady")
	require.Contains(t, byType, "DerivedReady")

	assert.Equal(t, "True", byType["ResourcesReady"].Status,
		"author's declared ResourcesReady should appear unchanged on the result")
	assert.Equal(t, "False", byType["DerivedReady"].Status,
		"runtime.condition(schema, 'ResourcesReady') must reflect kroBuiltins (False), "+
			"not the author's overriding 'True' declaration")
}

func TestEvaluateConditions_DuplicateTypeDroppedAndDegraded(t *testing.T) {
	exprs := []*krocel.Expression{
		compileConditionExpr(t, `runtime.newCondition({type: 'A', status: 'True', reason: '', message: ''})`),
		compileConditionExpr(t, `runtime.newCondition({type: 'X', status: 'True', reason: '', message: ''})`),
		compileConditionExpr(t, `runtime.newCondition({type: 'X', status: 'False', reason: '', message: ''})`),
		compileConditionExpr(t, `runtime.newCondition({type: 'B', status: 'True', reason: '', message: ''})`),
	}
	rt, err := FromGraph(graphWithConditions(exprs), testInstance("demo"), graph.RGDConfig{})
	require.NoError(t, err)

	conds, incomplete, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConditionEvaluationDegraded)
	assert.Contains(t, err.Error(), "duplicate condition type")
	assert.Contains(t, err.Error(), "X")

	require.Len(t, conds, 2)
	assert.Equal(t, "A", conds[0].ConditionType)
	assert.Equal(t, "B", conds[1].ConditionType)
	assert.True(t, incomplete, "dropped duplicates must be reported as incomplete")
}

func TestEvaluateConditions_DuplicateTypeFromCollectionExpansionDropped(t *testing.T) {
	exprs := []*krocel.Expression{
		compileConditionExpr(t, `runtime.newCondition({type: 'Survivor', status: 'True', reason: '', message: ''})`),
		compileConditionExpr(t, `schema.spec.regions.map(r, runtime.newCondition({
			type: 'StaticType', status: 'True', reason: '', message: '',
		}))`),
	}
	inst := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "example.com/v1",
			"kind":       "Test",
			"metadata":   map[string]any{"name": "demo", "namespace": "default"},
			"spec":       map[string]any{"regions": []any{"a", "b"}},
		},
	}
	rt, err := FromGraph(graphWithConditions(exprs), inst, graph.RGDConfig{})
	require.NoError(t, err)

	conds, _, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConditionEvaluationDegraded)
	require.Len(t, conds, 1)
	assert.Equal(t, "Survivor", conds[0].ConditionType)
}

func TestEvaluateConditions_FatalErrorInOneExpressionSkippedNotAborted(t *testing.T) {
	bad := compileConditionExpr(t, `'not a condition'`)
	bad.References = []string{library.RuntimeVarName, "schema"}

	good := compileConditionExpr(t, `runtime.newCondition({type: 'Good', status: 'True', reason: '', message: ''})`)

	rt, err := FromGraph(graphWithConditions([]*krocel.Expression{bad, good}), testInstance("demo"), graph.RGDConfig{})
	require.NoError(t, err)

	conds, _, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConditionEvaluationDegraded)
	require.Len(t, conds, 1)
	assert.Equal(t, "Good", conds[0].ConditionType,
		"the well-formed condition must still surface even when a sibling expression fails")
}
