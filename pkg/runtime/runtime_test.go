// Copyright 2025 The Kubernetes Authors.
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
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

func Test_RuntimeWorkflow(t *testing.T) {
	// 1. Setup initial resources
	instance := newTestResource(
		withObject(map[string]interface{}{
			"spec": map[string]interface{}{
				"appName": "myapp",
				"config": map[string]interface{}{
					"dbName": "prod-db",
					"port":   5432,
				},
				"secret": map[string]interface{}{
					"include": false,
					"name":    "myapp-secret",
				},
			},
		}),
		withVariables([]*variable.ResourceField{
			{
				FieldDescriptor: variable.FieldDescriptor{
					Path:                 "status.ready",
					Expressions:          []string{"deployment.status.readyReplicas > 0"},
					StandaloneExpression: true,
				},
				Kind:         variable.ResourceVariableKindDynamic,
				Dependencies: []string{"deployment"},
			},
		}),
	)

	secret := newTestResource(
		withIncludeWhenExpressions([]string{"schema.spec.secret.include == true"}),
		withObject(map[string]interface{}{
			"metadata": map[string]interface{}{
				// this should not be evaluated since the
				// resource should not be included
				"name": "${schema.spec.secret.name}",
			},
			"stringData": map[string]interface{}{
				"DB_URL": "${dburl_expr}",
			},
		}),
		withVariables([]*variable.ResourceField{
			{
				FieldDescriptor: variable.FieldDescriptor{
					Path:                 "metadata.name",
					Expressions:          []string{"schema.spec.secret.name"},
					StandaloneExpression: true,
				},
				Kind: variable.ResourceVariableKindStatic,
			},
			{
				FieldDescriptor: variable.FieldDescriptor{
					Path:                 "stringData.DB_URL",
					Expressions:          []string{"string(configmap.data.DB_NAME) + ':' + string(configmap.data.DB_PORT)"},
					StandaloneExpression: true,
				},
				Kind:         variable.ResourceVariableKindDynamic,
				Dependencies: []string{"configmap"},
			},
		}),
	)

	configMap := newTestResource(
		withObject(map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": "${configname_expr}",
			},
			"data": map[string]interface{}{
				"DB_NAME": "${dbname_expr}",
				"DB_PORT": "${dbport_expr}",
			},
		}),
		withVariables([]*variable.ResourceField{
			{
				FieldDescriptor: variable.FieldDescriptor{
					Path:                 "metadata.name",
					Expressions:          []string{"schema.spec.appName + '-config'"},
					StandaloneExpression: true,
				},
				Kind: variable.ResourceVariableKindStatic,
			},
			{
				FieldDescriptor: variable.FieldDescriptor{
					Path:                 "data.DB_NAME",
					Expressions:          []string{"schema.spec.config.dbName"},
					StandaloneExpression: true,
				},
				Kind: variable.ResourceVariableKindStatic,
			},
			{
				FieldDescriptor: variable.FieldDescriptor{
					Path:                 "data.DB_PORT",
					Expressions:          []string{"schema.spec.config.port"},
					StandaloneExpression: true,
				},
				Kind: variable.ResourceVariableKindStatic,
			},
		}),
	)

	deployment := newTestResource(
		withObject(map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": "${schema.spec.appName}",
			},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{
					"app": "${schema.spec.appName}",
				},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{
						"labels": map[string]interface{}{
							"app": "${schema.spec.appName}",
						},
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"envFrom": []interface{}{
									map[string]interface{}{
										"secretRef": map[string]interface{}{
											"name": "${secret.metadata.name}",
										},
									},
								},
							},
						},
					},
				},
			},
		}),
		withVariables([]*variable.ResourceField{
			{
				FieldDescriptor: variable.FieldDescriptor{
					Path:                 "metadata.name",
					Expressions:          []string{"schema.spec.appName"},
					StandaloneExpression: true,
				},
				Kind: variable.ResourceVariableKindStatic,
			},
			{
				FieldDescriptor: variable.FieldDescriptor{
					Path:                 "spec.template.spec.containers[0].envFrom[0].secretRef.name",
					Expressions:          []string{"secret.metadata.name"},
					StandaloneExpression: true,
				},
				Kind:         variable.ResourceVariableKindDynamic,
				Dependencies: []string{"secret"},
			},
		}),
	)

	service := newTestResource(
		withObject(map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": "${schema.spec.appName}-svc",
			},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{
					"app": "${deployment.spec.selector.app + schema.spec.appName}",
				},
			},
		}),
		withVariables([]*variable.ResourceField{
			{
				FieldDescriptor: variable.FieldDescriptor{
					Path:                 "metadata.name",
					Expressions:          []string{"schema.spec.appName"},
					StandaloneExpression: true,
				},
				Kind: variable.ResourceVariableKindStatic,
			},
			{
				FieldDescriptor: variable.FieldDescriptor{
					Path:                 "spec.selector.app",
					Expressions:          []string{"deployment.spec.selector.app + schema.spec.appName"},
					StandaloneExpression: true,
				},
				Kind:         variable.ResourceVariableKindDynamic,
				Dependencies: []string{"deployment"},
			},
		}),
	)

	resources := map[string]Resource{
		"configmap":  configMap,
		"secret":     secret,
		"deployment": deployment,
		"service":    service,
	}

	// 2. Create runtime
	rt, err := NewResourceGraphDefinitionRuntime(instance, resources, []string{"configmap", "secret", "deployment", "service"})
	if err != nil {
		t.Fatalf("NewResourceGraphDefinitionRuntime() error = %v", err)
	}

	// 3. First sync - should resolve static variables
	cont, err := rt.Synchronize()
	if err != nil {
		t.Fatalf("First Synchronize() error = %v", err)
	}
	if !cont {
		t.Error("First Synchronize() should return true as not everything is resolved")
	}

	// Verify ConfigMap static variables resolved
	obj, state := rt.GetResource("configmap")
	if state != ResourceStateResolved {
		t.Error("ConfigMap should be ready for processing")
	}
	if obj == nil {
		t.Fatal("ConfigMap object should not be nil")
	}
	if obj.Object["metadata"].(map[string]interface{})["name"] != "myapp-config" {
		t.Error("ConfigMap name not resolved")
	}

	// 4. Set ConfigMap as resolved
	rt.SetResource("configmap", &unstructured.Unstructured{
		Object: map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": "myapp-config",
			},
			"data": map[string]interface{}{
				"DB_NAME": "prod-db",
				"DB_PORT": int64(5432),
			},
		},
	})

	// 5. Second sync - should resolve Secret's dynamic variables
	cont, err = rt.Synchronize()
	if err != nil {
		t.Fatalf("Second Synchronize() error = %v", err)
	}
	if !cont {
		t.Error("Second Synchronize() should return true as not everything is resolved")
	}

	// Verify Secret ready for processing
	_, state = rt.GetResource("secret")
	if state != ResourceStateResolved {
		t.Error("Secret should be ready for processing")
	}

	// 6. Set Secret as resolved
	rt.SetResource("secret", &unstructured.Unstructured{
		Object: map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": "myapp-secret",
			},
			"stringData": map[string]interface{}{
				"DB_URL": "prod-db:5432",
			},
		},
	})

	// 7. Third sync - should resolve Deployments dynamic variables
	cont, err = rt.Synchronize()
	if err != nil {
		t.Fatalf("Third Synchronize() error = %v", err)
	}
	if !cont {
		t.Error("Third Synchronize() should return true as not everything is resolved")
	}

	// Set Deployment as resolved
	rt.SetResource("deployment", &unstructured.Unstructured{
		Object: map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": "myapp",
			},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{
					"app": "myapp",
				},
			},
			"status": map[string]interface{}{
				"readyReplicas": int64(1),
			},
		},
	})

	// 8. Fourth sync - should resolve the SVC dynamic variables
	cont, err = rt.Synchronize()
	if err != nil {
		t.Fatalf("Fourth Synchronize() error = %v", err)
	}
	if !cont {
		t.Error("Fourth Synchronize() should return true as instance status not resolved")
	}

	// Verify Service ready for processing
	_, state = rt.GetResource("service")
	if state != ResourceStateResolved {
		t.Error("Service should be ready for processing")
	}

	// 9. Set Service as resolved and verify final state
	rt.SetResource("service", &unstructured.Unstructured{
		Object: map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": "myapp-svc",
			},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{
					"app": "myapp",
				},
			},
		},
	})

	// 10. Final sync - should resolve instance status
	cont, err = rt.Synchronize()
	if err != nil {
		t.Fatalf("Final Synchronize() error = %v", err)
	}
	if cont {
		t.Error("Final Synchronize() should return false as everything is resolved")
	}

	// Verify instance status updated
	if instance.Unstructured().Object["status"].(map[string]interface{})["ready"] != true {
		t.Error("Instance status not properly updated")
	}

	cont, err = rt.Synchronize()
	if err != nil {
		t.Fatalf("Final Synchronize() error = %v", err)
	}
	if cont {
		t.Error("Final Synchronize() should return false as everything is resolved")
	}
}

