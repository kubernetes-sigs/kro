// Copyright 2025 The Kube Resource Orchestrator Authors
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
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/kro-run/kro/api/v1alpha1"
	krofake "github.com/kro-run/kro/pkg/client/fake"
)

func TestConditionsMarker(t *testing.T) {
	tests := []struct {
		name           string
		instanceYAML   string
		operation      func(*ConditionsMarker)
		expectedState  string
		expectedType   string
		expectedReason string
		expectedMsg    string
	}{
		{
			name: "InstanceManaged - success on empty instance",
			instanceYAML: `
apiVersion: example.com/v1
kind: MyInstance
metadata:
  name: test-instance
  generation: 1
spec:
  replicas: 1
`,
			operation: func(mark *ConditionsMarker) {
				mark.InstanceManaged()
			},
			expectedState:  "True",
			expectedType:   InstanceManaged,
			expectedReason: "Managed",
			expectedMsg:    "instance is properly managed with finalizers and labels",
		},
		{
			name: "InstanceNotManaged - failure",
			instanceYAML: `
apiVersion: example.com/v1
kind: MyInstance
metadata:
  name: test-instance
  generation: 1
spec:
  replicas: 1
`,
			operation: func(mark *ConditionsMarker) {
				mark.InstanceNotManaged("failed to set finalizer: %s", "permission denied")
			},
			expectedState:  "False",
			expectedType:   InstanceManaged,
			expectedReason: "ManagementFailed",
			expectedMsg:    "failed to set finalizer: permission denied",
		},
		{
			name: "GraphResolved - success",
			instanceYAML: `
apiVersion: example.com/v1
kind: MyInstance
metadata:
  name: test-instance
  generation: 1
spec:
  replicas: 1
status:
  conditions:
  - type: InstanceManaged
    status: "True"
    reason: Managed
    message: instance is properly managed
    observedGeneration: 1
`,
			operation: func(mark *ConditionsMarker) {
				mark.GraphResolved()
			},
			expectedState:  "True",
			expectedType:   GraphResolved,
			expectedReason: "Resolved",
			expectedMsg:    "runtime graph created and all resources resolved",
		},
		{
			name: "GraphNotResolved - failure with context",
			instanceYAML: `
apiVersion: example.com/v1
kind: MyInstance
metadata:
  name: test-instance
  generation: 1
spec:
  replicas: 1
`,
			operation: func(mark *ConditionsMarker) {
				mark.GraphNotResolved("failed to validate resource %s: %s", "deployment", "invalid field")
			},
			expectedState:  "False",
			expectedType:   GraphResolved,
			expectedReason: "ResolutionFailed",
			expectedMsg:    "failed to validate resource deployment: invalid field",
		},
		{
			name: "ResourcesInProgress - unknown state",
			instanceYAML: `
apiVersion: example.com/v1
kind: MyInstance
metadata:
  name: test-instance
  generation: 1
  creationTimestamp: "2025-08-08T15:30:00Z"
spec:
  replicas: 1
status:
  conditions:
  - type: InstanceManaged
    status: "True"
    reason: Managed
    message: instance is properly managed
    observedGeneration: 1
  - type: GraphResolved
    status: "True"
    reason: Resolved
    message: runtime graph created
    observedGeneration: 1
`,
			operation: func(mark *ConditionsMarker) {
				mark.ResourcesInProgress("processing %d resources", 5)
			},
			expectedState:  "Unknown",
			expectedType:   ResourcesReady,
			expectedReason: "ResourcesInProgress",
			expectedMsg:    "processing 5 resources",
		},
		{
			name: "ResourcesReady - success",
			instanceYAML: `
apiVersion: example.com/v1
kind: MyInstance
metadata:
  name: test-instance
  generation: 1
spec:
  replicas: 1
status:
  conditions:
  - type: InstanceManaged
    status: "True"
    reason: Managed
    message: instance is properly managed
    observedGeneration: 1
  - type: GraphResolved
    status: "True"
    reason: Resolved
    message: runtime graph created
    observedGeneration: 1
  - type: ResourcesReady
    status: "Unknown"
    reason: ResourcesInProgress
    message: processing resources
    observedGeneration: 1
`,
			operation: func(mark *ConditionsMarker) {
				mark.ResourcesReady()
			},
			expectedState:  "True",
			expectedType:   ResourcesReady,
			expectedReason: "AllResourcesReady",
			expectedMsg:    "all resources are created and ready",
		},
		{
			name: "ResourcesNotReady - failure with resource info",
			instanceYAML: `
apiVersion: example.com/v1
kind: MyInstance
metadata:
  name: test-instance
  generation: 1
spec:
  replicas: 1
`,
			operation: func(mark *ConditionsMarker) {
				mark.ResourcesNotReady("resource %s failed: %s", "pod-123", "ImagePullBackOff")
			},
			expectedState:  "False",
			expectedType:   ResourcesReady,
			expectedReason: "ResourcesNotReady",
			expectedMsg:    "resource pod-123 failed: ImagePullBackOff",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse YAML to unstructured
			var instance unstructured.Unstructured
			if err := yaml.Unmarshal([]byte(tt.instanceYAML), &instance.Object); err != nil {
				t.Fatalf("Failed to parse YAML: %v", err)
			}
			// Ensure generation is set if not present in YAML
			if instance.GetGeneration() == 0 {
				instance.SetGeneration(1)
			}

			// Create conditions marker and apply operation
			mark := NewConditionsMarkerFor(&instance)
			tt.operation(mark)

			// Get the updated conditions
			wrapped := wrapInstance(&instance)
			conditions := wrapped.GetConditions()

			// Find the condition we're testing
			var foundCondition *v1alpha1.Condition
			for i, cond := range conditions {
				if string(cond.Type) == tt.expectedType {
					foundCondition = &conditions[i]
					break
				}
			}

			if foundCondition == nil {
				t.Fatalf("Expected condition type %s not found in conditions: %+v", tt.expectedType, conditions)
			}

			// Debug output to see what we actually got
			t.Logf("Found condition: Type=%s, Status=%s, Reason=%v, Message=%v, LastTransitionTime=%v, ObservedGeneration=%d",
				foundCondition.Type, foundCondition.Status, foundCondition.Reason, foundCondition.Message, foundCondition.LastTransitionTime, foundCondition.ObservedGeneration)

			// Validate condition fields
			if string(foundCondition.Status) != tt.expectedState {
				t.Errorf("Expected status %s, got %s", tt.expectedState, foundCondition.Status)
			}

			if foundCondition.Reason == nil || *foundCondition.Reason != tt.expectedReason {
				t.Errorf("Expected reason %s, got %v", tt.expectedReason, foundCondition.Reason)
			}

			if foundCondition.Message == nil || *foundCondition.Message != tt.expectedMsg {
				t.Errorf("Expected message %s, got %v", tt.expectedMsg, foundCondition.Message)
			}

			// Validate observedGeneration is set to the instance generation
			expectedGeneration := instance.GetGeneration()
			if foundCondition.ObservedGeneration != expectedGeneration {
				t.Errorf("Expected observedGeneration %d, got %d", expectedGeneration, foundCondition.ObservedGeneration)
			}

			// Validate lastTransitionTime is set
			if foundCondition.LastTransitionTime == nil {
				t.Error("Expected lastTransitionTime to be set")
			} else {
				// Just check that it's a valid time (not zero)
				if foundCondition.LastTransitionTime.Time.IsZero() {
					t.Error("Expected lastTransitionTime to be non-zero")
				}
			}
		})
	}
}

