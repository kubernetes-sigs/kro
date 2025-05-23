// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package framework

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	yamlutil "sigs.k8s.io/yaml"
)

// VerifyOutputsWithTimeout verifies all resources match the expected outputs with timeout
func (tc *TestCase) VerifyOutputsWithTimeout(ctx context.Context, timeout time.Duration,
	outputsDir string) error {
	// Load and sort expected resources
	expectedResources, err := tc.SortedLoadObjectsFromDirectory(ctx, outputsDir)
	if err != nil {
		return fmt.Errorf("failed to load expected resources: %w", err)
	}

	// Create a deadline for total verification time
	deadline := time.Now().Add(timeout)

	// Verify each resource
	for _, res := range expectedResources {
		expected := res.obj

		// Calculate remaining time for this resource
		remainingTime := time.Until(deadline)
		if remainingTime <= 0 {
			return fmt.Errorf("timeout waiting for resources to be ready")
		}

		// Wait for resource to exist and match expected fields
		actual := &unstructured.Unstructured{}
		actual.SetGroupVersionKind(expected.GroupVersionKind())

		tc.t.Logf("      Verifying %s", res.String())
		found := false
		var compareError error
		err := wait.PollUntilContextTimeout(ctx, DefaultWaitInterval, remainingTime, true,
			func(ctx context.Context) (bool, error) {
				// Get the resource
				err := tc.Framework.Client.Get(ctx, types.NamespacedName{
					Name:      expected.GetName(),
					Namespace: expected.GetNamespace(),
				}, actual)
				if err != nil {
					if errors.IsNotFound(err) {
						return false, nil
					}
					return false, err
				}
				found = true

				// Compare metadata if set in expected
				if _, hasMetadata, _ := unstructured.NestedMap(expected.Object, "metadata"); hasMetadata {
					if err := CompareMetadata(expected, actual); err != nil {
						compareError = fmt.Errorf("metadata mismatch: %w", err)
						return false, nil
					}
				}

				// Compare spec if set in expected
				if _, hasSpec, _ := unstructured.NestedMap(expected.Object, "spec"); hasSpec {
					if err := CompareSpecs(expected, actual); err != nil {
						compareError = fmt.Errorf("spec mismatch: %w", err)
						return false, nil
					}
				}

				// Compare status if set in expected
				if _, hasStatus, _ := unstructured.NestedMap(expected.Object, "status"); hasStatus {
					if err := CompareStatus(expected, actual); err != nil {
						compareError = fmt.Errorf("status mismatch: %w", err)
						return false, nil
					}
				}

				return true, nil
			})
		if err != nil {
			if !found {
				return fmt.Errorf("resource %s/%s not found",
					expected.GetNamespace(), expected.GetName())
			}
			return fmt.Errorf("resource %s/%s failed to match expected state within timeout: %w",
				expected.GetNamespace(), expected.GetName(), compareError)
		}
	}

	return nil
}

// VerifyOutputs verifies all resources match the expected outputs with default timeout
func (tc *TestCase) VerifyOutputs(ctx context.Context) error {
	outputsDir := filepath.Join(tc.TestDataPath, OutputsDir)
	return tc.VerifyOutputsWithTimeout(ctx, DefaultWaitTimeout, outputsDir)
}

// WaitForResource waits for a resource to exist and match the given condition
func (tc *TestCase) WaitForResource(ctx context.Context, gvk schema.GroupVersionKind,
	name, namespace string, condition func(*unstructured.Unstructured) bool) error {
	return wait.PollUntilContextTimeout(ctx, DefaultWaitInterval, DefaultWaitTimeout, true,
		func(ctx context.Context) (bool, error) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(gvk)
			err := tc.Framework.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, obj)
			if err != nil {
				if errors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}

			return condition(obj), nil
		})
}

// WaitForResourceGraphDefinitionActive waits for a ResourceGraphDefinition to be present and in Active state
func (tc *TestCase) WaitForResourceGraphDefinitionActive(ctx context.Context, name string) error {
	return tc.WaitForResource(ctx,
		schema.GroupVersionKind{
			Group:   "kro.run",
			Version: "v1alpha1",
			Kind:    "ResourceGraphDefinition",
		},
		name,
		tc.Namespace,
		func(obj *unstructured.Unstructured) bool {
			state, found, _ := unstructured.NestedString(obj.Object, "status", "state")
			return found && state == "Active"
		},
	)
}

func compareObjects(obj1, obj2 interface{}) bool {
	val1 := reflect.ValueOf(obj1)
	val2 := reflect.ValueOf(obj2)

	if val1.Kind() == reflect.Slice || val1.Kind() == reflect.Array {
		if val2.Kind() != reflect.Slice && val2.Kind() != reflect.Array {
			return false
		}
		// TODO: Check if we need this
		if val1.Len() != val2.Len() {
			return false
		}
		for i := 0; i < val1.Len(); i++ {
			if !compareObjects(val1.Index(i).Interface(), val2.Index(i).Interface()) {
				return false
			}
		}
		return true
	}
	if val1.Kind() != reflect.Map || val2.Kind() != reflect.Map {
		//return cmp.Equal(obj1, obj2)
		return reflect.DeepEqual(obj1, obj2)
	}

	keys1 := val1.MapKeys()
	for _, key := range keys1 {
		value1 := val1.MapIndex(key)
		value2 := val2.MapIndex(key)

		if !value2.IsValid() {
			return false
		}
		if !compareObjects(value1.Interface(), value2.Interface()) {
			return false
		}
	}
	return true
}

// compareField compares a specific field between two unstructured objects
func compareField(expected, actual *unstructured.Unstructured, field string) error {
	expectedField, expectedFound, err := unstructured.NestedMap(expected.Object, field)
	if err != nil {
		return fmt.Errorf("failed to get expected %s: %w", field, err)
	}
	if !expectedFound {
		return nil // field not set in expected, skip comparison
	}

	actualField, actualFound, err := unstructured.NestedMap(actual.Object, field)
	if err != nil {
		return fmt.Errorf("failed to get actual %s: %w", field, err)
	}
	if !actualFound {
		return fmt.Errorf("actual resource has no %s", field)
	}

	if !compareObjects(expectedField, actualField) {
		// Convert both fields to YAML for better readability
		expectedYAML, err := yamlutil.Marshal(expectedField)
		if err != nil {
			return fmt.Errorf("failed to marshal expected %s: %w", field, err)
		}
		actualYAML, err := yamlutil.Marshal(actualField)
		if err != nil {
			return fmt.Errorf("failed to marshal actual %s: %w", field, err)
		}
		return fmt.Errorf("%s do not match:\nExpected:\n%s\nActual:\n%s",
			field, string(expectedYAML), string(actualYAML))
	}

	return nil
}

// CompareSpecs compares the specs of two unstructured objects
func CompareSpecs(expected, actual *unstructured.Unstructured) error {
	return compareField(expected, actual, "spec")
}

// CompareStatus compares the status of two unstructured objects
func CompareStatus(expected, actual *unstructured.Unstructured) error {
	return compareField(expected, actual, "status")
}

// CompareMetadata compares the metadata of two unstructured objects
func CompareMetadata(expected, actual *unstructured.Unstructured) error {
	return compareField(expected, actual, "metadata")
}
