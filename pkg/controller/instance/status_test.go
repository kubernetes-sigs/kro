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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/library"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

func TestConditionsMarkerAndInitialStatus(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	wrapper := &unstructuredWrapper{instance}

	wrapper.SetConditions([]v1alpha1.Condition{{
		Type:   v1alpha1.ConditionType(Ready),
		Status: metav1.ConditionTrue,
	}})
	conditions := wrapper.GetConditions()
	require.Len(t, conditions, 1)
	assert.Equal(t, v1alpha1.ConditionType(Ready), conditions[0].Type)

	marker := NewConditionsMarkerFor(instance)
	marker.InstanceManaged()
	marker.GraphResolved()
	marker.ResourcesReady()

	rcx := &ReconcileContext{
		Instance:     instance,
		StateManager: &StateManager{State: v1alpha1.InstanceStateInProgress},
	}
	status := rcx.initialStatus()
	assert.Equal(t, string(v1alpha1.InstanceStateActive), status["state"])

	marker.ResourcesNotReady("not yet")
	marker.ResourcesUnderDeletion("cleanup")
	marker.InstanceNotManaged("nope")
	marker.GraphResolutionFailed("bad graph")

	rcx.StateManager.State = v1alpha1.InstanceStateDeleting
	status = rcx.initialStatus()
	assert.Equal(t, string(v1alpha1.InstanceStateDeleting), status["state"])

	assert.Equal(t, metav1.ConditionFalse, conditionByType(t, instance, InstanceManaged).Status)
	assert.Equal(t, metav1.ConditionFalse, conditionByType(t, instance, GraphResolved).Status)
	assert.Equal(t, metav1.ConditionUnknown, conditionByType(t, instance, ResourcesReady).Status)
}

func TestUpdateStatusPaths(t *testing.T) {
	tests := []struct {
		name      string
		badExpr   bool
		wantURL   string
		wantState string
		wantErr   string
	}{
		{
			name:      "copies resolved status fields but preserves reserved keys",
			wantURL:   "https://demo",
			wantState: string(v1alpha1.InstanceStateDeleting),
		},
		{
			name:    "returns instance desired resolution error",
			badExpr: true,
			wantErr: "division by zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instance := newInstanceObject("demo", "default")

			instanceNode := &graph.Node{
				Meta: graph.NodeMeta{
					ID:         graph.InstanceNodeID,
					Type:       graph.NodeTypeInstance,
					GVR:        controllerTestParentGVR,
					Namespaced: true,
				},
			}
			if tt.badExpr {
				instanceNode.Template = &unstructured.Unstructured{
					Object: map[string]interface{}{
						"status": map[string]interface{}{
							"bad": "${1 / 0}",
						},
					},
				}
				instanceNode.Variables = []*variable.ResourceField{
					standaloneField("status.bad", mustCompileControllerExpr(t, "1 / 0"), variable.ResourceVariableKindStatic),
				}
			} else {
				instanceNode.Template = &unstructured.Unstructured{
					Object: map[string]interface{}{
						"status": map[string]interface{}{
							"url":        "${'https://demo'}",
							"state":      "${'OVERRIDE'}",
							"conditions": "${['bad']}",
						},
					},
				}
				instanceNode.Variables = []*variable.ResourceField{
					standaloneField("status.url", mustCompileControllerExpr(t, "'https://demo'"), variable.ResourceVariableKindStatic),
					standaloneField("status.state", mustCompileControllerExpr(t, "'OVERRIDE'"), variable.ResourceVariableKindStatic),
					standaloneField("status.conditions", mustCompileControllerExpr(t, "['bad']"), variable.ResourceVariableKindStatic),
				}
			}

			controller, rcx, raw := newControllerAndContext(t, instance, newTestGraphWithInstance(instanceNode))
			rcx.StateManager.State = v1alpha1.InstanceStateDeleting

			err := controller.updateStatus(rcx)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			stored := getStoredParentObject(t, raw)

			url, found, err := unstructured.NestedString(stored.Object, "status", "url")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, tt.wantURL, url)

			state, found, err := unstructured.NestedString(stored.Object, "status", "state")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, tt.wantState, state)

			conditions, found, err := unstructured.NestedSlice(stored.Object, "status", "conditions")
			require.NoError(t, err)
			require.True(t, found)
			assert.NotEqual(t, []interface{}{"bad"}, conditions)
		})
	}
}