func TestConditionsMarkerReadyCondition(t *testing.T) {
	tests := []struct {
		name            string
		instanceYAML    string
		operations      []func(*ConditionsMarker)
		expectedReady   metav1.ConditionStatus
		expectedReason  string
		expectedMessage string
	}{
		{
			name: "Ready when all sub-conditions are true",
			instanceYAML: `
apiVersion: example.com/v1
kind: MyInstance
metadata:
  name: test-instance
  generation: 1
spec:
  replicas: 1
`,
			operations: []func(*ConditionsMarker){
				func(mark *ConditionsMarker) { mark.InstanceManaged() },
				func(mark *ConditionsMarker) { mark.GraphResolved() },
				func(mark *ConditionsMarker) { mark.ResourcesReady() },
			},
			expectedReady:   metav1.ConditionTrue,
			expectedReason:  "Ready",
			expectedMessage: "",
		},
		{
			name: "Not ready when InstanceManaged is false",
			instanceYAML: `
apiVersion: example.com/v1
kind: MyInstance
metadata:
  name: test-instance
  generation: 1
spec:
  replicas: 1
`,
			operations: []func(*ConditionsMarker){
				func(mark *ConditionsMarker) { mark.InstanceNotManaged("setup failed") },
				func(mark *ConditionsMarker) { mark.GraphResolved() },
				func(mark *ConditionsMarker) { mark.ResourcesReady() },
			},
			expectedReady:   metav1.ConditionFalse,
			expectedReason:  "ManagementFailed",
			expectedMessage: "setup failed",
		},
		{
			name: "Unknown when resources are in progress",
			instanceYAML: `
apiVersion: example.com/v1
kind: MyInstance
metadata:
  name: test-instance
  generation: 1
spec:
  replicas: 1
`,
			operations: []func(*ConditionsMarker){
				func(mark *ConditionsMarker) { mark.InstanceManaged() },
				func(mark *ConditionsMarker) { mark.GraphResolved() },
				func(mark *ConditionsMarker) { mark.ResourcesInProgress("still processing") },
			},
			expectedReady:   metav1.ConditionUnknown,
			expectedReason:  "ResourcesInProgress",
			expectedMessage: "still processing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse YAML to unstructured
			var instance unstructured.Unstructured
			if err := yaml.Unmarshal([]byte(tt.instanceYAML), &instance.Object); err != nil {
				t.Fatalf("Failed to parse YAML: %v", err)
			}
			// Ensure generation is set if not present in YAML
			if instance.GetGeneration() == 0 {
				instance.SetGeneration(1)
			}

			// Apply all operations
			mark := NewConditionsMarkerFor(&instance)
			for _, op := range tt.operations {
				op(mark)
			}

			// Get the Ready condition
			wrapped := wrapInstance(&instance)
			conditions := wrapped.GetConditions()

			var readyCondition *v1alpha1.Condition
			for i, cond := range conditions {
				if string(cond.Type) == Ready {
					readyCondition = &conditions[i]
					break
				}
			}

			if readyCondition == nil {
				t.Fatal("Ready condition not found")
			}

			// Validate Ready condition
			if readyCondition.Status != tt.expectedReady {
				t.Errorf("Expected Ready status %s, got %s", tt.expectedReady, readyCondition.Status)
			}

			if readyCondition.Reason == nil || *readyCondition.Reason != tt.expectedReason {
				t.Errorf("Expected Ready reason %s, got %v", tt.expectedReason, readyCondition.Reason)
			}

			if tt.expectedMessage != "" {
				if readyCondition.Message == nil || *readyCondition.Message != tt.expectedMessage {
					t.Errorf("Expected Ready message %s, got %v", tt.expectedMessage, readyCondition.Message)
				}
			}
		})
	}
}