func Test_NewResourceGraphDefinitionRuntime(t *testing.T) {
	// Setup a test instance with a spec
	instance := newTestResource(
		withObject(map[string]interface{}{
			"spec": map[string]interface{}{
				"replicas": 3,
				"image":    "nginx:latest",
			},
		}),
		withVariables([]*variable.ResourceField{
			{
				FieldDescriptor: variable.FieldDescriptor{
					Path:                 "status.replicas",
					Expressions:          []string{"deployment.spec.replicas"},
					StandaloneExpression: true,
				},
				Kind: variable.ResourceVariableKindDynamic,
			},
		}),
	)

	// Setup test resources
	deployment := newTestResource(
		withObject(map[string]interface{}{
			"spec": map[string]interface{}{
				"replicas": "${schema.spec.replicas}",
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"image": "${schema.spec.image}",
							},
						},
					},
				},
			},
		}),
		withVariables([]*variable.ResourceField{
			{
				FieldDescriptor: variable.FieldDescriptor{
					Path:                 "spec.replicas",
					Expressions:          []string{"schema.spec.replicas"},
					StandaloneExpression: true,
				},
				Kind: variable.ResourceVariableKindStatic,
			},
			{
				FieldDescriptor: variable.FieldDescriptor{
					Path:                 "spec.template.spec.containers[0].image",
					Expressions:          []string{"schema.spec.image"},
					StandaloneExpression: true,
				},
				Kind: variable.ResourceVariableKindStatic,
			},
		}),
	)

	service := newTestResource(
		withObject(map[string]interface{}{
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{
					"app": "${deployment.spec.selector}",
				},
			},
		}),
		withVariables([]*variable.ResourceField{
			{
				FieldDescriptor: variable.FieldDescriptor{
					Path:                 "spec.selector",
					Expressions:          []string{"deployment.spec.selector"},
					StandaloneExpression: true,
				},
				Kind:         variable.ResourceVariableKindDynamic,
				Dependencies: []string{"deployment"},
			},
		}),
	)

	resources := map[string]Resource{
		"deployment": deployment,
		"service":    service,
	}

	rt, err := NewResourceGraphDefinitionRuntime(instance, resources, []string{"deployment", "service"})
	if err != nil {
		t.Fatalf("NewResourceGraphDefinitionRuntime() error = %v", err)
	}

	// Test 1: Check expressionsCache initialization
	expectedExpressions := map[string]struct{}{
		"deployment.spec.replicas": {},
		"schema.spec.replicas":     {},
		"schema.spec.image":        {},
		"deployment.spec.selector": {},
	}

	for expr := range expectedExpressions {
		if _, ok := rt.expressionsCache[expr]; !ok {
			t.Errorf("expressionsCache missing expression %s", expr)
		}
	}

	// Test 2: Check runtimeVariables initialization
	expectedVars := map[string]int{
		"instance":   1,
		"deployment": 2,
		"service":    1,
	}

	for resource, count := range expectedVars {
		if vars := rt.runtimeVariables[resource]; len(vars) != count {
			t.Errorf("runtimeVariables[%s] = %d vars, want %d", resource, len(vars), count)
		}
	}

	// Test 3: Verify static variables were evaluated
	deploymentObj := deployment.Unstructured().Object
	expectedDeployment := map[string]interface{}{
		"spec": map[string]interface{}{
			"replicas": 3,
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"image": "nginx:latest",
						},
					},
				},
			},
		},
	}

	bJSON, _ := json.MarshalIndent(deploymentObj, "", "  ")
	cJSON, _ := json.MarshalIndent(expectedDeployment, "", "  ")
	if string(bJSON) != string(cJSON) {
		t.Errorf("deployment not properly evaluated\ngot = %v\nwant= %v", deploymentObj, expectedDeployment)
	}

	// Test 4: Verify dynamic variables are NOT evaluated
	svcObj := service.Unstructured().Object
	if svcSelector := svcObj["spec"].(map[string]interface{})["selector"].(map[string]interface{})["app"]; svcSelector != "${deployment.spec.selector}" {
		t.Errorf("service selector was evaluated, should remain as expression. got = %v", svcSelector)
	}

	// Test 5: Verify no resources are resolved yet
	if len(rt.resolvedResources) != 0 {
		t.Errorf("resolvedResources should be empty, got %d entries", len(rt.resolvedResources))
	}
}

func Test_GetResource(t *testing.T) {
	tests := []struct {
		name              string
		resources         map[string]Resource
		resolvedResources map[string]*unstructured.Unstructured
		runtimeVariables  map[string][]*expressionEvaluationState
		resourceName      string
		wantObj           *unstructured.Unstructured
		wantState         ResourceState
	}{
		{
			name: "already resolved resource",
			resources: map[string]Resource{
				"test": newTestResource(
					withObject(map[string]interface{}{
						"spec": map[string]interface{}{
							"value": "original",
						},
					}),
				),
			},
			resolvedResources: map[string]*unstructured.Unstructured{
				"test": {
					Object: map[string]interface{}{
						"spec": map[string]interface{}{
							"value": "resolved",
						},
					},
				},
			},
			resourceName: "test",
			wantObj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"spec": map[string]interface{}{
						"value": "resolved",
					},
				},
			},
			wantState: ResourceStateResolved,
		},
		{
			name: "ready to process",
			resources: map[string]Resource{
				"test": newTestResource(
					withObject(map[string]interface{}{
						"spec": map[string]interface{}{
							"value": "original",
						},
					}),
				),
			},
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: true,
					},
				},
			},
			resourceName: "test",
			wantObj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"spec": map[string]interface{}{
						"value": "original",
					},
				},
			},
			wantState: ResourceStateResolved,
		},
		{
			name: "waiting on dependencies",
			resources: map[string]Resource{
				"test": newTestResource(
					withObject(map[string]interface{}{
						"spec": map[string]interface{}{
							"value": "${expr1}",
						},
					}),
					withVariables([]*variable.ResourceField{
						{
							FieldDescriptor: variable.FieldDescriptor{
								Path:        "spec.value",
								Expressions: []string{"expr1"},
							},
						},
					}),
				),
			},
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: false,
					},
				},
			},
			resourceName: "test",
			wantObj:      nil,
			wantState:    ResourceStateWaitingOnDependencies,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				resources:         tt.resources,
				resolvedResources: tt.resolvedResources,
				runtimeVariables:  tt.runtimeVariables,
			}

			gotObj, gotState := rt.GetResource(tt.resourceName)
			if gotState != tt.wantState {
				t.Errorf("GetResource() state = %v, want %v", gotState, tt.wantState)
			}

			if !reflect.DeepEqual(gotObj, tt.wantObj) {
				t.Errorf("GetResource() obj = %v, want %v", gotObj, tt.wantObj)
			}
		})
	}
}
func Test_Synchronize(t *testing.T) {
	tests := []struct {
		name              string
		instance          Resource
		resources         map[string]Resource
		resolvedResources map[string]*unstructured.Unstructured
		expressionsCache  map[string]*expressionEvaluationState
		runtimeVariables  map[string][]*expressionEvaluationState
		wantContinue      bool
		wantErr           bool
	}{
		{
			name:     "everything resolved",
			instance: newTestResource(),
			resources: map[string]Resource{
				"test": newTestResource(),
			},
			resolvedResources: map[string]*unstructured.Unstructured{
				"test": {},
			},
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression: "expr1",
					Kind:       variable.ResourceVariableKindDynamic,
					Resolved:   true,
				},
			},
			wantContinue: false,
		},
		{
			name: "unresolved dynamic variables",
			instance: newTestResource(
				withObject(map[string]interface{}{
					"spec": map[string]interface{}{
						"value": "${expr1}",
					},
				}),
			),
			resources: map[string]Resource{
				"test": newTestResource(),
			},
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:   "'unresolved'",
					Kind:         variable.ResourceVariableKindDynamic,
					Dependencies: []string{"test"},
					Resolved:     false,
				},
			},
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {
					{
						Expression:   "'unresolved'",
						Kind:         variable.ResourceVariableKindDynamic,
						Dependencies: []string{"test"},
						Resolved:     false,
					},
				},
			},
			wantContinue: true,
		},
		{
			name:     "resources not resolved yet",
			instance: newTestResource(),
			resources: map[string]Resource{
				"test": newTestResource(
					withObject(map[string]interface{}{
						"spec": map[string]interface{}{
							"value": "${expr1}",
						},
					}),
				),
			},
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:    "expr1",
					Kind:          variable.ResourceVariableKindDynamic,
					Resolved:      true,
					ResolvedValue: 42,
				},
			},
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {
					{
						Expression: "expr1",
						Kind:       variable.ResourceVariableKindDynamic,
						Resolved:   true,
					},
				},
			},
			wantContinue: true,
		},
		{
			name:     "resolving in progress",
			instance: newTestResource(),
			resources: map[string]Resource{
				"dep": newTestResource(
					withObject(map[string]interface{}{
						"spec": map[string]interface{}{
							"value": "resolved",
						},
					}),
				),
				"test": newTestResource(
					withObject(map[string]interface{}{
						"spec": map[string]interface{}{
							"value": "${expr1}",
						},
					}),
					withDependencies([]string{"dep"}),
				),
			},
			resolvedResources: map[string]*unstructured.Unstructured{
				"dep": {
					Object: map[string]interface{}{
						"spec": map[string]interface{}{
							"value": "resolved",
						},
					},
				},
			},
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:   "dep.spec.value",
					Kind:         variable.ResourceVariableKindDynamic,
					Dependencies: []string{"dep"},
					Resolved:     false,
				},
			},
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {
					{
						Expression:   "expr1",
						Kind:         variable.ResourceVariableKindDynamic,
						Dependencies: []string{"dep"},
						Resolved:     false,
					},
				},
			},
			wantContinue: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				instance:          tt.instance,
				resources:         tt.resources,
				resolvedResources: tt.resolvedResources,
				expressionsCache:  tt.expressionsCache,
				runtimeVariables:  tt.runtimeVariables,
			}

			gotContinue, err := rt.Synchronize()
			if (err != nil) != tt.wantErr {
				t.Errorf("Synchronize() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotContinue != tt.wantContinue {
				t.Errorf("Synchronize() = %v, want %v", gotContinue, tt.wantContinue)
			}
		})
	}
}