func TestUpdateStatusMirrorsPersistedConditionsOntoInstance(t *testing.T) {
	instance := newInstanceObject("demo", "default")

	instanceNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         graph.InstanceNodeID,
			Type:       graph.NodeTypeInstance,
			GVR:        controllerTestParentGVR,
			Namespaced: true,
		},
		Template: &unstructured.Unstructured{
			Object: map[string]interface{}{"status": map[string]interface{}{}},
		},
		Conditions: []*krocel.Expression{
			mustCompileControllerExpr(t,
				`runtime.newCondition({type: 'PrimaryReady', status: 'True', reason: 'Healthy', message: 'all good'})`,
				library.RuntimeVarName,
			),
		},
	}

	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraphWithInstance(instanceNode))
	rcx.StateManager.State = v1alpha1.InstanceStateActive

	rcx.Mark.InstanceManaged()
	rcx.Mark.GraphResolved()
	rcx.Mark.ResourcesReady()

	require.NoError(t, controller.updateStatus(rcx))

	instanceConditions := conditionsFromInstance(rcx.Instance)
	require.Len(t, instanceConditions, 1,
		"only the author condition should be on rcx.Instance; built-ins are suppressed from the wire")
	assert.Equal(t, v1alpha1.ConditionType("PrimaryReady"), instanceConditions[0].Type,
		"rcx.Instance must carry the author condition the emitter reports")
	assert.Equal(t, metav1.ConditionTrue, instanceConditions[0].Status)

	stored := getStoredParentObject(t, raw)
	assert.Equal(t,
		conditionsFromInstance(stored),
		instanceConditions,
		"rcx.Instance conditions must match the persisted wire conditions")
}

func TestUpdateStatusKeepsBuiltinConditionsWhenNoAuthorConditions(t *testing.T) {
	instance := newInstanceObject("demo", "default")

	instanceNode := &graph.Node{
		Meta: graph.NodeMeta{
			ID:         graph.InstanceNodeID,
			Type:       graph.NodeTypeInstance,
			GVR:        controllerTestParentGVR,
			Namespaced: true,
		},
		Template: &unstructured.Unstructured{
			Object: map[string]interface{}{"status": map[string]interface{}{}},
		},
	}

	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraphWithInstance(instanceNode))
	rcx.StateManager.State = v1alpha1.InstanceStateActive

	rcx.Mark.InstanceManaged()
	rcx.Mark.GraphResolved()
	rcx.Mark.ResourcesReady()

	require.NoError(t, controller.updateStatus(rcx))

	instanceConditions := conditionsFromInstance(rcx.Instance)
	types := make(map[v1alpha1.ConditionType]struct{}, len(instanceConditions))
	for _, c := range instanceConditions {
		types[c.Type] = struct{}{}
	}
	for _, builtin := range []string{InstanceManaged, GraphResolved, ResourcesReady, Ready} {
		assert.Contains(t, types, v1alpha1.ConditionType(builtin),
			"built-in %q should remain on rcx.Instance when no author conditions are declared", builtin)
	}

	assert.Equal(t, conditionsFromInstance(getStoredParentObject(t, raw)), instanceConditions,
		"rcx.Instance conditions must match the persisted wire conditions")
}