func TestUnstructuredConditionAdapter(t *testing.T) {
	instanceYAML := `
apiVersion: example.com/v1
kind: MyInstance
metadata:
  name: test-instance
  generation: 1
spec:
  replicas: 1
status:
  conditions:
  - type: InstanceManaged
    status: "True"
    reason: Managed
    message: instance is properly managed
    lastTransitionTime: "2025-08-07T21:31:02Z"
    observedGeneration: 1
  - type: GraphResolved
    status: "False"
    reason: ValidationError
    message: invalid resource schema
    lastTransitionTime: "2025-08-07T21:31:02Z"
    observedGeneration: 1
`

	var instance unstructured.Unstructured
	if err := yaml.Unmarshal([]byte(instanceYAML), &instance.Object); err != nil {
		t.Fatalf("Failed to parse YAML: %v", err)
	}
	// Ensure generation is set if not present in YAML
	if instance.GetGeneration() == 0 {
		instance.SetGeneration(1)
	}

	// Test GetConditions
	wrapped := wrapInstance(&instance)
	conditions := wrapped.GetConditions()

	if len(conditions) != 2 {
		t.Fatalf("Expected 2 conditions, got %d", len(conditions))
	}

	// Validate first condition
	cond1 := conditions[0]
	if string(cond1.Type) != InstanceManaged {
		t.Errorf("Expected type %s, got %s", InstanceManaged, cond1.Type)
	}
	if cond1.Status != metav1.ConditionTrue {
		t.Errorf("Expected status True, got %s", cond1.Status)
	}
	if cond1.Reason == nil || *cond1.Reason != "Managed" {
		t.Errorf("Expected reason Managed, got %v", cond1.Reason)
	}

	// Test SetConditions
	newConditions := []v1alpha1.Condition{
		{
			Type:               v1alpha1.ConditionType(ResourcesReady),
			Status:             metav1.ConditionTrue,
			Reason:             ptr.To("AllReady"),
			Message:            ptr.To("all resources ready"),
			ObservedGeneration: 1,
		},
	}

	wrapped.SetConditions(newConditions)

	// Verify the conditions were set
	updatedConditions := wrapped.GetConditions()
	if len(updatedConditions) != 1 {
		t.Fatalf("Expected 1 condition after SetConditions, got %d", len(updatedConditions))
	}

	cond := updatedConditions[0]
	if string(cond.Type) != ResourcesReady {
		t.Errorf("Expected type %s, got %s", ResourcesReady, cond.Type)
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("Expected status True, got %s", cond.Status)
	}
}

