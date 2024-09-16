// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package k8smetadata

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/aws-controllers-k8s/symphony/api/v1alpha1"
)

const symphonyFinalizer = v1alpha1.SymphonyDomainName + "/finalizer"

// SetResourceGroupFinalizer adds the Symphony finalizer to the object if it's not already present.
func SetResourceGroupFinalizer(obj metav1.Object) {
	if !HasResourceGroupFinalizer(obj) {
		obj.SetFinalizers(append(obj.GetFinalizers(), symphonyFinalizer))
	}
}

// RemoveResourceGroupFinalizer removes the Symphony finalizer from the object.
func RemoveResourceGroupFinalizer(obj metav1.Object) {
	obj.SetFinalizers(removeString(obj.GetFinalizers(), symphonyFinalizer))
}

// HasResourceGroupFinalizer checks if the object has the Symphony finalizer.
func HasResourceGroupFinalizer(obj metav1.Object) bool {
	return containsString(obj.GetFinalizers(), symphonyFinalizer)
}

// SetInstanceFinalizer adds an instance-specific finalizer to the object.
func SetInstanceFinalizer(obj metav1.Object, uid types.UID) {
	finalizerName := getInstanceFinalizerName(uid)
	if !HasInstanceFinalizer(obj, uid) {
		obj.SetFinalizers(append(obj.GetFinalizers(), finalizerName))
	}
}

// RemoveInstanceFinalizer removes an instance-specific finalizer from the object.
func RemoveInstanceFinalizer(obj metav1.Object, uid types.UID) {
	finalizerName := getInstanceFinalizerName(uid)
	obj.SetFinalizers(removeString(obj.GetFinalizers(), finalizerName))
}

// HasInstanceFinalizer checks if the object has an instance-specific finalizer.
func HasInstanceFinalizer(obj metav1.Object, uid types.UID) bool {
	finalizerName := getInstanceFinalizerName(uid)
	return containsString(obj.GetFinalizers(), finalizerName)
}

// SetInstanceFinalizerUnstructured adds an instance-specific finalizer to an unstructured object.
func SetInstanceFinalizerUnstructured(obj *unstructured.Unstructured, uid types.UID) error {
	finalizerName := getInstanceFinalizerName(uid)
	finalizers, found, err := unstructured.NestedStringSlice(obj.Object, "metadata", "finalizers")
	if err != nil {
		return fmt.Errorf("error getting finalizers: %w", err)
	}

	if !found || !containsString(finalizers, finalizerName) {
		finalizers = append(finalizers, finalizerName)
		if err := unstructured.SetNestedStringSlice(obj.Object, finalizers, "metadata", "finalizers"); err != nil {
			return fmt.Errorf("error setting finalizers: %w", err)
		}
	}
	return nil
}

// RemoveInstanceFinalizerUnstructured removes an instance-specific finalizer from an unstructured object.
func RemoveInstanceFinalizerUnstructured(obj *unstructured.Unstructured, uid types.UID) error {
	finalizerName := getInstanceFinalizerName(uid)
	finalizers, found, err := unstructured.NestedStringSlice(obj.Object, "metadata", "finalizers")
	if err != nil {
		return fmt.Errorf("error getting finalizers: %w", err)
	}

	if found {
		finalizers = removeString(finalizers, finalizerName)
		if err := unstructured.SetNestedStringSlice(obj.Object, finalizers, "metadata", "finalizers"); err != nil {
			return fmt.Errorf("error setting finalizers: %w", err)
		}
	}
	return nil
}

// HasInstanceFinalizerUnstructured checks if an unstructured object has an instance-specific finalizer.
func HasInstanceFinalizerUnstructured(obj *unstructured.Unstructured, uid types.UID) (bool, error) {
	finalizerName := getInstanceFinalizerName(uid)
	finalizers, found, err := unstructured.NestedStringSlice(obj.Object, "metadata", "finalizers")
	if err != nil {
		return false, fmt.Errorf("error getting finalizers: %w", err)
	}

	if !found {
		return false, nil
	}

	return containsString(finalizers, finalizerName), nil
}

// Helper functions

func getInstanceFinalizerName(uid types.UID) string {
	return fmt.Sprintf("%s.x.%s", string(uid), symphonyFinalizer)
}

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