func TestStampAuthorConditionsNewWire(t *testing.T) {
	authored := []library.Condition{
		{ConditionType: "PrimaryReady", Status: "True", Reason: "Healthy", Message: "all good"},
		{ConditionType: "AppReady", Status: "False", Reason: "Init", Message: "starting"},
	}
	const generation int64 = 7

	stamped := stampAuthorConditions(authored, nil, generation)
	require.Len(t, stamped, 2)

	for _, c := range stamped {
		assert.Equal(t, generation, c.ObservedGeneration, "%s should have ObservedGeneration set", c.Type)
		require.NotNil(t, c.LastTransitionTime, "%s should have LastTransitionTime set on first appearance", c.Type)
	}
	assert.Equal(t, v1alpha1.ConditionType("PrimaryReady"), stamped[0].Type)
	assert.Equal(t, metav1.ConditionTrue, stamped[0].Status)
	require.NotNil(t, stamped[0].Reason)
	assert.Equal(t, "Healthy", *stamped[0].Reason)
}

func TestStampAuthorConditionsPreservesLastTransitionTimeWhenStatusUnchanged(t *testing.T) {
	earlier := metav1.NewTime(metav1.Now().Add(-1 * 60 * 60 * 1e9))
	previous := []v1alpha1.Condition{
		{
			Type:               "PrimaryReady",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: &earlier,
			ObservedGeneration: 1,
		},
	}
	authored := []library.Condition{
		{ConditionType: "PrimaryReady", Status: "True", Reason: "still healthy"},
	}

	stamped := stampAuthorConditions(authored, previous, 2)
	require.Len(t, stamped, 1)
	require.NotNil(t, stamped[0].LastTransitionTime)
	assert.Equal(t, earlier, *stamped[0].LastTransitionTime,
		"LastTransitionTime should be preserved when status is unchanged")
	assert.Equal(t, int64(2), stamped[0].ObservedGeneration,
		"ObservedGeneration should reflect the current generation even when status is unchanged")
}

func TestStampAuthorConditionsAdvancesLastTransitionTimeOnStatusChange(t *testing.T) {
	earlier := metav1.NewTime(metav1.Now().Add(-1 * 60 * 60 * 1e9))
	previous := []v1alpha1.Condition{
		{
			Type:               "PrimaryReady",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: &earlier,
		},
	}
	authored := []library.Condition{
		{ConditionType: "PrimaryReady", Status: "True"},
	}

	stamped := stampAuthorConditions(authored, previous, 1)
	require.Len(t, stamped, 1)
	require.NotNil(t, stamped[0].LastTransitionTime)
	assert.True(t, stamped[0].LastTransitionTime.Time.After(earlier.Time),
		"LastTransitionTime should advance when status flipped from False to True")
}

func TestStampAuthorConditionsEmptyReasonAndMessage(t *testing.T) {
	authored := []library.Condition{
		{ConditionType: "T", Status: "True"},
	}
	stamped := stampAuthorConditions(authored, nil, 1)
	require.Len(t, stamped, 1)
	assert.Nil(t, stamped[0].Reason, "empty reason should serialize as nil pointer")
	assert.Nil(t, stamped[0].Message, "empty message should serialize as nil pointer")
}

// authorConditionsInstanceNode returns an instance node declaring the given
// author condition expressions.
func authorConditionsInstanceNode(t *testing.T, exprs ...*krocel.Expression) *graph.Node {
	t.Helper()
	return &graph.Node{
		Meta: graph.NodeMeta{
			ID:         graph.InstanceNodeID,
			Type:       graph.NodeTypeInstance,
			GVR:        controllerTestParentGVR,
			Namespaced: true,
		},
		Template: &unstructured.Unstructured{
			Object: map[string]interface{}{"status": map[string]interface{}{}},
		},
		Conditions: exprs,
	}
}

