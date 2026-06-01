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
	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/library"
	"github.com/kubernetes-sigs/kro/pkg/graph"
)

// compileConditionExpr compiles a single CEL condition expression against
// an env that has the runtime library and a `schema` variable. Mirrors what
// the builder does at RGD compile time.
func compileConditionExpr(t *testing.T, expr string) *krocel.Expression {
	t.Helper()
	env, err := cel.NewEnv(
		cel.Variable("schema", cel.AnyType),
		library.Runtime(),
	)
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

// graphWithConditions builds a minimal *graph.Graph whose instance node
// carries the supplied condition expressions. No resource nodes; `schema`
// is the only dep.
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

	conds, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
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
	conds, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
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
	// Author condition reads schema.spec.healthy to derive its status.
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

	conds, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.NoError(t, err)
	require.Len(t, conds, 1)
	assert.Equal(t, "True", conds[0].Status)
}

func TestEvaluateConditions_DataPendingOmittedSilently(t *testing.T) {
	// schema.spec.missing isn't populated; CEL surfaces "no such key",
	// which kro classifies as DataPending. The offending condition
	// is silently omitted; the reconcile is not failed.
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

	conds, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.NoError(t, err, "Should silently omit DataPending failures, not fail the reconcile")
	require.Len(t, conds, 1, "the OK condition should be present; Pending should be omitted")
	assert.Equal(t, "OK", conds[0].ConditionType)
}

// TestEvaluateConditions_CollectionExpansion verifies that a single
// author entry whose CEL expression returns list(Condition) (rather
// than a single Condition) gets flattened into multiple status
// conditions on the instance.
//
// Example: schema.spec.regions.map(r, runtime.newCondition({...}))
// produces one Condition per region; all of them appear on the wire.
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

	conds, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.NoError(t, err)
	require.Len(t, conds, 3, "single map() expression should flatten to 3 conditions, one per region")

	// Order matches input; type names are dynamically built from each region.
	assert.Equal(t, "Region-us-east-1-Ready", conds[0].ConditionType)
	assert.Equal(t, "Region-eu-west-1-Ready", conds[1].ConditionType)
	assert.Equal(t, "Region-ap-south-1-Ready", conds[2].ConditionType)

	// Sanity check on field propagation.
	for _, c := range conds {
		assert.Equal(t, "True", c.Status)
		assert.Equal(t, "Running", c.Reason)
		assert.Contains(t, c.Message, "is healthy")
	}
}

// TestEvaluateConditions_CollectionExpansionMixedWithSingle verifies that
// when one expression returns Condition and another returns list(Condition),
// kro flattens correctly and preserves declaration order.
func TestEvaluateConditions_CollectionExpansionMixedWithSingle(t *testing.T) {
	exprs := []*krocel.Expression{
		// Single Condition.
		compileConditionExpr(t, `runtime.newCondition({type: 'Overall', status: 'True', reason: 'OK', message: ''})`),
		// list(Condition) via map().
		compileConditionExpr(t, `schema.spec.regions.map(r, runtime.newCondition({
			type: 'Region' + r + 'Ready', status: 'True', reason: '', message: '',
		}))`),
		// Another single Condition.
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

	conds, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.NoError(t, err)
	require.Len(t, conds, 4, "Overall + 2 from map() + Final = 4")

	assert.Equal(t, "Overall", conds[0].ConditionType)
	assert.Equal(t, "RegionaReady", conds[1].ConditionType)
	assert.Equal(t, "RegionbReady", conds[2].ConditionType)
	assert.Equal(t, "Final", conds[3].ConditionType)
}

// TestEvaluateConditions_CollectionExpansionEmpty handles the edge case
// where the iteration source is an empty list. Should produce zero
// conditions from that expression with no error.
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

	conds, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
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

	conds, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.NoError(t, err)
	require.Len(t, conds, 3)
	assert.Equal(t, "A", conds[0].ConditionType)
	assert.Equal(t, "B", conds[1].ConditionType)
	assert.Equal(t, "C", conds[2].ConditionType)
}

