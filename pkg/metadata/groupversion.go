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
	"strings"

	"github.com/gobuffalo/flect"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kro-run/kro/api/v1alpha1"
)

const (
	KROInstancesGroupSuffix = v1alpha1.KRODomainName
)

// ExtractGVKFromUnstructured extracts the GroupVersionKind from an unstructured object.
func ExtractGVKFromUnstructured(unstructured map[string]interface{}) (schema.GroupVersionKind, error) {
	kind, ok := unstructured["kind"].(string)
	if !ok {
		return schema.GroupVersionKind{}, fmt.Errorf("kind not found or not a string")
	}

	apiVersion, ok := unstructured["apiVersion"].(string)
	if !ok {
		return schema.GroupVersionKind{}, fmt.Errorf("apiVersion not found or not a string")
	}

	parts := strings.Split(apiVersion, "/")
	if len(parts) > 2 {
		return schema.GroupVersionKind{}, fmt.Errorf("invalid apiVersion format: %s", apiVersion)
	}

	var group, version string
	if len(parts) == 2 {
		group, version = parts[0], parts[1]
	} else {
		version = parts[0]
	}

	return schema.GroupVersionKind{
		Group:   group,
		Version: version,
		Kind:    kind,
	}, nil
}

func GetResourceGraphDefinitionInstanceGVK(group, apiVersion, kind string) schema.GroupVersionKind {
	//pluralKind := flect.Pluralize(strings.ToLower(kind))

	return schema.GroupVersionKind{
		Group:   group,
		Version: apiVersion,
		Kind:    kind,
	}
}

func GetResourceGraphDefinitionInstanceGVR(group, apiVersion, kind string) schema.GroupVersionResource {
	pluralKind := flect.Pluralize(strings.ToLower(kind))
	return schema.GroupVersionResource{
		Group:    group,
		Version:  apiVersion,
		Resource: pluralKind,
	}
}

func GVRtoGVK(gvr schema.GroupVersionResource) schema.GroupVersionKind {
	singular := flect.Singularize(gvr.Resource)
	return schema.GroupVersionKind{
		Group:   gvr.Group,
		Version: gvr.Version,
		Kind:    singular,
	}
}

func GVKtoGVR(gvk schema.GroupVersionKind) schema.GroupVersionResource {
	resource := flect.Pluralize(strings.ToLower(gvk.Kind))
	return schema.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: resource,
	}
}