func Test_propagateResourceVariables(t *testing.T) {
	tests := []struct {
		name             string
		resources        map[string]Resource
		runtimeVariables map[string][]*expressionEvaluationState
		expressionsCache map[string]*expressionEvaluationState
		wantResources    map[string]map[string]interface{}
		wantErr          bool
	}{
		{
			name: "single resource variables",
			resources: map[string]Resource{
				"test": newTestResource(
					withObject(map[string]interface{}{
						"spec": map[string]interface{}{
							"value": "${expr1}",
						},
					}),
					withVariables([]*variable.ResourceField{
						{
							FieldDescriptor: variable.FieldDescriptor{
								Path:                 "spec.value",
								Expressions:          []string{"expr1"},
								StandaloneExpression: true,
							},
						},
					}),
				),
			},
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {
					{
						Expression: "expr1",
						Resolved:   true,
					},
				},
			},
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:    "expr1",
					Resolved:      true,
					ResolvedValue: 42,
				},
			},
			wantResources: map[string]map[string]interface{}{
				"test": {
					"spec": map[string]interface{}{
						"value": 42,
					},
				},
			},
		},
		{
			name: "dependency chain",
			resources: map[string]Resource{
				"first": newTestResource(
					withObject(map[string]interface{}{
						"spec": map[string]interface{}{
							"value": "${expr1}",
						},
					}),
					withVariables([]*variable.ResourceField{
						{
							FieldDescriptor: variable.FieldDescriptor{
								Path:                 "spec.value",
								Expressions:          []string{"expr1"},
								StandaloneExpression: true,
							},
						},
					}),
				),
				"second": newTestResource(
					withObject(map[string]interface{}{
						"spec": map[string]interface{}{
							"value": "${expr2}",
						},
					}),
					withVariables([]*variable.ResourceField{
						{
							FieldDescriptor: variable.FieldDescriptor{
								Path:                 "spec.value",
								Expressions:          []string{"expr2"},
								StandaloneExpression: true,
							},
						},
					}),
					withDependencies([]string{"first"}),
				),
			},
			runtimeVariables: map[string][]*expressionEvaluationState{
				"first": {
					{
						Expression: "expr1",
						Resolved:   true,
					},
				},
				"second": {
					{
						Expression: "expr2",
						Resolved:   true,
					},
				},
			},
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:    "expr1",
					Resolved:      true,
					ResolvedValue: 42,
				},
				"expr2": {
					Expression:    "expr2",
					Resolved:      true,
					ResolvedValue: 84,
				},
			},
			wantResources: map[string]map[string]interface{}{
				"first": {
					"spec": map[string]interface{}{
						"value": 42,
					},
				},
				"second": {
					"spec": map[string]interface{}{
						"value": 84,
					},
				},
			},
		},
		{
			name: "unresolved dependency skips evaluation",
			resources: map[string]Resource{
				"first": newTestResource(
					withObject(map[string]interface{}{
						"spec": map[string]interface{}{
							"value": "${expr1}",
						},
					}),
					withVariables([]*variable.ResourceField{
						{
							FieldDescriptor: variable.FieldDescriptor{
								Path:                 "spec.value",
								Expressions:          []string{"expr1"},
								StandaloneExpression: true,
							},
						},
					}),
				),
				"second": newTestResource(
					withObject(map[string]interface{}{
						"spec": map[string]interface{}{
							"value": "${expr2}",
						},
					}),
					withVariables([]*variable.ResourceField{
						{
							FieldDescriptor: variable.FieldDescriptor{
								Path:                 "spec.value",
								Expressions:          []string{"expr2"},
								StandaloneExpression: true,
							},
						},
					}),
					withDependencies([]string{"first"}),
				),
			},
			runtimeVariables: map[string][]*expressionEvaluationState{
				"first": {
					{
						Expression: "expr1",
						Kind:       variable.ResourceVariableKindDynamic,
						Resolved:   false,
					},
				},
				"second": {
					{
						Expression: "expr2",
						Kind:       variable.ResourceVariableKindDynamic,
						Resolved:   false,
					},
				},
			},
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression: "expr1",
					Kind:       variable.ResourceVariableKindDynamic,
					Resolved:   false,
				},
				"expr2": {
					Expression: "expr2",
					Kind:       variable.ResourceVariableKindDynamic,
					Resolved:   false,
				},
			},
			wantResources: map[string]map[string]interface{}{
				"first": {
					"spec": map[string]interface{}{
						"value": "${expr1}",
					},
				},
				"second": {
					"spec": map[string]interface{}{
						"value": "${expr2}",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				resources:        tt.resources,
				runtimeVariables: tt.runtimeVariables,
				expressionsCache: tt.expressionsCache,
			}

			err := rt.propagateResourceVariables()
			if (err != nil) != tt.wantErr {
				t.Errorf("propagateResourceVariables() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				for resourceName, wantObj := range tt.wantResources {
					got := tt.resources[resourceName].Unstructured().Object
					if !reflect.DeepEqual(got, wantObj) {
						t.Errorf("resource %s\ngot  = %v\nwant = %v",
							resourceName, got, wantObj)
					}
				}
			}

		})
	}
}

func Test_canProcessResource(t *testing.T) {
	tests := []struct {
		name             string
		resources        map[string]Resource
		runtimeVariables map[string][]*expressionEvaluationState
		resource         string
		want             bool
	}{
		{
			name: "no dependencies or variables",
			resources: map[string]Resource{
				"test": newTestResource(),
			},
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {},
			},
			resource: "test",
			want:     true,
		},
		{
			name: "dependencies resolved and variables resolved",
			resources: map[string]Resource{
				"test": newTestResource(
					withDependencies([]string{"dep1"}),
				),
				"dep1": newTestResource(),
			},
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: true,
					},
				},
				"dep1": {
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: true,
					},
				},
			},
			resource: "test",
			want:     true,
		},
		{
			name: "dependencies unresolved",
			resources: map[string]Resource{
				"test": newTestResource(
					withDependencies([]string{"dep1"}),
				),
				"dep1": newTestResource(),
			},
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: true,
					},
				},
				"dep1": {
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: false,
					},
				},
			},
			resource: "test",
			want:     false,
		},
		{
			name: "variables unresolved",
			resources: map[string]Resource{
				"test": newTestResource(
					withDependencies([]string{"dep1"}),
				),
				"dep1": newTestResource(),
			},
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: false,
					},
				},
				"dep1": {
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: true,
					},
				},
			},
			resource: "test",
			want:     false,
		},
		{
			name: "multiple dependencies all resolved",
			resources: map[string]Resource{
				"test": newTestResource(
					withDependencies([]string{"dep1", "dep2"}),
				),
				"dep1": newTestResource(),
				"dep2": newTestResource(),
			},
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: true,
					},
				},
				"dep1": {
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: true,
					},
				},
				"dep2": {
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: true,
					},
				},
			},
			resource: "test",
			want:     true,
		},
		{
			name: "multiple dependencies one unresolved",
			resources: map[string]Resource{
				"test": newTestResource(
					withDependencies([]string{"dep1", "dep2"}),
				),
				"dep1": newTestResource(),
				"dep2": newTestResource(),
			},
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: true,
					},
				},
				"dep1": {
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: true,
					},
				},
				"dep2": {
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: false,
					},
				},
			},
			resource: "test",
			want:     false,
		},
		{
			name: "only static variables",
			resources: map[string]Resource{
				"test": newTestResource(),
			},
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {
					{
						Kind:     variable.ResourceVariableKindStatic,
						Resolved: true,
					},
				},
			},
			resource: "test",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				resources:        tt.resources,
				runtimeVariables: tt.runtimeVariables,
			}

			got := rt.canProcessResource(tt.resource)
			if got != tt.want {
				t.Errorf("canProcessResource() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_resourceVariablesResolved(t *testing.T) {
	tests := []struct {
		name             string
		runtimeVariables map[string][]*expressionEvaluationState
		resource         string
		want             bool
	}{
		{
			name: "no variables",
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {},
			},
			resource: "test",
			want:     true,
		},
		{
			name: "all static resolved",
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {
					{
						Kind:     variable.ResourceVariableKindStatic,
						Resolved: true,
					},
					{
						Kind:     variable.ResourceVariableKindStatic,
						Resolved: true,
					},
				},
			},
			resource: "test",
			want:     true,
		},
		{
			name: "all dynamic resolved",
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: true,
					},
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: true,
					},
				},
			},
			resource: "test",
			want:     true,
		},
		{
			name: "mixed resolved",
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {
					{
						Kind:     variable.ResourceVariableKindStatic,
						Resolved: true,
					},
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: true,
					},
				},
			},
			resource: "test",
			want:     true,
		},
		{
			name: "unresolved dynamic",
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {
					{
						Kind:     variable.ResourceVariableKindStatic,
						Resolved: true,
					},
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: false,
					},
				},
			},
			resource: "test",
			want:     false,
		},
		{
			name: "multiple unresolved dynamic",
			runtimeVariables: map[string][]*expressionEvaluationState{
				"test": {
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: false,
					},
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: false,
					},
				},
			},
			resource: "test",
			want:     false,
		},
		{
			name: "resource not found",
			runtimeVariables: map[string][]*expressionEvaluationState{
				"other": {
					{
						Kind:     variable.ResourceVariableKindDynamic,
						Resolved: true,
					},
				},
			},
			resource: "test",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				runtimeVariables: tt.runtimeVariables,
			}

			got := rt.resourceVariablesResolved(tt.resource)
			if got != tt.want {
				t.Errorf("resourceVariablesResolved() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_evaluateStaticVariables(t *testing.T) {
	tests := []struct {
		name             string
		instance         Resource
		expressionsCache map[string]*expressionEvaluationState
		wantCache        map[string]*expressionEvaluationState
		wantErr          bool
	}{
		{
			name: "static variable evaluation",
			instance: newTestResource(
				withObject(map[string]interface{}{
					"spec": map[string]interface{}{
						"value": 42,
					},
				}),
			),
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression: "schema.spec.value",
					Kind:       variable.ResourceVariableKindStatic,
					Resolved:   false,
				},
			},
			wantCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:    "schema.spec.value",
					Kind:          variable.ResourceVariableKindStatic,
					Resolved:      true,
					ResolvedValue: int64(42),
				},
			},
		},
		{
			name: "mixed static and dynamic",
			instance: newTestResource(
				withObject(map[string]interface{}{
					"spec": map[string]interface{}{
						"value": 42,
					},
				}),
			),
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression: "schema.spec.value",
					Kind:       variable.ResourceVariableKindStatic,
					Resolved:   false,
				},
				"expr2": {
					Expression: "status.ready",
					Kind:       variable.ResourceVariableKindDynamic,
					Resolved:   false,
				},
			},
			wantCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:    "schema.spec.value",
					Kind:          variable.ResourceVariableKindStatic,
					Resolved:      true,
					ResolvedValue: int64(42),
				},
				"expr2": {
					Expression: "status.ready",
					Kind:       variable.ResourceVariableKindDynamic,
					Resolved:   false,
				},
			},
		},
		{
			name: "invalid expression",
			instance: newTestResource(
				withObject(map[string]interface{}{
					"spec": map[string]interface{}{},
				}),
			),
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression: "invalid )",
					Kind:       variable.ResourceVariableKindStatic,
					Resolved:   false,
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				instance:         tt.instance,
				expressionsCache: tt.expressionsCache,
			}

			err := rt.evaluateStaticVariables()
			if (err != nil) != tt.wantErr {
				t.Errorf("evaluateStaticVariables() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && !reflect.DeepEqual(tt.expressionsCache, tt.wantCache) {
				t.Errorf("evaluateStaticVariables() cache = %v, want %v", tt.expressionsCache, tt.wantCache)
			}
		})
	}
}

