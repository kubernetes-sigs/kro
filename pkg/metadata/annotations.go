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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ApplyOrderAnnotation persists a managed resource's reverse topological deletion wave.
	ApplyOrderAnnotation = InternalKROPrefix + "apply-order"

	// ResourceGraphDefinitionNameAnnotation records the full name of the owning
	// ResourceGraphDefinition. Unlike the label of the same name, annotation
	// values are not limited to 63 characters, so this always holds the complete
	// name. Readers should use GetResourceGraphDefinitionName, which prefers
	// this annotation over the label.
	ResourceGraphDefinitionNameAnnotation = KROPrefix + "resource-graph-definition-name"
)


// GetResourceGraphDefinitionName returns the owning RGD name recorded on the
// object, preferring the annotation (always the complete name) over the label
// (omitted when the name does not fit in a label value). Objects written
// before the annotation existed carry only the label; for those the label is
// necessarily the full name, since longer names could never be written.
func GetResourceGraphDefinitionName(obj metav1.Object) (string, bool) {
	if name, ok := obj.GetAnnotations()[ResourceGraphDefinitionNameAnnotation]; ok {
		return name, true
	}
	name, ok := obj.GetLabels()[ResourceGraphDefinitionNameLabel]
	return name, ok
}
