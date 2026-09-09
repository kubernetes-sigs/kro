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

package instance

import (
	"maps"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/validate/content"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// labelForIdentityAnnotation maps an identity annotation to the label whose
// value had to be hashed for it to be stamped.
var labelForIdentityAnnotation = map[string]string{
	metadata.InstanceNameAnnotation:  metadata.InstanceLabel,
	metadata.InstanceGroupAnnotation: metadata.InstanceGroupLabel,
}

// warnOnEncodedInstanceLabels emits one event per instance identity value too
// long to be stored verbatim in its label.
func (c *Controller) warnOnEncodedInstanceLabels(inst *unstructured.Unstructured, annotations map[string]string) {
	if c.eventRecorder == nil {
		return
	}
	for annotation, label := range labelForIdentityAnnotation {
		full, ok := annotations[annotation]
		if !ok {
			continue
		}
		c.eventRecorder.Eventf(inst, corev1.EventTypeWarning, "InstanceLabelEncoded",
			"%q exceeds the %d character label value limit; %s is set to %q on this instance's managed "+
				"resources. The full value is preserved in the %s annotation",
			full, content.LabelValueMaxLength, label,
			metadata.LabelValueToken(full), annotation)
	}
}

// applyInstanceIdentityAnnotations stamps the preserved identity values onto a
// managed resource. A nil map (nothing was hashed) leaves obj untouched.
func applyInstanceIdentityAnnotations(obj *unstructured.Unstructured, annotations map[string]string) {
	if len(annotations) == 0 {
		return
	}
	existing := obj.GetAnnotations()
	if existing == nil {
		existing = make(map[string]string, len(annotations))
	}
	maps.Copy(existing, annotations)

	obj.SetAnnotations(existing)
}
