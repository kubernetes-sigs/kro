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
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	applysetspec "github.com/kubernetes-sigs/kro/pkg/applyset"
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

// ReleaseKROMetadata strips every label and annotation kro stamps on a
// resource it manages, and reports whether anything was removed.
//
// Releasing is how a Detach resource leaves an instance: the object stays in
// the cluster but stops being an ApplySet member, so neither prune nor
// teardown can rediscover it (both list by the part-of label) and a later
// instance can adopt it without tripping the ApplySet conflict guard. kro's
// field manager is deliberately left in place — stripping managed fields buys
// nothing, and on the RGD path every template shares one field manager, so an
// adopting instance reuses it and never conflicts.
//
// Keys are matched by kro's domain prefixes rather than enumerated so a label
// added later cannot be silently left behind on a released object.
func ReleaseKROMetadata(obj metav1.Object) bool {
	if obj == nil {
		return false
	}
	labels, labelsChanged := stripKROKeys(obj.GetLabels())
	if labelsChanged {
		obj.SetLabels(labels)
	}
	annotations, annotationsChanged := stripKROKeys(obj.GetAnnotations())
	if annotationsChanged {
		obj.SetAnnotations(annotations)
	}
	return labelsChanged || annotationsChanged
}

// stripKROKeys returns m without the kro-applied keys, and whether any were
// present. The returned map is nil when every key was stripped, so the object
// ends up without an empty labels/annotations stanza.
func stripKROKeys(m map[string]string) (map[string]string, bool) {
	if len(m) == 0 {
		return m, false
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if isKROKey(k, v) {
			continue
		}
		out[k] = v
	}
	if len(out) == len(m) {
		return m, false
	}
	if len(out) == 0 {
		return nil, true
	}
	return out, true
}

// isKROKey reports whether a label/annotation key was stamped by kro on a
// managed resource. The value matters only for ManagedByLabelKey, which is a
// shared upstream key that other tools also use.
func isKROKey(key, value string) bool {
	switch {
	case strings.HasPrefix(key, KROPrefix), strings.HasPrefix(key, InternalKROPrefix):
		return true
	case key == applysetspec.ApplysetPartOfLabel:
		return true
	case key == ManagedByLabelKey:
		return value == ManagedByKROValue
	default:
		return false
	}
}
