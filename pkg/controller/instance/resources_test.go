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

package instance

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

func TestPrepareRegularResource_OwnershipConflicts(t *testing.T) {
	// 1. Setup global variables for the test
	// FIX: Group must be empty "" for core resources like ConfigMap
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
	instanceID := types.UID("instance-uid-123")
	otherInstanceID := "instance-uid-999"

	tests := []struct {
		name          string
		existingObj   *unstructured.Unstructured
		shouldError   bool
		errorContains string
	}{
		{
			name: "Conflict: Resource exists but is NOT managed by kro",
			existingObj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name":      "my-config",
						"namespace": "default",
						// No labels -> unmanaged
					},
				},
			},
			shouldError:   true,
			errorContains: "exists but is not managed by kro",
		},
		{
			name: "Conflict: Resource managed by DIFFERENT instance",
			existingObj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name":      "my-config",
						"namespace": "default",
						"labels": map[string]interface{}{
							metadata.OwnedLabel:      "true",
							metadata.InstanceIDLabel: otherInstanceID, // ID mismatch
						},
					},
				},
			},
			shouldError:   true,
			errorContains: "already owned by instance",
		},
		{
			name: "Success: Resource managed by THIS instance",
			existingObj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name":      "my-config",
						"namespace": "default",
						"labels": map[string]interface{}{
							metadata.OwnedLabel:      "true",
							metadata.InstanceIDLabel: string(instanceID), // Matches!
						},
					},
				},
			},
			shouldError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 2. Setup Fake Client
			fakeClient := dynamicfake.NewSimpleDynamicClient(scheme.Scheme, tt.existingObj)

			// 3. Setup Controller and Context
			controller := &Controller{
				client: nil,
			}

			// Initialize StateManager
			stateMgr := &StateManager{
				ResourceStates: make(map[string]*ResourceState),
			}

			// Define the instance object
			instanceObj := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"metadata": map[string]interface{}{
						"uid":       string(instanceID),
						"name":      "test-instance",
						"namespace": "default",
					},
				},
			}

			// Initialize a Labeler
			labeler := metadata.NewInstanceLabeler(instanceObj)

			// Construct mock ReconcileContext
			rcx := &ReconcileContext{
				Ctx:          context.Background(),
				Client:       fakeClient,
				Log:          logr.Discard(),
				Instance:     instanceObj,
				StateManager: stateMgr,
				Labeler:      labeler,
			}

			// 4. Create the Node (the desired resource definition)
			node := &runtime.Node{
				Spec: &graph.Node{
					Meta: graph.NodeMeta{
						ID:   "test-node",
						GVR:  gvr,
						Type: graph.NodeTypeResource,
					},
				},
			}

			// Mock the desired state
			desired := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name":      "my-config",
						"namespace": "default",
					},
				},
			}

			// 5. Run the function under test
			_, err := controller.prepareRegularResource(rcx, node, &ResourceState{}, []*unstructured.Unstructured{desired})

			// 6. Assertions
			if tt.shouldError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorContains)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