func TestUpdateInstanceStatusOnErrorPreservesConditions(t *testing.T) {
	// Create an instance with existing conditions including unknown ones
	instanceYAML := `
apiVersion: example.com/v1
kind: MyInstance
metadata:
  name: test-instance
  generation: 1
  creationTimestamp: "2025-08-08T15:30:00Z"
  namespace: test-namespace
spec:
  replicas: 1
status:
  conditions:
  - type: InstanceManaged
    status: "True"
    reason: "Managed"
    message: "instance is properly managed"
    lastTransitionTime: "2025-08-08T15:30:00Z"
    observedGeneration: 1
  - type: GraphResolved
    status: "False"  
    reason: "ValidationError"
    message: "graph validation failed"
    lastTransitionTime: "2025-08-08T15:30:01Z"
    observedGeneration: 1
  - type: ResourcesReady
    status: "Unknown"
    reason: "ResourcesInProgress"
    message: "processing 3 resources"
    lastTransitionTime: "2025-08-08T15:30:02Z"
    observedGeneration: 1
  - type: CustomCondition
    status: "Unknown"
    reason: "CustomReason"
    message: "custom unknown condition"
    lastTransitionTime: "2025-08-08T15:30:03Z"
    observedGeneration: 1
  state: Active
`

	var instance unstructured.Unstructured
	if err := yaml.Unmarshal([]byte(instanceYAML), &instance.Object); err != nil {
		t.Fatalf("Failed to parse YAML: %v", err)
	}

	// Set up fake dynamic client and fake client set
	scheme := runtime.NewScheme()
	fakeDynamicClient := fake.NewSimpleDynamicClient(scheme, &instance)
	fakeClientSet := krofake.NewFakeSet(fakeDynamicClient)

	// Mock controller with fake client setup
	testGVR := schema.GroupVersionResource{
		Group:    "example.com",
		Version:  "v1",
		Resource: "myinstances",
	}

	controller := &Controller{
		clientSet: fakeClientSet,
		gvr:       testGVR,
	}

	// Call updateInstanceStatusOnError
	controller.updateInstanceStatusOnError(context.Background(), &instance)

	// Verify the status was updated to error state
	status, ok := instance.Object["status"].(map[string]interface{})
	if !ok {
		t.Fatal("Status should be a map")
	}

	state, ok := status["state"].(string)
	if !ok || state != "ERROR" {
		t.Errorf("Expected state to be 'ERROR', got %v", state)
	}

	// Verify conditions are preserved
	conditionsInterface, ok := status["conditions"].([]interface{})
	if !ok {
		t.Fatal("Conditions should be present as a slice")
	}

	// Convert back to structured conditions for easier testing
	conditionsJSON, err := json.Marshal(conditionsInterface)
	if err != nil {
		t.Fatalf("Failed to marshal conditions: %v", err)
	}

	var conditions []v1alpha1.Condition
	if err := json.Unmarshal(conditionsJSON, &conditions); err != nil {
		t.Fatalf("Failed to unmarshal conditions: %v", err)
	}

	// Convert to map for easier access
	foundConditions := make(map[string]v1alpha1.Condition)
	for _, cond := range conditions {
		foundConditions[string(cond.Type)] = cond
	}

	// Check that known conditions are preserved
	if instanceManaged, exists := foundConditions["InstanceManaged"]; exists {
		if instanceManaged.Status != metav1.ConditionTrue {
			t.Errorf("Expected InstanceManaged to remain True, got %s", instanceManaged.Status)
		}
		if instanceManaged.Reason == nil || *instanceManaged.Reason != "Managed" {
			t.Errorf("Expected InstanceManaged reason to be preserved")
		}
	} else {
		t.Error("InstanceManaged condition should be preserved")
	}

	if graphResolved, exists := foundConditions["GraphResolved"]; exists {
		if graphResolved.Status != metav1.ConditionFalse {
			t.Errorf("Expected GraphResolved to remain False, got %s", graphResolved.Status)
		}
		if graphResolved.Reason == nil || *graphResolved.Reason != "ValidationError" {
			t.Errorf("Expected GraphResolved reason to be preserved")
		}
	} else {
		t.Error("GraphResolved condition should be preserved")
	}

	if resourcesReady, exists := foundConditions["ResourcesReady"]; exists {
		if resourcesReady.Status != metav1.ConditionUnknown {
			t.Errorf("Expected ResourcesReady to remain Unknown, got %s", resourcesReady.Status)
		}
		if resourcesReady.Reason == nil || *resourcesReady.Reason != "ResourcesInProgress" {
			t.Errorf("Expected ResourcesReady reason to be preserved")
		}
	} else {
		t.Error("ResourcesReady condition should be preserved")
	}

	// The Ready condition should be computed based on the dependents
	if ready, exists := foundConditions["Ready"]; exists {
		// Since GraphResolved is False, Ready should be False (but it might be Unknown during initialization)
		if ready.Status != metav1.ConditionFalse && ready.Status != metav1.ConditionUnknown {
			t.Errorf("Expected Ready to be False or Unknown due to GraphResolved being False, got %s", ready.Status)
		}
	}

	// CustomCondition WILL be preserved since condition.For() preserves existing conditions
	// and adds our managed conditions. This is actually correct behavior - we preserve existing state
	// The key test is that our known conditions are preserved with their correct values

	t.Logf("Found %d conditions after updateInstanceStatusOnError", len(conditions))
	for _, cond := range conditions {
		t.Logf("Condition: %s = %s (reason: %v)", cond.Type, cond.Status, cond.Reason)
	}
}