// TestEvaluateConditions_RuntimeConditionReadsKroBuiltins verifies
// runtime.condition(schema, 'X') for the four reserved kro types
// returns kro's INTERNAL value, not whatever happens to be on the
// instance's wire conditions. This is what makes patterns like
// runtime.condition(schema, 'ResourcesReady').status == 'True' && ...
// actually work.
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
	conds, err := rt.Instance().EvaluateConditions(logr.Discard(), kroBuiltins)
	require.NoError(t, err)
	require.Len(t, conds, 1)
	assert.Equal(t, "Ready", conds[0].ConditionType)
	assert.Equal(t, "True", conds[0].Status, "ResourcesReady=True must surface through runtime.condition lookup")
	assert.Equal(t, "AllHealthy", conds[0].Reason)
}

// Lookup of an unknown type returns an empty Condition
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

	conds, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.NoError(t, err)
	require.Len(t, conds, 1)
	assert.Equal(t, "True", conds[0].Status)
}

// Author declares a condition whose type matches one of kro's reserved
// names (e.g. 'ResourcesReady') AND a separate condition that reads kro's
// internal value via runtime.condition(schema, 'ResourcesReady'). The
// lookup must reflect kroBuiltins, NOT the author's overriding declaration
func TestEvaluateConditions_AuthorOverrideDoesNotShadowKroBuiltinLookup(t *testing.T) {
	// Author claims ResourcesReady=True...
	override := compileConditionExpr(t, `runtime.newCondition({
		type: 'ResourcesReady',
		status: 'True',
		reason: 'AuthorOverride',
		message: '',
	})`)
	// ...but a sibling condition that reads kro's internal value should
	// see kroBuiltins (False), not the author's override.
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
	conds, err := rt.Instance().EvaluateConditions(logr.Discard(), kroBuiltins)
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

// Duplicate condition types are dropped from the result and the
// degraded-evaluation sentinel error is returned. Surviving conditions
// (those with no duplication) are kept so the wire still carries them.
func TestEvaluateConditions_DuplicateTypeDroppedAndDegraded(t *testing.T) {
	exprs := []*krocel.Expression{
		compileConditionExpr(t, `runtime.newCondition({type: 'A', status: 'True', reason: '', message: ''})`),
		compileConditionExpr(t, `runtime.newCondition({type: 'X', status: 'True', reason: '', message: ''})`),
		compileConditionExpr(t, `runtime.newCondition({type: 'X', status: 'False', reason: '', message: ''})`),
		compileConditionExpr(t, `runtime.newCondition({type: 'B', status: 'True', reason: '', message: ''})`),
	}
	rt, err := FromGraph(graphWithConditions(exprs), testInstance("demo"), graph.RGDConfig{})
	require.NoError(t, err)

	conds, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConditionEvaluationDegraded)
	assert.Contains(t, err.Error(), "duplicate condition type")
	assert.Contains(t, err.Error(), "X")

	// Both copies of X are dropped (no principled tiebreaker between them).
	// A and B survive and surface to the caller in their original order.
	require.Len(t, conds, 2)
	assert.Equal(t, "A", conds[0].ConditionType)
	assert.Equal(t, "B", conds[1].ConditionType)
}

// Collection expansion can also produce duplicates dynamically. Those are
// dropped at runtime in the same manner as static dups.
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

	conds, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConditionEvaluationDegraded)
	require.Len(t, conds, 1)
	assert.Equal(t, "Survivor", conds[0].ConditionType)
}

// Fatal CEL errors in one expression must not abort the entire result —
// the failing expression is dropped, ErrConditionEvaluationDegraded is
// returned, and the other expressions still produce conditions.
func TestEvaluateConditions_FatalErrorInOneExpressionSkippedNotAborted(t *testing.T) {
	bad := compileConditionExpr(t, `'not a condition'`)
	bad.References = []string{library.RuntimeVarName, "schema"}

	good := compileConditionExpr(t, `runtime.newCondition({type: 'Good', status: 'True', reason: '', message: ''})`)

	rt, err := FromGraph(graphWithConditions([]*krocel.Expression{bad, good}), testInstance("demo"), graph.RGDConfig{})
	require.NoError(t, err)

	conds, err := rt.Instance().EvaluateConditions(logr.Discard(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConditionEvaluationDegraded)
	require.Len(t, conds, 1)
	assert.Equal(t, "Good", conds[0].ConditionType,
		"the well-formed condition must still surface even when a sibling expression fails")
}
