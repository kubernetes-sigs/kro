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

package validate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

func TestOfflineRESTMapper(t *testing.T) {
	resolver, err := NewOfflineSchemaResolver("v1.30", "")
	require.NoError(t, err)

	tests := []struct {
		name          string
		group         string
		version       string
		kind          string
		expectError   bool
		expectCluster bool // true if cluster-scoped
	}{
		{
			name:          "core Pod is namespaced",
			group:         "",
			version:       "v1",
			kind:          "Pod",
			expectError:   false,
			expectCluster: false,
		},
		{
			name:          "apps Deployment is namespaced",
			group:         "apps",
			version:       "v1",
			kind:          "Deployment",
			expectError:   false,
			expectCluster: false,
		},
		{
			name:          "core Namespace is cluster-scoped",
			group:         "",
			version:       "v1",
			kind:          "Namespace",
			expectError:   false,
			expectCluster: true,
		},
		{
			name:          "StorageClass is cluster-scoped",
			group:         "storage.k8s.io",
			version:       "v1",
			kind:          "StorageClass",
			expectError:   false,
			expectCluster: true,
		},
		{
			name:        "unknown type errors",
			group:       "custom.io",
			version:     "v1",
			kind:        "CustomResource",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mapping, err := resolver.RESTMapping(
				schema.GroupKind{Group: tt.group, Kind: tt.kind},
				tt.version,
			)

			if tt.expectError {
				assert.Error(t, err, "expected error for unknown type")
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.group, mapping.GroupVersionKind.Group)
			assert.Equal(t, tt.version, mapping.GroupVersionKind.Version)
			assert.Equal(t, tt.kind, mapping.GroupVersionKind.Kind)

			if tt.expectCluster {
				assert.Equal(t, meta.RESTScopeRoot, mapping.Scope, "expected cluster-scoped")
			} else {
				assert.Equal(t, meta.RESTScopeNamespace, mapping.Scope, "expected namespaced")
			}

			// Verify GVR is properly populated
			assert.NotEmpty(t, mapping.Resource.Resource, "GVR resource name should be populated")
			assert.Equal(t, tt.group, mapping.Resource.Group)
			assert.Equal(t, tt.version, mapping.Resource.Version)
		})
	}
}

func TestValidateRGD_WithEmbeddedSchemas(t *testing.T) {
	// Create a simple RGD that uses core k8s types
	rgdYAML := `apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: test-deployment
spec:
  schema:
    apiVersion: v1alpha1
    kind: TestApp
    spec:
      name: string
      replicas: integer
  resources:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.spec.name}
        spec:
          replicas: ${schema.spec.replicas}
          selector:
            matchLabels:
              app: test
          template:
            metadata:
              labels:
                app: test
            spec:
              containers:
                - name: nginx
                  image: nginx:latest
`

	tmpDir := t.TempDir()
	rgdFile := filepath.Join(tmpDir, "test-rgd.yaml")
	require.NoError(t, os.WriteFile(rgdFile, []byte(rgdYAML), 0644))

	// Save original flags
	origFile := resourceGroupDefinitionFile
	origVersion := kubernetesVersion
	origFromCluster := fromCluster
	defer func() {
		resourceGroupDefinitionFile = origFile
		kubernetesVersion = origVersion
		fromCluster = origFromCluster
	}()

	resourceGroupDefinitionFile = rgdFile
	kubernetesVersion = "v1.30"
	fromCluster = false

	err := validateRGDCmd.RunE(nil, nil)
	assert.NoError(t, err, "embedded schemas should work")
}

