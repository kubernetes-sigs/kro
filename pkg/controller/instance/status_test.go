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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
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
	assert.Equal(t, v1alpha1.InstanceStateActive, status["state"])

	marker.ResourcesNotReady("not yet")
	marker.ResourcesUnderDeletion("cleanup")
	marker.InstanceNotManaged("nope")
	marker.GraphResolutionFailed("bad graph")

	rcx.StateManager.State = v1alpha1.InstanceStateDeleting
	status = rcx.initialStatus()
	assert.Equal(t, v1alpha1.InstanceStateDeleting, status["state"])

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