func Test_evaluateDynamicVariables(t *testing.T) {
	tests := []struct {
		name              string
		expressionsCache  map[string]*expressionEvaluationState
		resolvedResources map[string]*unstructured.Unstructured
		wantCache         map[string]*expressionEvaluationState
		wantErr           bool
	}{
		{
			name: "dynamic no dependencies",
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression: "true",
					Kind:       variable.ResourceVariableKindDynamic,
					Resolved:   false,
				},
			},
			wantCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:    "true",
					Kind:          variable.ResourceVariableKindDynamic,
					Resolved:      true,
					ResolvedValue: true,
				},
			},
		},
		{
			name: "dynamic with resolved dependency",
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:   "res1.spec.count > 0",
					Kind:         variable.ResourceVariableKindDynamic,
					Dependencies: []string{"res1"},
					Resolved:     false,
				},
			},
			resolvedResources: map[string]*unstructured.Unstructured{
				"res1": {
					Object: map[string]interface{}{
						"spec": map[string]interface{}{
							"count": 5,
						},
					},
				},
			},
			wantCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:    "res1.spec.count > 0",
					Kind:          variable.ResourceVariableKindDynamic,
					Dependencies:  []string{"res1"},
					Resolved:      true,
					ResolvedValue: true,
				},
			},
		},
		{
			name: "dynamic with unresolved dependency",
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:   "res1.spec.count > 0",
					Kind:         variable.ResourceVariableKindDynamic,
					Dependencies: []string{"res1"},
					Resolved:     false,
				},
			},
			resolvedResources: map[string]*unstructured.Unstructured{},
			wantCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:   "res1.spec.count > 0",
					Kind:         variable.ResourceVariableKindDynamic,
					Dependencies: []string{"res1"},
					Resolved:     false,
				},
			},
		},
		{
			name: "multiple dependencies all resolved",
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:   "(res1.spec.count + res2.spec.count) > 5",
					Kind:         variable.ResourceVariableKindDynamic,
					Dependencies: []string{"res1", "res2"},
					Resolved:     false,
				},
			},
			resolvedResources: map[string]*unstructured.Unstructured{
				"res1": {
					Object: map[string]interface{}{
						"spec": map[string]interface{}{
							"count": 3,
						},
					},
				},
				"res2": {
					Object: map[string]interface{}{
						"spec": map[string]interface{}{
							"count": 4,
						},
					},
				},
			},
			wantCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:    "(res1.spec.count + res2.spec.count) > 5",
					Kind:          variable.ResourceVariableKindDynamic,
					Dependencies:  []string{"res1", "res2"},
					Resolved:      true,
					ResolvedValue: true,
				},
			},
		},
		{
			name: "multiple dependencies one unresolved",
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:   "res1.spec.count + res2.spec.count > 5",
					Kind:         variable.ResourceVariableKindDynamic,
					Dependencies: []string{"res1", "res2"},
					Resolved:     false,
				},
			},
			resolvedResources: map[string]*unstructured.Unstructured{
				"res1": {
					Object: map[string]interface{}{
						"spec": map[string]interface{}{
							"count": 3,
						},
					},
				},
			},
			wantCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:   "res1.spec.count + res2.spec.count > 5",
					Kind:         variable.ResourceVariableKindDynamic,
					Dependencies: []string{"res1", "res2"},
					Resolved:     false,
				},
			},
		},
		{
			name: "3 dependencies one resolved - 1 unresolved",
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:   "res1.spec.count + res2.spec.count + res3.spec.count + res4.spec.count",
					Kind:         variable.ResourceVariableKindDynamic,
					Dependencies: []string{"res1", "res2", "res3", "res4"},
					Resolved:     false,
				},
			},
			resolvedResources: map[string]*unstructured.Unstructured{
				"res1": {
					Object: map[string]interface{}{
						"spec": map[string]interface{}{
							"count": 3,
						},
					},
				},
				"res2": {
					Object: map[string]interface{}{
						"spec": map[string]interface{}{
							"count": 3,
						},
					},
				},
				"res3": {
					Object: map[string]interface{}{
						"spec": map[string]interface{}{
							"count": 3,
						},
					},
				},
			},
			wantCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:   "res1.spec.count + res2.spec.count + res3.spec.count + res4.spec.count",
					Kind:         variable.ResourceVariableKindDynamic,
					Dependencies: []string{"res1", "res2", "res3", "res4"},
					Resolved:     false,
				},
			},
		},
		{
			name: "mix static and dynamic",
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:   "res1.spec.count > 0",
					Kind:         variable.ResourceVariableKindDynamic,
					Dependencies: []string{"res1"},
					Resolved:     false,
				},
				"expr2": {
					Expression: "true",
					Kind:       variable.ResourceVariableKindStatic,
					Resolved:   false,
				},
			},
			resolvedResources: map[string]*unstructured.Unstructured{
				"res1": {
					Object: map[string]interface{}{
						"spec": map[string]interface{}{
							"count": 5,
						},
					},
				},
			},
			wantCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:    "res1.spec.count > 0",
					Kind:          variable.ResourceVariableKindDynamic,
					Dependencies:  []string{"res1"},
					Resolved:      true,
					ResolvedValue: true,
				},
				"expr2": {
					Expression: "true",
					Kind:       variable.ResourceVariableKindStatic,
					Resolved:   false,
				},
			},
		},
		{
			name: "invalid expression",
			expressionsCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression: "invalid )",
					Kind:       variable.ResourceVariableKindDynamic,
					Resolved:   false,
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				instance: newTestResource(
					withObject(map[string]interface{}{}),
				),
				expressionsCache:  tt.expressionsCache,
				resolvedResources: tt.resolvedResources,
			}

			err := rt.evaluateDynamicVariables()
			if (err != nil) != tt.wantErr {
				t.Errorf("evaluateDynamicVariables() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && !reflect.DeepEqual(tt.expressionsCache, tt.wantCache) {
				t.Errorf("evaluateDynamicVariables() cache = %v, want %v", tt.expressionsCache, tt.wantCache)
			}
		})
	}
}

func Test_evaluateInstanceStatuses(t *testing.T) {
	tests := []struct {
		name     string
		instance Resource
		expCache map[string]*expressionEvaluationState
		wantObj  map[string]interface{}
		wantErr  bool
	}{
		{
			name: "simple status update",
			instance: newTestResource(
				withObject(map[string]interface{}{
					"status": map[string]interface{}{
						"ready": "${expr1}",
					},
				}),
				withVariables([]*variable.ResourceField{
					{
						FieldDescriptor: variable.FieldDescriptor{
							Path:                 "status.ready",
							Expressions:          []string{"expr1"},
							StandaloneExpression: true,
						},
					},
				}),
			),
			expCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:    "expr1",
					Resolved:      true,
					ResolvedValue: true,
				},
			},
			wantObj: map[string]interface{}{
				"status": map[string]interface{}{
					"ready": true,
				},
			},
		},
		{
			name: "multiple status fields",
			instance: newTestResource(
				withObject(map[string]interface{}{
					"status": map[string]interface{}{
						"ready": "${expr1}",
						"count": "${expr2}",
					},
				}),
				withVariables([]*variable.ResourceField{
					{
						FieldDescriptor: variable.FieldDescriptor{
							Path:                 "status.ready",
							Expressions:          []string{"expr1"},
							StandaloneExpression: true,
						},
					},
					{
						FieldDescriptor: variable.FieldDescriptor{
							Path:                 "status.count",
							Expressions:          []string{"expr2"},
							StandaloneExpression: true,
						},
					},
				}),
			),
			expCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:    "expr1",
					Resolved:      true,
					ResolvedValue: true,
				},
				"expr2": {
					Expression:    "expr2",
					Resolved:      true,
					ResolvedValue: 5,
				},
			},
			wantObj: map[string]interface{}{
				"status": map[string]interface{}{
					"ready": true,
					"count": 5,
				},
			},
		},
		{
			name: "blind resolution - partially resolved",
			instance: newTestResource(
				withObject(map[string]interface{}{
					"status": map[string]interface{}{},
				}),
				withVariables([]*variable.ResourceField{
					{
						FieldDescriptor: variable.FieldDescriptor{
							Path:                 "status.ready",
							Expressions:          []string{"expr1"},
							StandaloneExpression: true,
						},
					},
					{
						FieldDescriptor: variable.FieldDescriptor{
							Path:                 "status.count",
							Expressions:          []string{"expr2"},
							StandaloneExpression: true,
						},
					},
					{
						FieldDescriptor: variable.FieldDescriptor{
							Path:                 "status.conditions[0].status",
							Expressions:          []string{"expr3"},
							StandaloneExpression: true,
						},
					},
				}),
			),
			expCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:    "expr1",
					Resolved:      true,
					ResolvedValue: true,
				},
				"expr2": {
					Expression: "expr2",
					Resolved:   false,
				},
				"expr3": {
					Expression: "expr3",
					Resolved:   false,
				},
			},
			wantObj: map[string]interface{}{
				"status": map[string]interface{}{
					"ready": true,
				},
			},
		},
		{
			name: "blind resolution - fully resolved",
			instance: newTestResource(
				withObject(map[string]interface{}{
					"status": map[string]interface{}{},
				}),
				withVariables([]*variable.ResourceField{
					{
						FieldDescriptor: variable.FieldDescriptor{
							Path:                 "status.ready",
							Expressions:          []string{"expr1"},
							StandaloneExpression: true,
						},
					},
					{
						FieldDescriptor: variable.FieldDescriptor{
							Path:                 "status.count",
							Expressions:          []string{"expr2"},
							StandaloneExpression: true,
						},
					},
					{
						FieldDescriptor: variable.FieldDescriptor{
							Path:                 "status.conditions[0].status",
							Expressions:          []string{"expr3"},
							StandaloneExpression: true,
						},
					},
				}),
			),
			expCache: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:    "expr1",
					Resolved:      true,
					ResolvedValue: true,
				},
				"expr2": {
					Expression:    "expr2",
					Resolved:      true,
					ResolvedValue: 5,
				},
				"expr3": {
					Expression:    "expr3",
					Resolved:      true,
					ResolvedValue: "Healthy",
				},
			},
			wantObj: map[string]interface{}{
				"status": map[string]interface{}{
					"ready": true,
					"count": 5,
					"conditions": []interface{}{
						map[string]interface{}{
							"status": "Healthy",
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				instance:         tt.instance,
				expressionsCache: tt.expCache,
			}

			err := rt.evaluateInstanceStatuses()
			if (err != nil) != tt.wantErr {
				t.Errorf("evaluateInstanceStatuses() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				got := tt.instance.Unstructured().Object
				if !reflect.DeepEqual(got, tt.wantObj) {
					t.Errorf("evaluateInstanceStatuses() = %v, want %v", got, tt.wantObj)
				}
			}
		})
	}
}

