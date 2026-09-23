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

package metadata

import (
	"maps"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// DeletionPolicyOf reads the deletion policy kro stamped on a managed
// resource. An absent or unrecognised value is DeletionPolicyDelete, so a
// resource written before this annotation existed, or one carrying a typo,
// keeps the historical delete behaviour rather than silently becoming
// undeletable.
func DeletionPolicyOf(obj metav1.Object) v1alpha1.DeletionPolicy {
	if obj == nil {
		return v1alpha1.DeletionPolicyDelete
	}
	if obj.GetAnnotations()[DeletionPolicyAnnotation] == string(v1alpha1.DeletionPolicyDetach) {
		return v1alpha1.DeletionPolicyDetach
	}
	return v1alpha1.DeletionPolicyDelete
}

// ReleaseKROMetadata strips every label and annotation kro has on a
// resource it manages, and reports whether anything was removed.
// kro's field manager is deliberately left in-place.
func ReleaseKROMetadata(obj metav1.Object) bool {
	if obj == nil {
		return false
	}

	// do not return stripKROKeys(obj.GetLabels()) || stripKROKeys(obj.GetAnnotations())
	// because that would short-circuit.
	changedLabels, labelsChanged := stripKROKeys(obj.GetLabels())
	if labelsChanged {
		obj.SetLabels(changedLabels)
	}
	changedAnnotations, annotationsChanged := stripKROKeys(obj.GetAnnotations())
	if annotationsChanged {
		obj.SetAnnotations(changedAnnotations)
	}
	return labelsChanged || annotationsChanged
}

// stripKROKeys removes all keys that belong to kro.
func stripKROKeys(m map[string]string) (map[string]string, bool) {
	if len(m) == 0 {
		return nil, false
	}
	l := len(m)
	maps.DeleteFunc(m, func(k, v string) bool {
		return isKROKey(k, v)
	})
	if len(m) == l {
		return m, false
	}
	return m, true
}

// isKROKey reports whether a label/annotation key was stamped by kro on a
// managed resource. The value matters only for ManagedByLabelKey, which is a
// shared upstream key that other tools also use.
func isKROKey(key, value string) bool {
	switch {
	case strings.HasPrefix(key, KROPrefix), strings.HasPrefix(key, InternalKROPrefix):
		return true
	case key == ManagedByLabelKey:
		return value == ManagedByKROValue
	default:
		return false
	}
}