func TestValidateRGD_InvalidYAML(t *testing.T) {
	invalidYAML := `this is not: valid: yaml: content`

	tmpDir := t.TempDir()
	rgdFile := filepath.Join(tmpDir, "invalid.yaml")
	require.NoError(t, os.WriteFile(rgdFile, []byte(invalidYAML), 0644))

	origFile := resourceGroupDefinitionFile
	origVersion := kubernetesVersion
	defer func() {
		resourceGroupDefinitionFile = origFile
		kubernetesVersion = origVersion
	}()

	resourceGroupDefinitionFile = rgdFile
	kubernetesVersion = "v1.30"

	err := validateRGDCmd.RunE(nil, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal")
}

func TestValidateRGD_MissingFile(t *testing.T) {
	origFile := resourceGroupDefinitionFile
	defer func() {
		resourceGroupDefinitionFile = origFile
	}()

	resourceGroupDefinitionFile = "/nonexistent/path/file.yaml"

	err := validateRGDCmd.RunE(nil, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "reading")
}

func TestValidateRGD_MissingSchemaKind(t *testing.T) {
	// RGD with schema but missing required field
	rgdYAML := `apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: test-missing-kind
spec:
  schema:
    apiVersion: v1alpha1
    spec:
      name: string
  resources:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: test
`

	tmpDir := t.TempDir()
	rgdFile := filepath.Join(tmpDir, "missing-kind.yaml")
	require.NoError(t, os.WriteFile(rgdFile, []byte(rgdYAML), 0644))

	origFile := resourceGroupDefinitionFile
	origVersion := kubernetesVersion
	defer func() {
		resourceGroupDefinitionFile = origFile
		kubernetesVersion = origVersion
	}()

	resourceGroupDefinitionFile = rgdFile
	kubernetesVersion = "v1.30"

	err := validateRGDCmd.RunE(nil, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "kind")
}

func TestValidateRGD_CRDLoading(t *testing.T) {
	// Test that FileResolver can load CRDs from a directory
	crdYAML := `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: mycustomresources.custom.io
spec:
  group: custom.io
  names:
    kind: MyCustomResource
    plural: mycustomresources
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                field1:
                  type: string
`

	tmpDir := t.TempDir()
	crdFile := filepath.Join(tmpDir, "mycustomresource.yaml")
	require.NoError(t, os.WriteFile(crdFile, []byte(crdYAML), 0644))

	// Test OfflineSchemaResolver loads the CRD
	resolver, err := NewOfflineSchemaResolver("v1.30", tmpDir)
	require.NoError(t, err, "should load CRDs from directory")

	// Test that the schema can be resolved
	gvk := schema.GroupVersionKind{
		Group:   "custom.io",
		Version: "v1",
		Kind:    "MyCustomResource",
	}
	resolvedSchema, err := resolver.ResolveSchema(gvk)
	assert.NoError(t, err, "should resolve custom CRD schema")
	assert.NotNil(t, resolvedSchema)

	// Test that same resolver also provides scope information
	gk := schema.GroupKind{Group: "custom.io", Kind: "MyCustomResource"}
	mapping, err := resolver.RESTMapping(gk, "v1")
	assert.NoError(t, err, "resolver should provide scope for CRD")
	assert.Equal(t, meta.RESTScopeNamespace, mapping.Scope)
}

func TestValidateRGD_MissingCRD(t *testing.T) {
	// RGD that references a CRD we don't have
	rgdYAML := `apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: test-missing-crd
spec:
  schema:
    apiVersion: v1alpha1
    kind: TestMissing
    spec:
      name: string
  resources:
    - id: custom
      template:
        apiVersion: missing.io/v1
        kind: MissingResource
        metadata:
          name: ${schema.spec.name}
`

	tmpDir := t.TempDir()
	rgdFile := filepath.Join(tmpDir, "test-rgd.yaml")
	require.NoError(t, os.WriteFile(rgdFile, []byte(rgdYAML), 0644))

	origFile := resourceGroupDefinitionFile
	origVersion := kubernetesVersion
	defer func() {
		resourceGroupDefinitionFile = origFile
		kubernetesVersion = origVersion
	}()

	resourceGroupDefinitionFile = rgdFile
	kubernetesVersion = "v1.30"

	err := validateRGDCmd.RunE(nil, nil)
	assert.Error(t, err, "should fail when CRD not found")
	assert.Contains(t, err.Error(), "schema not found")
}

func TestAugmentWithKubernetesMetadata(t *testing.T) {
	// Test that augmentWithKubernetesMetadata adds ObjectMeta $ref
	schema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"apiVersion": {
					SchemaProps: spec.SchemaProps{Type: []string{"string"}},
				},
				"kind": {
					SchemaProps: spec.SchemaProps{Type: []string{"string"}},
				},
				"metadata": {
					SchemaProps: spec.SchemaProps{Type: []string{"object"}},
				},
				"spec": {
					SchemaProps: spec.SchemaProps{Type: []string{"object"}},
				},
			},
		},
	}

	augmented := augmentWithKubernetesMetadata(schema)

	// Check that metadata now has a $ref to ObjectMeta
	metadata := augmented.Properties["metadata"]
	assert.NotEmpty(t, metadata.Ref.String(), "metadata should have a $ref")
	assert.Contains(t, metadata.Ref.String(), "io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta",
		"metadata should reference ObjectMeta")

	// Check that other properties are preserved
	assert.NotNil(t, augmented.Properties["apiVersion"])
	assert.NotNil(t, augmented.Properties["kind"])
	assert.NotNil(t, augmented.Properties["spec"])
}

func TestValidateRGD_CRDWithMetadataReference(t *testing.T) {
	// Test that RGDs can reference metadata.name, metadata.namespace from CRDs
	crdYAML := `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: myapps.example.io
spec:
  group: example.io
  names:
    kind: MyApp
    plural: myapps
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            apiVersion:
              type: string
            kind:
              type: string
            metadata:
              type: object
            spec:
              type: object
              properties:
                appName:
                  type: string
`

	// RGD that references metadata.name and metadata.namespace
	rgdYAML := `apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: test-metadata-ref
spec:
  schema:
    apiVersion: v1alpha1
    kind: TestMetadata
    spec:
      appName: string
  resources:
    - id: myapp
      template:
        apiVersion: example.io/v1
        kind: MyApp
        metadata:
          name: ${schema.spec.appName}
          namespace: default
        spec:
          appName: ${schema.spec.appName}
`

	tmpDir := t.TempDir()
	crdDir := filepath.Join(tmpDir, "crds")
	require.NoError(t, os.MkdirAll(crdDir, 0755))

	crdFile := filepath.Join(crdDir, "myapp-crd.yaml")
	require.NoError(t, os.WriteFile(crdFile, []byte(crdYAML), 0644))

	rgdFile := filepath.Join(tmpDir, "test-rgd.yaml")
	require.NoError(t, os.WriteFile(rgdFile, []byte(rgdYAML), 0644))

	origFile := resourceGroupDefinitionFile
	origVersion := kubernetesVersion
	origCrdsDir := crdsDir
	defer func() {
		resourceGroupDefinitionFile = origFile
		kubernetesVersion = origVersion
		crdsDir = origCrdsDir
	}()

	resourceGroupDefinitionFile = rgdFile
	kubernetesVersion = "v1.30"
	crdsDir = crdDir

	// This should succeed because augmentWithKubernetesMetadata adds ObjectMeta $ref
	err := validateRGDCmd.RunE(nil, nil)
	assert.NoError(t, err, "should validate RGD that references metadata.name and metadata.namespace")
}