func Test_evaluateResourceExpressions(t *testing.T) {
	tests := []struct {
		name        string
		resource    Resource
		expressions map[string]*expressionEvaluationState
		wantObj     map[string]interface{}
		wantErr     bool
	}{
		{
			name: "simple replacement",
			resource: newTestResource(
				withObject(map[string]interface{}{
					"spec": map[string]interface{}{
						"replicas": "${expr1}",
					},
				}),
				withVariables([]*variable.ResourceField{
					{
						FieldDescriptor: variable.FieldDescriptor{
							Path:                 "spec.replicas",
							Expressions:          []string{"expr1"},
							StandaloneExpression: true,
						},
					},
				}),
			),
			expressions: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:    "expr1",
					Resolved:      true,
					ResolvedValue: 3,
				},
			},
			wantObj: map[string]interface{}{
				"spec": map[string]interface{}{
					"replicas": 3,
				},
			},
		},
		{
			name: "multiple replacements",
			resource: newTestResource(
				withObject(map[string]interface{}{
					"spec": map[string]interface{}{
						"name": "${expr1}",
						"size": "${expr2}",
					},
				}),
				withVariables([]*variable.ResourceField{
					{
						FieldDescriptor: variable.FieldDescriptor{
							Path:                 "spec.name",
							Expressions:          []string{"expr1"},
							StandaloneExpression: true,
						},
					},
					{
						FieldDescriptor: variable.FieldDescriptor{
							Path:                 "spec.size",
							Expressions:          []string{"expr2"},
							StandaloneExpression: true,
						},
					},
				}),
			),
			expressions: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:    "expr1",
					Resolved:      true,
					ResolvedValue: "test",
				},
				"expr2": {
					Expression:    "expr2",
					Resolved:      true,
					ResolvedValue: 5,
				},
			},
			wantObj: map[string]interface{}{
				"spec": map[string]interface{}{
					"name": "test",
					"size": 5,
				},
			},
		},
		{
			name: "nested replacement",
			resource: newTestResource(
				withObject(map[string]interface{}{
					"spec": map[string]interface{}{
						"config": map[string]interface{}{
							"value": "${expr1}",
						},
					},
				}),
				withVariables([]*variable.ResourceField{
					{
						FieldDescriptor: variable.FieldDescriptor{
							Path:        "spec.config.value",
							Expressions: []string{"expr1"},
						},
					},
				}),
			),
			expressions: map[string]*expressionEvaluationState{
				"expr1": {
					Expression:    "expr1",
					Resolved:      true,
					ResolvedValue: "nested",
				},
			},
			wantObj: map[string]interface{}{
				"spec": map[string]interface{}{
					"config": map[string]interface{}{
						"value": "nested",
					},
				},
			},
		},
		{
			name: "unresolved expression",
			resource: newTestResource(
				withObject(map[string]interface{}{
					"spec": map[string]interface{}{
						"value": "${expr1}",
					},
				}),
				withVariables([]*variable.ResourceField{
					{
						FieldDescriptor: variable.FieldDescriptor{
							Path:        "spec.value",
							Expressions: []string{"expr1"},
						},
					},
				}),
			),
			expressions: map[string]*expressionEvaluationState{
				"expr1": {
					Expression: "expr1",
					Resolved:   false,
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				resources:        map[string]Resource{"test": tt.resource},
				expressionsCache: tt.expressions,
			}

			err := rt.evaluateResourceExpressions("test")
			if (err != nil) != tt.wantErr {
				t.Errorf("evaluateResourceExpressions() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				got := tt.resource.Unstructured().Object
				if !reflect.DeepEqual(got, tt.wantObj) {
					t.Errorf("evaluateResourceExpressions() resource = %v, want %v", got, tt.wantObj)
				}
			}
		})
	}
}

