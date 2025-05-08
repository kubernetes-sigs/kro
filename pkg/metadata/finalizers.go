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

package metadata

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kro-run/kro/api/v1alpha1"
)

const kroFinalizer = v1alpha1.KRODomainName + "/finalizer"

// SetResourceGraphDefinitionFinalizer adds the kro finalizer to the object if it's not already present.
func SetResourceGraphDefinitionFinalizer(obj metav1.Object) {
	if !HasResourceGraphDefinitionFinalizer(obj) {
		obj.SetFinalizers(append(obj.GetFinalizers(), kroFinalizer))
	}
}

// RemoveResourceGraphDefinitionFinalizer removes the kro finalizer from the object.
func RemoveResourceGraphDefinitionFinalizer(obj metav1.Object) {
	obj.SetFinalizers(removeString(obj.GetFinalizers(), kroFinalizer))
}

// HasResourceGraphDefinitionFinalizer checks if the object has the kro finalizer.
func HasResourceGraphDefinitionFinalizer(obj metav1.Object) bool {
	return containsString(obj.GetFinalizers(), kroFinalizer)
}

// SetInstanceFinalizerUnstructured adds an instance-specific finalizer to an unstructured object.
func SetInstanceFinalizerUnstructured(obj *unstructured.Unstructured) error {
	finalizers, found, err := unstructured.NestedStringSlice(obj.Object, "metadata", "finalizers")
	if err != nil {
		return fmt.Errorf("error getting finalizers: %w", err)
	}

	if !found || !containsString(finalizers, kroFinalizer) {
		finalizers = append(finalizers, kroFinalizer)
		if err := unstructured.SetNestedStringSlice(obj.Object, finalizers, "metadata", "finalizers"); err != nil {
			return fmt.Errorf("error setting finalizers: %w", err)
		}
	}
	return nil
}

// RemoveInstanceFinalizerUnstructured removes an instance-specific finalizer from an unstructured object.
func RemoveInstanceFinalizerUnstructured(obj *unstructured.Unstructured) error {
	finalizers, found, err := unstructured.NestedStringSlice(obj.Object, "metadata", "finalizers")
	if err != nil {
		return fmt.Errorf("error getting finalizers: %w", err)
	}

	if found {
		finalizers = removeString(finalizers, kroFinalizer)
		if err := unstructured.SetNestedStringSlice(obj.Object, finalizers, "metadata", "finalizers"); err != nil {
			return fmt.Errorf("error setting finalizers: %w", err)
		}
	}
	return nil
}

// HasInstanceFinalizerUnstructured checks if an unstructured object has an instance-specific finalizer.
func HasInstanceFinalizerUnstructured(obj *unstructured.Unstructured) (bool, error) {
	finalizers, found, err := unstructured.NestedStringSlice(obj.Object, "metadata", "finalizers")
	if err != nil {
		return false, fmt.Errorf("error getting finalizers: %w", err)
	}

	if !found {
		return false, nil
	}

	return containsString(finalizers, kroFinalizer), nil
}

// Helper functions

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) []string {
	result := make([]string, 0, len(slice))
	for _, item := range slice {
		if item != s {
			result = append(result, item)
		}
	}
	return result
}
