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
	"context"
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ApplyManifests applies all yaml manifests in the specified directory
func ApplyManifests(ctx context.Context, config *rest.Config, dir string) error {
	// Create client
	c, err := client.New(config, client.Options{})
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}

	// Walk through directory
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Skip non-yaml files
		if filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml" {
			return nil
		}

		// Read file
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", path, err)
		}

		// Decode yaml
		decoder := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)
		obj := &unstructured.Unstructured{}
		_, _, err = decoder.Decode(data, nil, obj)
		if err != nil {
			return fmt.Errorf("failed to decode yaml %s: %w", path, err)
		}

		// Create or update resource
		err = c.Create(ctx, obj)
		if err != nil {
			if errors.IsAlreadyExists(err) {
				// Update if already exists
				existing := &unstructured.Unstructured{}
				existing.SetGroupVersionKind(obj.GroupVersionKind())
				err = c.Get(ctx, client.ObjectKey{
					Namespace: obj.GetNamespace(),
					Name:      obj.GetName(),
				}, existing)
				if err != nil {
					return fmt.Errorf("failed to get existing resource %s: %w", path, err)
				}

				// Set resource version
				obj.SetResourceVersion(existing.GetResourceVersion())

				err = c.Update(ctx, obj)
				if err != nil {
					return fmt.Errorf("failed to update resource %s: %w", path, err)
				}
			} else {
				return fmt.Errorf("failed to create resource %s: %w", path, err)
			}
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to walk directory %s: %w", dir, err)
	}

	return nil
}