func Test_allExpressionsAreResolved(t *testing.T) {
	tests := []struct {
		name        string
		expressions map[string]*expressionEvaluationState
		want        bool
	}{
		{
			name:        "empty cache",
			expressions: map[string]*expressionEvaluationState{},
			want:        true,
		},
		{
			name: "all resolved",
			expressions: map[string]*expressionEvaluationState{
				"expr1": {
					Expression: "true",
					Resolved:   true,
				},
				"expr2": {
					Expression: "false",
					Resolved:   true,
				},
			},
			want: true,
		},
		{
			name: "one unresolved",
			expressions: map[string]*expressionEvaluationState{
				"expr1": {
					Expression: "true",
					Resolved:   true,
				},
				"expr2": {
					Expression: "false",
					Resolved:   false,
				},
			},
			want: false,
		},
		{
			name: "all unresolved",
			expressions: map[string]*expressionEvaluationState{
				"expr1": {
					Expression: "true",
					Resolved:   false,
				},
				"expr2": {
					Expression: "false",
					Resolved:   false,
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				expressionsCache: tt.expressions,
			}

			got := rt.allExpressionsAreResolved()
			if got != tt.want {
				t.Errorf("allExpressionsAreResolved() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_IsResourceReady(t *testing.T) {
	tests := []struct {
		name           string
		resource       Resource
		resolvedObject map[string]interface{}
		want           bool
		wantReason     string
		wantErr        bool
	}{
		{
			name: "no ready expressions",
			resource: newTestResource(
				withReadyExpressions(nil),
			),
			resolvedObject: map[string]interface{}{},
			want:           true,
		},
		{
			name: "resource not resolved",
			resource: newTestResource(
				withReadyExpressions([]string{"test.status.ready"}),
			),
			want:       false,
			wantReason: "resource test is not resolved",
		},
		{
			name: "ready expression true",
			resource: newTestResource(
				withReadyExpressions([]string{"test.status.ready"}),
			),
			resolvedObject: map[string]interface{}{
				"status": map[string]interface{}{
					"ready": true,
				},
			},
			want: true,
		},
		{
			name: "ready expression false",
			resource: newTestResource(
				withReadyExpressions([]string{"test.status.ready"}),
			),
			resolvedObject: map[string]interface{}{
				"status": map[string]interface{}{
					"ready": false,
				},
			},
			want:       false,
			wantReason: "expression test.status.ready evaluated to false",
		},
		{
			name: "invalid expression",
			resource: newTestResource(
				withReadyExpressions([]string{"invalid )"}),
			),
			resolvedObject: map[string]interface{}{},
			want:           false,
			wantErr:        true,
		},
		{
			name: "multiple expressions all true",
			resource: newTestResource(
				withReadyExpressions([]string{
					"test.status.ready",
					"test.status.healthy && test.status.count > 10",
					"test.status.count > 5",
				}),
			),
			resolvedObject: map[string]interface{}{
				"status": map[string]interface{}{
					"ready":   true,
					"healthy": true,
					"count":   15,
				},
			},
			want: true,
		},
		{
			name: "multiple expressions one false",
			resource: newTestResource(
				withReadyExpressions([]string{"test.status.ready", "test.status.healthy"}),
			),
			resolvedObject: map[string]interface{}{
				"status": map[string]interface{}{
					"ready":   true,
					"healthy": false,
				},
			},
			want:       false,
			wantReason: "expression test.status.healthy evaluated to false",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				resources:         map[string]Resource{"test": tt.resource},
				resolvedResources: map[string]*unstructured.Unstructured{},
			}

			if tt.resolvedObject != nil {
				rt.resolvedResources["test"] = &unstructured.Unstructured{Object: tt.resolvedObject}
			}

			got, reason, err := rt.IsResourceReady("test")
			if (err != nil) != tt.wantErr {
				t.Errorf("IsResourceReady() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("IsResourceReady() = %v, want %v", got, tt.want)
			}
			if reason != tt.wantReason {
				t.Errorf("IsResourceReady() reason = %v, want %v", reason, tt.wantReason)
			}
		})
	}
}
func Test_ReadyToProcessResource(t *testing.T) {
	tests := []struct {
		name         string
		resource     Resource
		instanceSpec map[string]interface{}
		ignoredDeps  map[string]bool
		want         bool
		wantSkip     bool
		wantErr      bool
	}{
		{
			name: "no conditions",
			resource: newTestResource(
				withIncludeWhenExpressions(nil),
			),
			want: true,
		},
		{
			name: "simple true condition",
			resource: newTestResource(
				withIncludeWhenExpressions([]string{"true"}),
			),
			want: true,
		},
		{
			name: "simple false condition",
			resource: newTestResource(
				withIncludeWhenExpressions([]string{"false"}),
			),
			want:     false,
			wantSkip: true,
		},
		{
			name: "spec based condition",
			resource: newTestResource(
				withIncludeWhenExpressions([]string{"schema.spec.enabled == true"}),
			),
			instanceSpec: map[string]interface{}{
				"enabled": true,
			},
			want: true,
		},
		{
			name: "ignored dependency",
			resource: newTestResource(
				withDependencies([]string{"dep1"}),
			),
			ignoredDeps: map[string]bool{"dep1": true},
			want:        false,
		},
		{
			name: "invalid expression",
			resource: newTestResource(
				withIncludeWhenExpressions([]string{"invalid )"}),
			),
			wantErr: true,
		},
		{
			name: "multiple conditions all true",
			resource: newTestResource(
				withIncludeWhenExpressions([]string{"true", "1 == 1"}),
			),
			want: true,
		},
		{
			name: "multiple conditions one false",
			resource: newTestResource(
				withIncludeWhenExpressions([]string{"true", "false"}),
			),
			want:     false,
			wantSkip: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				ignoredByConditionsResources: tt.ignoredDeps,
				instance: newTestResource(
					withObject(map[string]interface{}{
						"spec": tt.instanceSpec,
					}),
				),
				resources: map[string]Resource{
					"test": tt.resource,
				},
			}

			got, err := rt.ReadyToProcessResource("test")
			if tt.wantErr {
				if err == nil {
					t.Error("ReadyToProcessResource() expected error, got none")
				}
				return
			}
			if tt.wantSkip {
				if err == nil || !strings.Contains(err.Error(), "skipping resource creation due to condition") {
					t.Errorf("ReadyToProcessResource() expected skip message, got %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("ReadyToProcessResource() unexpected error = %v", err)
				return
			}
			if got != tt.want {
				t.Errorf("ReadyToProcessResource() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_areDependenciesIgnored(t *testing.T) {
	tests := []struct {
		name        string
		resource    Resource
		ignoredDeps map[string]bool
		want        bool
	}{
		{
			name: "no dependencies",
			resource: newTestResource(
				withDependencies(nil),
			),
			ignoredDeps: map[string]bool{},
			want:        false,
		},
		{
			name: "dependencies not ignored",
			resource: newTestResource(
				withDependencies([]string{"dep1", "dep2"}),
			),
			ignoredDeps: map[string]bool{},
			want:        false,
		},
		{
			name: "one dependency ignored",
			resource: newTestResource(
				withDependencies([]string{"dep1", "dep2"}),
			),
			ignoredDeps: map[string]bool{
				"dep1": true,
			},
			want: true,
		},
		{
			name: "all dependencies ignored",
			resource: newTestResource(
				withDependencies([]string{"dep1", "dep2"}),
			),
			ignoredDeps: map[string]bool{
				"dep1": true,
				"dep2": true,
			},
			want: true,
		},
		{
			name: "more dependencies ignored",
			resource: newTestResource(
				withDependencies([]string{"dep1", "dep2"}),
			),
			ignoredDeps: map[string]bool{
				"dep1": true,
				"dep2": true,
				"dep3": true,
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				resources:                    map[string]Resource{"test": tt.resource},
				ignoredByConditionsResources: tt.ignoredDeps,
			}

			got := rt.areDependenciesIgnored("test")
			if got != tt.want {
				t.Errorf("areDependenciesIgnored() = %v, want %v", got, tt.want)
			}
		})
	}
}

func setupTestEnv(names []string) (*cel.Env, error) {
	return krocel.DefaultEnvironment(krocel.WithResourceIDs(names))
}

func Test_evaluateExpression(t *testing.T) {
	env, err := setupTestEnv([]string{"data"})
	if err != nil {
		t.Fatalf("failed to create environment: %v", err)
	}

	tests := []struct {
		name       string
		context    map[string]interface{}
		expression string
		want       interface{}
		wantErr    bool
	}{
		{
			name:       "simple math",
			context:    map[string]interface{}{},
			expression: "1 + 1",
			want:       int64(2),
		},
		{
			name: "map access",
			context: map[string]interface{}{
				"data": map[string]interface{}{
					"value": "hello",
				},
			},
			expression: "data.value",
			want:       "hello",
		},
		{
			name:       "invalid expression",
			context:    map[string]interface{}{},
			expression: "invalid )",
			wantErr:    true,
		},
		{
			name:       "use of undefined variable",
			context:    map[string]interface{}{},
			expression: "undefined.value",
			wantErr:    true,
		},
		{
			name: "unsupported type in expression",
			context: map[string]interface{}{
				"data": map[string]interface{}{
					"value": map[int]interface{}{
						1: "one",
					},
				},
			},
			expression: "data.value",
			wantErr:    true,
		},
		{
			name: "unsupported type in optional expression",
			context: map[string]interface{}{
				"data": map[string]interface{}{
					"value": map[int]interface{}{
						1: "one",
					},
				},
			},
			expression: "data.?value",
			wantErr:    true,
		},
	}

	// Create a minimal runtime for testing
	rt := &ResourceGraphDefinitionRuntime{
		celProgramCache: make(map[string]cel.Program),
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rt.evaluateExpression(env, tt.context, tt.expression)
			if (err != nil) != tt.wantErr {
				t.Errorf("evaluateExpression() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err == nil && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("evaluateExpression() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Benchmark_evaluateExpression(b *testing.B) {
	env, err := setupTestEnv([]string{"resource"})
	if err != nil {
		b.Fatalf("failed to create environment: %v", err)
	}

	context := map[string]interface{}{
		"resource": map[string]interface{}{
			"status": map[string]interface{}{
				"ready":     true,
				"phase":     "Running",
				"replicas":  int64(3),
				"available": int64(3),
			},
			"metadata": map[string]interface{}{
				"name":      "test-resource",
				"namespace": "default",
			},
		},
	}

	b.Run("cached_simple_expression", func(b *testing.B) {
		rt := &ResourceGraphDefinitionRuntime{
			celProgramCache: make(map[string]cel.Program),
		}
		expression := "resource.status.ready"

		b.ResetTimer()
		for b.Loop() {
			_, err := rt.evaluateExpression(env, context, expression)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("cached_complex_expression", func(b *testing.B) {
		rt := &ResourceGraphDefinitionRuntime{
			celProgramCache: make(map[string]cel.Program),
		}
		expression := "resource.status.ready && resource.status.phase == 'Running' && resource.status.replicas == resource.status.available"

		b.ResetTimer()
		for b.Loop() {
			_, err := rt.evaluateExpression(env, context, expression)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("uncached_expression", func(b *testing.B) {
		rt := &ResourceGraphDefinitionRuntime{
			celProgramCache: nil,
		}
		expression := "resource.status.ready"

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, err := rt.evaluateExpression(env, context, expression)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("multiple_different_expressions", func(b *testing.B) {
		rt := &ResourceGraphDefinitionRuntime{
			celProgramCache: make(map[string]cel.Program),
		}
		expressions := []string{
			"resource.status.ready",
			"resource.status.phase == 'Running'",
			"resource.status.replicas == 3",
			"resource.metadata.name == 'test-resource'",
		}

		b.ResetTimer()
		for b.Loop() {
			expr := expressions[b.N%len(expressions)]
			_, err := rt.evaluateExpression(env, context, expr)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func Test_containsAllElements(t *testing.T) {
	tests := []struct {
		name  string
		outer []string
		inner []string
		want  bool
	}{
		{
			name:  "empty slices",
			outer: []string{},
			inner: []string{},
			want:  true,
		},
		{
			name:  "empty inner slice",
			outer: []string{"a", "b", "c"},
			inner: []string{},
			want:  true,
		},
		{
			name:  "empty outer slice",
			outer: []string{},
			inner: []string{"a"},
			want:  false,
		},
		{
			name:  "exact match",
			outer: []string{"a", "b", "c"},
			inner: []string{"a", "b", "c"},
			want:  true,
		},
		{
			name:  "subset match",
			outer: []string{"a", "b", "c"},
			inner: []string{"a", "b"},
			want:  true,
		},
		{
			name:  "single element match",
			outer: []string{"a", "b", "c"},
			inner: []string{"b"},
			want:  true,
		},
		{
			name:  "no match",
			outer: []string{"a", "b", "c"},
			inner: []string{"d"},
			want:  false,
		},
		{
			name:  "partial match failure",
			outer: []string{"a", "b", "c"},
			inner: []string{"a", "d"},
			want:  false,
		},
		{
			name:  "outer has duplicates",
			outer: []string{"a", "b", "b", "c"},
			inner: []string{"a", "b"},
			want:  true,
		},
		{
			name:  "inner has duplicates",
			outer: []string{"a", "b", "c"},
			inner: []string{"a", "b", "b"},
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containsAllElements(tt.outer, tt.inner)
			if got != tt.want {
				t.Errorf("containsAllElements() = %v, want %v", got, tt.want)
			}
		})
	}
}

type mockResource struct {
	gvr                    schema.GroupVersionResource
	variables              []*variable.ResourceField
	dependencies           []string
	readyExpressions       []string
	includeWhenExpressions []string
	namespaced             bool
	isExternalRef          bool
	isCollection           bool
	forEachDimensions      []ForEachDimensionInfo
	obj                    *unstructured.Unstructured
}

func newMockResource() *mockResource {
	return &mockResource{
		obj: &unstructured.Unstructured{
			Object: make(map[string]interface{}),
		},
	}
}

func (m *mockResource) GetGroupVersionResource() schema.GroupVersionResource {
	return m.gvr
}

func (m *mockResource) GetVariables() []*variable.ResourceField {
	return m.variables
}

func (m *mockResource) GetDependencies() []string {
	return m.dependencies
}

func (m *mockResource) GetReadyWhenExpressions() []string {
	return m.readyExpressions
}

func (m *mockResource) GetIncludeWhenExpressions() []string {
	return m.includeWhenExpressions
}

func (m *mockResource) IsNamespaced() bool {
	return m.namespaced
}

func (m *mockResource) Unstructured() *unstructured.Unstructured {
	return m.obj
}

func (m *mockResource) IsExternalRef() bool {
	return m.isExternalRef
}

func (m *mockResource) IsCollection() bool {
	return m.isCollection
}

func (m *mockResource) GetForEachDimensionInfo() []ForEachDimensionInfo {
	return m.forEachDimensions
}

type mockResourceOption func(*mockResource)

/* func withGVR(group, version, resource string) mockResourceOption {
	return func(m *mockResource) {
		m.gvr = schema.GroupVersionResource{
			Group:    group,
			Version:  version,
			Resource: resource,
		}
	}
} */

func withVariables(vars []*variable.ResourceField) mockResourceOption {
	return func(m *mockResource) {
		m.variables = vars
	}
}

func withDependencies(deps []string) mockResourceOption {
	return func(m *mockResource) {
		m.dependencies = deps
	}
}

func withReadyExpressions(exprs []string) mockResourceOption {
	return func(m *mockResource) {
		m.readyExpressions = exprs
	}
}

func withIncludeWhenExpressions(exprs []string) mockResourceOption {
	return func(m *mockResource) {
		m.includeWhenExpressions = exprs
	}
}

func withIsCollection(isCollection bool) mockResourceOption {
	return func(m *mockResource) {
		m.isCollection = isCollection
	}
}

/* func withNamespaced(namespaced bool) mockResourceOption {
	return func(m *mockResource) {
		m.namespaced = namespaced
	}
} */

func withObject(obj map[string]interface{}) mockResourceOption {
	return func(m *mockResource) {
		m.obj.Object = obj
	}
}

func withUnstructured(obj *unstructured.Unstructured) mockResourceOption {
	return func(m *mockResource) {
		m.obj = obj
	}
}

func withForEachDimensions(dimensions []ForEachDimensionInfo) mockResourceOption {
	return func(m *mockResource) {
		m.forEachDimensions = dimensions
	}
}

func newTestResource(opts ...mockResourceOption) *mockResource {
	r := newMockResource()
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func TestCartesianProduct(t *testing.T) {
	tests := []struct {
		name      string
		iterators []ForEachDimensionInfo
		values    [][]interface{}
		wantLen   int
		wantFirst map[string]interface{}
		wantLast  map[string]interface{}
	}{
		{
			name: "single iterator with 3 values",
			iterators: []ForEachDimensionInfo{
				{Name: "region", Expression: "schema.spec.regions"},
			},
			values:    [][]interface{}{{"us", "eu", "ap"}},
			wantLen:   3,
			wantFirst: map[string]interface{}{"region": "us"},
			wantLast:  map[string]interface{}{"region": "ap"},
		},
		{
			name: "two iterators - cartesian product",
			iterators: []ForEachDimensionInfo{
				{Name: "region", Expression: "schema.spec.regions"},
				{Name: "tier", Expression: "schema.spec.tiers"},
			},
			values:    [][]interface{}{{"us", "eu"}, {"web", "api"}},
			wantLen:   4,
			wantFirst: map[string]interface{}{"region": "us", "tier": "web"},
			wantLast:  map[string]interface{}{"region": "eu", "tier": "api"},
		},
		{
			name: "empty values returns nil",
			iterators: []ForEachDimensionInfo{
				{Name: "region", Expression: "schema.spec.regions"},
			},
			values:  [][]interface{}{{}},
			wantLen: 0,
		},
		{
			name:      "no iterators returns nil",
			iterators: []ForEachDimensionInfo{},
			values:    [][]interface{}{},
			wantLen:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := cartesianProduct(tt.iterators, tt.values)
			if tt.wantLen == 0 {
				if result != nil {
					t.Errorf("expected nil, got %v", result)
				}
				return
			}
			if len(result) != tt.wantLen {
				t.Errorf("expected length %d, got %d", tt.wantLen, len(result))
			}
			if !reflect.DeepEqual(result[0], tt.wantFirst) {
				t.Errorf("first element: expected %v, got %v", tt.wantFirst, result[0])
			}
			if !reflect.DeepEqual(result[len(result)-1], tt.wantLast) {
				t.Errorf("last element: expected %v, got %v", tt.wantLast, result[len(result)-1])
			}
		})
	}
}

// Test_CollectionStorage tests the collection storage methods:
// GetCollectionResources and SetCollectionResources
func Test_CollectionStorage(t *testing.T) {
	tests := []struct {
		name      string
		resources []*unstructured.Unstructured
	}{
		{
			name:      "empty collection",
			resources: []*unstructured.Unstructured{},
		},
		{
			name: "single resource",
			resources: []*unstructured.Unstructured{
				{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "item-0"}}},
			},
		},
		{
			name: "multiple resources",
			resources: []*unstructured.Unstructured{
				{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "item-0"}}},
				{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "item-1"}}},
				{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "item-2"}}},
			},
		},
		{
			name: "resources with nil entries",
			resources: []*unstructured.Unstructured{
				{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "item-0"}}},
				nil,
				{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "item-2"}}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				resolvedCollections: make(map[string][]*unstructured.Unstructured),
			}

			// Test SetCollectionResources
			rt.SetCollectionResources("myCollection", tt.resources)

			// Test GetCollectionResources
			got, state := rt.GetCollectionResources("myCollection")
			if state != ResourceStateResolved {
				t.Errorf("GetCollectionResources() state = %v, want ResourceStateResolved", state)
			}
			if len(got) != len(tt.resources) {
				t.Errorf("GetCollectionResources() length = %d, want %d", len(got), len(tt.resources))
			}
			for i := range tt.resources {
				if got[i] != tt.resources[i] {
					t.Errorf("GetCollectionResources()[%d] = %v, want %v", i, got[i], tt.resources[i])
				}
			}

			// Test GetCollectionResources for non-existent collection
			missing, missingState := rt.GetCollectionResources("nonExistent")
			if missing != nil {
				t.Errorf("GetCollectionResources(nonExistent) = %v, want nil", missing)
			}
			if missingState != ResourceStateWaitingOnDependencies {
				t.Errorf("GetCollectionResources(nonExistent) state = %v, want ResourceStateWaitingOnDependencies", missingState)
			}
		})
	}
}

// Test_isResourceResolved tests the isResourceResolved helper
func Test_isResourceResolved(t *testing.T) {
	tests := []struct {
		name                string
		resourceID          string
		isCollection        bool
		resolvedResources   map[string]*unstructured.Unstructured
		resolvedCollections map[string][]*unstructured.Unstructured
		want                bool
	}{
		{
			name:         "single resource - resolved",
			resourceID:   "configmap",
			isCollection: false,
			resolvedResources: map[string]*unstructured.Unstructured{
				"configmap": {Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "test"}}},
			},
			resolvedCollections: make(map[string][]*unstructured.Unstructured),
			want:                true,
		},
		{
			name:                "single resource - not resolved",
			resourceID:          "configmap",
			isCollection:        false,
			resolvedResources:   make(map[string]*unstructured.Unstructured),
			resolvedCollections: make(map[string][]*unstructured.Unstructured),
			want:                false,
		},
		{
			name:              "collection - resolved",
			resourceID:        "configs",
			isCollection:      true,
			resolvedResources: make(map[string]*unstructured.Unstructured),
			resolvedCollections: map[string][]*unstructured.Unstructured{
				"configs": {{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "item-0"}}}},
			},
			want: true,
		},
		{
			name:                "collection - not resolved",
			resourceID:          "configs",
			isCollection:        true,
			resolvedResources:   make(map[string]*unstructured.Unstructured),
			resolvedCollections: make(map[string][]*unstructured.Unstructured),
			want:                false,
		},
		{
			name:              "collection - empty slice is still resolved",
			resourceID:        "configs",
			isCollection:      true,
			resolvedResources: make(map[string]*unstructured.Unstructured),
			resolvedCollections: map[string][]*unstructured.Unstructured{
				"configs": {}, // empty but exists
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				resources: map[string]Resource{
					tt.resourceID: newTestResource(withIsCollection(tt.isCollection)),
				},
				resolvedResources:   tt.resolvedResources,
				resolvedCollections: tt.resolvedCollections,
			}

			got := rt.isResourceResolved(tt.resourceID)
			if got != tt.want {
				t.Errorf("isResourceResolved() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Test_allResourcesResolved tests the allResourcesResolved helper
func Test_allResourcesResolved(t *testing.T) {
	tests := []struct {
		name                string
		resources           map[string]Resource
		resolvedResources   map[string]*unstructured.Unstructured
		resolvedCollections map[string][]*unstructured.Unstructured
		want                bool
	}{
		{
			name:                "no resources",
			resources:           map[string]Resource{},
			resolvedResources:   make(map[string]*unstructured.Unstructured),
			resolvedCollections: make(map[string][]*unstructured.Unstructured),
			want:                true,
		},
		{
			name: "all single resources resolved",
			resources: map[string]Resource{
				"configmap":  newTestResource(withIsCollection(false)),
				"deployment": newTestResource(withIsCollection(false)),
			},
			resolvedResources: map[string]*unstructured.Unstructured{
				"configmap":  {Object: map[string]interface{}{}},
				"deployment": {Object: map[string]interface{}{}},
			},
			resolvedCollections: make(map[string][]*unstructured.Unstructured),
			want:                true,
		},
		{
			name: "single resource not resolved",
			resources: map[string]Resource{
				"configmap":  newTestResource(withIsCollection(false)),
				"deployment": newTestResource(withIsCollection(false)),
			},
			resolvedResources: map[string]*unstructured.Unstructured{
				"configmap": {Object: map[string]interface{}{}},
				// deployment missing
			},
			resolvedCollections: make(map[string][]*unstructured.Unstructured),
			want:                false,
		},
		{
			name: "all collections resolved",
			resources: map[string]Resource{
				"configs": newTestResource(withIsCollection(true)),
				"pods":    newTestResource(withIsCollection(true)),
			},
			resolvedResources: make(map[string]*unstructured.Unstructured),
			resolvedCollections: map[string][]*unstructured.Unstructured{
				"configs": {{Object: map[string]interface{}{}}},
				"pods":    {{Object: map[string]interface{}{}}},
			},
			want: true,
		},
		{
			name: "collection not resolved",
			resources: map[string]Resource{
				"configs": newTestResource(withIsCollection(true)),
				"pods":    newTestResource(withIsCollection(true)),
			},
			resolvedResources: make(map[string]*unstructured.Unstructured),
			resolvedCollections: map[string][]*unstructured.Unstructured{
				"configs": {{Object: map[string]interface{}{}}},
				// pods missing
			},
			want: false,
		},
		{
			name: "mixed - all resolved",
			resources: map[string]Resource{
				"configmap": newTestResource(withIsCollection(false)),
				"configs":   newTestResource(withIsCollection(true)),
			},
			resolvedResources: map[string]*unstructured.Unstructured{
				"configmap": {Object: map[string]interface{}{}},
			},
			resolvedCollections: map[string][]*unstructured.Unstructured{
				"configs": {{Object: map[string]interface{}{}}},
			},
			want: true,
		},
		{
			name: "mixed - collection not resolved",
			resources: map[string]Resource{
				"configmap": newTestResource(withIsCollection(false)),
				"configs":   newTestResource(withIsCollection(true)),
			},
			resolvedResources: map[string]*unstructured.Unstructured{
				"configmap": {Object: map[string]interface{}{}},
			},
			resolvedCollections: make(map[string][]*unstructured.Unstructured),
			want:                false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				resources:           tt.resources,
				resolvedResources:   tt.resolvedResources,
				resolvedCollections: tt.resolvedCollections,
			}

			got := rt.allResourcesResolved()
			if got != tt.want {
				t.Errorf("allResourcesResolved() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Test_getCollectionResourcesAsSlice tests the getCollectionResourcesAsSlice helper
func Test_getCollectionResourcesAsSlice(t *testing.T) {
	tests := []struct {
		name       string
		resourceID string
		collection []*unstructured.Unstructured
		wantLen    int
	}{
		{
			name:       "nil collection",
			resourceID: "configs",
			collection: nil,
			wantLen:    0,
		},
		{
			name:       "empty collection",
			resourceID: "configs",
			collection: []*unstructured.Unstructured{},
			wantLen:    0,
		},
		{
			name:       "single item",
			resourceID: "configs",
			collection: []*unstructured.Unstructured{
				{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "item-0"}}},
			},
			wantLen: 1,
		},
		{
			name:       "multiple items",
			resourceID: "configs",
			collection: []*unstructured.Unstructured{
				{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "item-0"}}},
				{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "item-1"}}},
				{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "item-2"}}},
			},
			wantLen: 3,
		},
		{
			name:       "items with nil entries are filtered",
			resourceID: "configs",
			collection: []*unstructured.Unstructured{
				{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "item-0"}}},
				nil,
				{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "item-2"}}},
			},
			wantLen: 2, // nil entries are skipped
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				resolvedCollections: map[string][]*unstructured.Unstructured{
					tt.resourceID: tt.collection,
				},
			}

			got := rt.getCollectionResourcesAsSlice(tt.resourceID)
			if len(got) != tt.wantLen {
				t.Errorf("getCollectionResourcesAsSlice() length = %d, want %d", len(got), tt.wantLen)
			}

			// Verify each item is the .Object of the unstructured
			nonNilIdx := 0
			for _, item := range tt.collection {
				if item != nil {
					if !reflect.DeepEqual(got[nonNilIdx], item.Object) {
						t.Errorf("getCollectionResourcesAsSlice()[%d] = %v, want %v", nonNilIdx, got[nonNilIdx], item.Object)
					}
					nonNilIdx++
				}
			}
		})
	}
}

// Test_evaluateDynamicVariablesWithCollections tests that isResourceResolved
// correctly identifies collection dependencies as resolved/unresolved.
// This is a simpler test that doesn't require full CEL evaluation.
func Test_evaluateDynamicVariablesWithCollections(t *testing.T) {
	tests := []struct {
		name                string
		resources           map[string]Resource
		resolvedResources   map[string]*unstructured.Unstructured
		resolvedCollections map[string][]*unstructured.Unstructured
		dependencies        []string
		wantAllResolved     bool
	}{
		{
			name: "collection dependency resolved",
			resources: map[string]Resource{
				"configs": newTestResource(withIsCollection(true)),
			},
			resolvedResources: make(map[string]*unstructured.Unstructured),
			resolvedCollections: map[string][]*unstructured.Unstructured{
				"configs": {
					{Object: map[string]interface{}{"data": map[string]interface{}{"key": "val1"}}},
					{Object: map[string]interface{}{"data": map[string]interface{}{"key": "val2"}}},
				},
			},
			dependencies:    []string{"configs"},
			wantAllResolved: true,
		},
		{
			name: "collection dependency not resolved",
			resources: map[string]Resource{
				"configs": newTestResource(withIsCollection(true)),
			},
			resolvedResources:   make(map[string]*unstructured.Unstructured),
			resolvedCollections: make(map[string][]*unstructured.Unstructured), // not resolved
			dependencies:        []string{"configs"},
			wantAllResolved:     false,
		},
		{
			name: "single resource dependency resolved",
			resources: map[string]Resource{
				"configmap": newTestResource(withIsCollection(false)),
			},
			resolvedResources: map[string]*unstructured.Unstructured{
				"configmap": {Object: map[string]interface{}{"data": map[string]interface{}{"key": "value"}}},
			},
			resolvedCollections: make(map[string][]*unstructured.Unstructured),
			dependencies:        []string{"configmap"},
			wantAllResolved:     true,
		},
		{
			name: "mixed dependencies - all resolved",
			resources: map[string]Resource{
				"configmap": newTestResource(withIsCollection(false)),
				"configs":   newTestResource(withIsCollection(true)),
			},
			resolvedResources: map[string]*unstructured.Unstructured{
				"configmap": {Object: map[string]interface{}{}},
			},
			resolvedCollections: map[string][]*unstructured.Unstructured{
				"configs": {{Object: map[string]interface{}{}}},
			},
			dependencies:    []string{"configmap", "configs"},
			wantAllResolved: true,
		},
		{
			name: "mixed dependencies - collection not resolved",
			resources: map[string]Resource{
				"configmap": newTestResource(withIsCollection(false)),
				"configs":   newTestResource(withIsCollection(true)),
			},
			resolvedResources: map[string]*unstructured.Unstructured{
				"configmap": {Object: map[string]interface{}{}},
			},
			resolvedCollections: make(map[string][]*unstructured.Unstructured),
			dependencies:        []string{"configmap", "configs"},
			wantAllResolved:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				resources:           tt.resources,
				resolvedResources:   tt.resolvedResources,
				resolvedCollections: tt.resolvedCollections,
			}

			// Test that isResourceResolved works correctly for all dependencies
			allResolved := true
			for _, dep := range tt.dependencies {
				if !rt.isResourceResolved(dep) {
					allResolved = false
					break
				}
			}

			if allResolved != tt.wantAllResolved {
				t.Errorf("all dependencies resolved = %v, want %v", allResolved, tt.wantAllResolved)
			}
		})
	}
}