// TestUpdateStatusPreservesLastTransitionTimeForBuiltinOverride is a
// regression test for the reconcile hot loop: an author condition overriding
// a built-in type must preserve its wire lastTransitionTime even though the
// markers overwrite the in-memory copy with kro's internal value.
func TestUpdateStatusPreservesLastTransitionTimeForBuiltinOverride(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	require.NoError(t, unstructured.SetNestedField(instance.Object, int64(1), "metadata", "generation"))
	require.NoError(t, unstructured.SetNestedSlice(instance.Object, []interface{}{
		map[string]interface{}{
			"type":               ResourcesReady,
			"status":             "True",
			"reason":             "AuthorOverride",
			"lastTransitionTime": "2026-01-01T00:00:00Z",
			"observedGeneration": int64(1),
		},
	}, "status", "conditions"))

	instanceNode := authorConditionsInstanceNode(t, mustCompileControllerExpr(t,
		`runtime.newCondition({type: 'ResourcesReady', status: 'True', reason: 'AuthorOverride', message: ''})`,
		library.RuntimeVarName,
	))

	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraphWithInstance(instanceNode))
	rcx.StateManager.State = v1alpha1.InstanceStateInProgress

	// kro's internal ResourcesReady disagrees with the author's True; before
	// the wire-snapshot fix this bumped lastTransitionTime every reconcile.
	rcx.Mark.InstanceManaged()
	rcx.Mark.GraphResolved()
	rcx.Mark.ResourcesNotReady("resources not ready yet")

	require.NoError(t, controller.updateStatus(rcx))

	stored := getStoredParentObject(t, raw)
	conds := conditionsFromInstance(stored)
	require.Len(t, conds, 1)
	assert.Equal(t, v1alpha1.ConditionType(ResourcesReady), conds[0].Type)
	assert.Equal(t, metav1.ConditionTrue, conds[0].Status)
	require.NotNil(t, conds[0].LastTransitionTime)
	assert.Equal(t, "2026-01-01T00:00:00Z", conds[0].LastTransitionTime.UTC().Format(time.RFC3339),
		"lastTransitionTime must be preserved from the wire, not recomputed against the marker's value")
}

func TestBuiltinConditionsFiltersAuthorTypes(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	require.NoError(t, unstructured.SetNestedSlice(instance.Object, []interface{}{
		map[string]interface{}{"type": "AuthorThing", "status": "True"},
	}, "status", "conditions"))

	mark := NewConditionsMarkerFor(instance)
	mark.InstanceManaged()
	mark.GraphResolved()
	mark.ResourcesReady()

	builtins := builtinConditions(instance)
	require.Len(t, builtins, len(v1alpha1.KROBuiltinConditionTypes),
		"only kro's built-in conditions may be injected into author CEL")
	for _, c := range builtins {
		assert.Contains(t, v1alpha1.KROBuiltinConditionTypes, string(c.Type))
	}
}

// TestUpdateStatusCleanEvalRemovesStaleConditions verifies that a fully
// successful evaluation replaces the wire, so condition types no longer
// produced by any expression are cleaned up.
func TestUpdateStatusCleanEvalRemovesStaleConditions(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	require.NoError(t, unstructured.SetNestedSlice(instance.Object, []interface{}{
		map[string]interface{}{
			"type":               "Stale",
			"status":             "True",
			"reason":             "NoLongerDeclared",
			"lastTransitionTime": "2026-01-01T00:00:00Z",
		},
	}, "status", "conditions"))

	instanceNode := authorConditionsInstanceNode(t, mustCompileControllerExpr(t,
		`runtime.newCondition({type: 'Fresh', status: 'True', reason: '', message: ''})`,
		library.RuntimeVarName,
	))

	controller, rcx, raw := newControllerAndContext(t, instance, newTestGraphWithInstance(instanceNode))
	rcx.StateManager.State = v1alpha1.InstanceStateActive

	rcx.Mark.InstanceManaged()
	rcx.Mark.GraphResolved()
	rcx.Mark.ResourcesReady()

	require.NoError(t, controller.updateStatus(rcx))

	stored := getStoredParentObject(t, raw)
	conds := conditionsFromInstance(stored)
	require.Len(t, conds, 1, "a clean evaluation replaces the wire; stale types are removed")
	assert.Equal(t, v1alpha1.ConditionType("Fresh"), conds[0].Type)
}
