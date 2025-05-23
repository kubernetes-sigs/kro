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
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/apimachinery/pkg/util/sets"
	yamlutil "sigs.k8s.io/yaml"
)

// wellKnownClusterScopedKinds is a set of well-known cluster-scoped kinds
var wellKnownClusterScopedKinds = sets.NewString(
	"Namespace",
	"ClusterRole",
	"ClusterRoleBinding",
	"CustomResourceDefinition",
	"PriorityClass",
	"StorageClass",
	"VolumeSnapshotClass",
	"PersistentVolume",
	"CSIDriver",
	"CSINode",
	"NodeClass",
)

// resourceInfo holds information about a Kubernetes resource
type resourceInfo struct {
	obj             *unstructured.Unstructured
	isClusterScoped bool
	filename        string
}

// String returns a string representation of the resource
func (r *resourceInfo) String() string {
	return fmt.Sprintf("%s/%s[%s]", r.obj.GetKind(), r.obj.GetName(), r.filename)
}

// LoadObjectsFromDirectory reads and applies Kubernetes manifests from a directory with proper ordering
func LoadObjectsFromDirectory(path string) ([]resourceInfo, error) {
	// Collect all resources from the directory
	var resources []resourceInfo
	err := filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || (!strings.HasSuffix(info.Name(), ".yaml") && !strings.HasSuffix(info.Name(), ".yml")) {
			return nil
		}

		// Read file
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", path, err)
		}

		// Split content into multiple documents
		docs := bytes.Split(content, []byte("---\n"))
		for _, doc := range docs {
			if len(bytes.TrimSpace(doc)) == 0 {
				continue
			}

			// Decode YAML to unstructured
			var obj unstructured.Unstructured
			if err := yamlutil.Unmarshal(doc, &obj); err != nil {
				return fmt.Errorf("failed to decode file %s: %w", path, err)
			}

			if obj.Object == nil {
				continue
			}

			resources = append(resources, resourceInfo{
				obj:      &obj,
				filename: info.Name(),
			})
		}
		return nil
	})
	if err != nil {
		return resources, fmt.Errorf("failed to walk directory: %w", err)
	}

	return resources, nil
}

// ReadYAMLFile reads a YAML file and decodes it into the given object
func ReadYAMLFile(path string, obj *unstructured.Unstructured) error {
	// Read file
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	// Decode YAML
	decoder := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)
	_, _, err = decoder.Decode(data, nil, obj)
	if err != nil {
		return fmt.Errorf("failed to decode yaml: %w", err)
	}

	return nil
}