// Test_IsCollectionReady tests the IsCollectionReady method.
// readyWhen for collections uses the `each` keyword for per-item checks (AND semantics).
// Collections cannot reference themselves in readyWhen expressions.
func Test_IsCollectionReady(t *testing.T) {
	tests := []struct {
		name             string
		resourceID       string
		readyExpressions []string
		collection       []*unstructured.Unstructured
		wantReady        bool
		wantReason       string
		wantErr          bool
	}{
		{
			name:             "collection not expanded",
			resourceID:       "configs",
			readyExpressions: []string{},
			collection:       nil,
			wantReady:        false,
			wantReason:       "collection not expanded",
		},
		{
			name:             "no readyWhen expressions - always ready",
			resourceID:       "configs",
			readyExpressions: []string{},
			collection: []*unstructured.Unstructured{
				{Object: map[string]interface{}{"status": map[string]interface{}{"phase": "Pending"}}},
			},
			wantReady: true,
		},
		{
			name:             "each - all items ready",
			resourceID:       "configs",
			readyExpressions: []string{"each.status.phase == 'Running'"},
			collection: []*unstructured.Unstructured{
				{Object: map[string]interface{}{"status": map[string]interface{}{"phase": "Running"}}},
				{Object: map[string]interface{}{"status": map[string]interface{}{"phase": "Running"}}},
			},
			wantReady: true,
		},
		{
			name:             "each - one item not ready",
			resourceID:       "configs",
			readyExpressions: []string{"each.status.phase == 'Running'"},
			collection: []*unstructured.Unstructured{
				{Object: map[string]interface{}{"status": map[string]interface{}{"phase": "Running"}}},
				{Object: map[string]interface{}{"status": map[string]interface{}{"phase": "Pending"}}},
			},
			wantReady:  false,
			wantReason: "item 1: expression each.status.phase == 'Running' evaluated to false",
		},
		{
			name:             "each - first item not ready",
			resourceID:       "configs",
			readyExpressions: []string{"each.status.phase == 'Running'"},
			collection: []*unstructured.Unstructured{
				{Object: map[string]interface{}{"status": map[string]interface{}{"phase": "Pending"}}},
				{Object: map[string]interface{}{"status": map[string]interface{}{"phase": "Running"}}},
			},
			wantReady:  false,
			wantReason: "item 0: expression each.status.phase == 'Running' evaluated to false",
		},
		{
			name:             "each - all items pending",
			resourceID:       "configs",
			readyExpressions: []string{"each.status.phase == 'Running'"},
			collection: []*unstructured.Unstructured{
				{Object: map[string]interface{}{"status": map[string]interface{}{"phase": "Pending"}}},
				{Object: map[string]interface{}{"status": map[string]interface{}{"phase": "Pending"}}},
			},
			wantReady:  false,
			wantReason: "item 0: expression each.status.phase == 'Running' evaluated to false",
		},
		{
			name:             "each - multiple expressions all pass",
			resourceID:       "configs",
			readyExpressions: []string{"each.status.phase == 'Running'", "each.status.ready == true"},
			collection: []*unstructured.Unstructured{
				{Object: map[string]interface{}{"status": map[string]interface{}{"phase": "Running", "ready": true}}},
				{Object: map[string]interface{}{"status": map[string]interface{}{"phase": "Running", "ready": true}}},
			},
			wantReady: true,
		},
		{
			name:             "each - multiple expressions one fails",
			resourceID:       "configs",
			readyExpressions: []string{"each.status.phase == 'Running'", "each.status.ready == true"},
			collection: []*unstructured.Unstructured{
				{Object: map[string]interface{}{"status": map[string]interface{}{"phase": "Running", "ready": true}}},
				{Object: map[string]interface{}{"status": map[string]interface{}{"phase": "Running", "ready": false}}},
			},
			wantReady:  false,
			wantReason: "item 1: expression each.status.ready == true evaluated to false",
		},
		{
			name:             "nil item in collection",
			resourceID:       "configs",
			readyExpressions: []string{"each.status.phase == 'Running'"},
			collection: []*unstructured.Unstructured{
				{Object: map[string]interface{}{"status": map[string]interface{}{"phase": "Running"}}},
				nil,
			},
			wantReady:  false,
			wantReason: "expanded resource 1 not resolved",
		},
		{
			name:             "empty collection is ready (vacuous truth)",
			resourceID:       "configs",
			readyExpressions: []string{"each.status.phase == 'Running'"},
			collection:       []*unstructured.Unstructured{},
			wantReady:        true,
		},
		{
			name:             "each with has() for optional field - field exists",
			resourceID:       "configs",
			readyExpressions: []string{"has(each.status.phase) && each.status.phase == 'Running'"},
			collection: []*unstructured.Unstructured{
				{Object: map[string]interface{}{"status": map[string]interface{}{"phase": "Running"}}},
			},
			wantReady: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &ResourceGraphDefinitionRuntime{
				resources: map[string]Resource{
					tt.resourceID: newTestResource(
						withIsCollection(true),
						withReadyExpressions(tt.readyExpressions),
					),
				},
				resolvedCollections: map[string][]*unstructured.Unstructured{
					tt.resourceID: tt.collection,
				},
			}

			// Remove the collection from resolved if it's nil (simulating "not expanded")
			if tt.collection == nil {
				delete(rt.resolvedCollections, tt.resourceID)
			}

			ready, reason, err := rt.IsCollectionReady(tt.resourceID)
			if (err != nil) != tt.wantErr {
				t.Errorf("IsCollectionReady() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if ready != tt.wantReady {
				t.Errorf("IsCollectionReady() ready = %v, want %v", ready, tt.wantReady)
			}
			if reason != tt.wantReason {
				t.Errorf("IsCollectionReady() reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

// Test_ExpandCollection_SizeLimit tests that ExpandCollection enforces the
// DefaultMaxCollectionSize limit to prevent resource exhaustion.
func Test_ExpandCollection_SizeLimit(t *testing.T) {
	tests := []struct {
		name        string
		itemCount   int
		wantErr     bool
		errContains string
	}{
		{
			name:      "within limit",
			itemCount: 10,
			wantErr:   false,
		},
		{
			name:      "at limit",
			itemCount: DefaultMaxCollectionSize,
			wantErr:   false,
		},
		{
			name:        "exceeds limit",
			itemCount:   DefaultMaxCollectionSize + 1,
			wantErr:     true,
			errContains: "exceeding the maximum limit",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a list of items for forEach
			items := make([]interface{}, tt.itemCount)
			for i := 0; i < tt.itemCount; i++ {
				items[i] = map[string]interface{}{"name": fmt.Sprintf("item-%d", i)}
			}

			// Create mock collection resource
			collectionResource := newTestResource(
				withIsCollection(true),
				withForEachDimensions([]ForEachDimensionInfo{
					{Name: "item", Expression: "schema.spec.items"},
				}),
				withUnstructured(&unstructured.Unstructured{
					Object: map[string]interface{}{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]interface{}{
							"name": "${item.name}",
						},
					},
				}),
			)

			// Create instance with the items
			instance := newTestResource(withUnstructured(&unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "example.com/v1",
					"kind":       "TestInstance",
					"spec": map[string]interface{}{
						"items": items,
					},
				},
			}))

			rt := &ResourceGraphDefinitionRuntime{
				instance:            instance,
				resources:           map[string]Resource{"collection": collectionResource},
				topologicalOrder:    []string{"collection"},
				resolvedResources:   make(map[string]*unstructured.Unstructured),
				resolvedCollections: make(map[string][]*unstructured.Unstructured),
			}

			_, err := rt.ExpandCollection("collection")

			if tt.wantErr {
				if err == nil {
					t.Errorf("ExpandCollection() expected error, got nil")
					return
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("ExpandCollection() error = %v, want error containing %q", err, tt.errContains)
				}
			} else {
				if err != nil {
					t.Errorf("ExpandCollection() unexpected error: %v", err)
				}
			}
		})
	}
}
