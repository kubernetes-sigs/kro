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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeletionPolicy controls whether kro deletes or releases a resource it
// manages once that resource leaves the desired set or its instance is deleted.
type DeletionPolicy string

const (
	// DeletionPolicyDelete deletes the resource. This is the default.
	DeletionPolicyDelete DeletionPolicy = "Delete"
	// DeletionPolicyOrphaned leaves the resource in the cluster.
	DeletionPolicyOrphaned DeletionPolicy = "Orphaned"
)

// DeletionPolicyOf reads the deletion policy declared on a managed resource.
// An absent or unrecognised value is defaulted to DeletionPolicyDelete, so a resource
// written before this annotation was introduced, or one that has a typo, keeps the
// historical delete behaviour instead of failing or being interpreted incorrectly.
func DeletionPolicyOf(obj metav1.Object) DeletionPolicy {
	if obj == nil {
		return DeletionPolicyDelete
	}
	if obj.GetAnnotations()[DeletionPolicyAnnotation] == string(DeletionPolicyOrphaned) {
		return DeletionPolicyOrphaned
	}
	return DeletionPolicyDelete
}

// ReleaseKROMetadata strips the labels and annotations the instance
// controller stamped on a resource it manages, and reports whether anything
// was removed.
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

// releasedKeys are the keys the instance controller stamps on the resources it
// applies. Other kro keys (kro.run/reconcile, patch-contributions) are state
// of the object itself, e.g. a nested instance, and must survive a release.
var releasedKeys = map[string]struct{}{
	NodeIDLabel:              {},
	CollectionIndexLabel:     {},
	CollectionSizeLabel:      {},
	OwnedLabel:               {},
	KROVersionLabel:          {},
	InstanceIDLabel:          {},
	InstanceLabel:            {},
	InstanceNamespaceLabel:   {},
	InstanceGroupLabel:       {},
	InstanceVersionLabel:     {},
	InstanceKindLabel:        {},
	NodePathAnnotation:       {},
	ApplyOrderAnnotation:     {},
	InstanceNameAnnotation:   {},
	InstanceGroupAnnotation:  {},
	DeletionPolicyAnnotation: {},
}

// isKROKey reports whether a label/annotation key was stamped by kro on a
// managed resource. The value matters only for ManagedByLabelKey, which is a
// shared upstream key that other tools also use.
func isKROKey(key, value string) bool {
	if key == ManagedByLabelKey {
		return value == ManagedByKROValue
	}
	_, ok := releasedKeys[key]
	return ok
}
