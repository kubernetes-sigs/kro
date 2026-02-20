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

package metadata

import (
	"fmt"
	"strings"

	"github.com/gobuffalo/flect"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

const (
	KROInstancesGroupSuffix = v1alpha1.KRODomainName
)

// ExtractGVKFromUnstructured extracts the GroupVersionKind from an unstructured object.
// It performs early validation to fail fast and avoid unnecessary API calls:
// - apiVersion: parsed with schema.ParseGroupVersion for proper validation
// - kind: validated as DNS-1035 label after lowercasing
func ExtractGVKFromUnstructured(unstructured map[string]interface{}) (schema.GroupVersionKind, error) {
	kind, ok := unstructured["kind"].(string)
	if !ok {
		return schema.GroupVersionKind{}, fmt.Errorf("kind not found or not a string")
	}

	apiVersion, ok := unstructured["apiVersion"].(string)
	if !ok {
		return schema.GroupVersionKind{}, fmt.Errorf("apiVersion not found or not a string")
	}

	// Early validation: parse apiVersion using schema.ParseGroupVersion
	// This validates the format before attempting schema resolution
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("invalid apiVersion format %q: %w", apiVersion, err)
	}

	// Early validation: validate kind as DNS-1035 label after lowercasing
	// This ensures the kind is valid before attempting API lookups
	kindLower := strings.ToLower(kind)
	if errs := validation.IsDNS1035Label(kindLower); len(errs) > 0 {
		return schema.GroupVersionKind{}, fmt.Errorf("invalid kind %q: %s (must be a valid DNS-1035 label when lowercased)", kind, strings.Join(errs, ", "))
	}

	return schema.GroupVersionKind{
		Group:   gv.Group,
		Version: gv.Version,
		Kind:    kind,
	}, nil
}

func GetResourceGraphDefinitionInstanceGVR(group, apiVersion, kind string) schema.GroupVersionResource {
	pluralKind := flect.Pluralize(strings.ToLower(kind))
	return schema.GroupVersionResource{
		Group:    group,
		Version:  apiVersion,
		Resource: pluralKind,
	}
}
