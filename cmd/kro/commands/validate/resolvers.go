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
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/common"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"sigs.k8s.io/yaml"

	"github.com/kubernetes-sigs/kro/cmd/kro/embeddedschemas"
	kroschema "github.com/kubernetes-sigs/kro/pkg/graph/schema"
)

const (
	// objectMetaSchemaRef is the $ref to the standard Kubernetes ObjectMeta definition
	// This matches the constant used in k8s.io/apiextensions-apiserver/pkg/controller/openapi/builder
	objectMetaSchemaRef = "#/definitions/io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta"
)

// OfflineSchemaResolver resolves OpenAPI schemas and scopes from embedded k8s data + local CRDs
// Implements both resolver.SchemaResolver and graph.ScopeResolver
type OfflineSchemaResolver struct {
	defs         map[string]common.OpenAPIDefinition
	isNamespaced map[string]bool   // key: "group/version/kind"
	resourceName map[string]string // key: "group/version/kind" → plural resource name
}

// NewOfflineSchemaResolver creates a resolver with embedded k8s schemas, optionally extended with local CRDs
func NewOfflineSchemaResolver(kubernetesVersion, crdDir string) (*OfflineSchemaResolver, error) {
	// Start with embedded k8s schemas
	if kubernetesVersion == "" {
		kubernetesVersion = getLatestEmbeddedVersion()
		if kubernetesVersion == "" {
			return nil, fmt.Errorf("no embedded Kubernetes schemas available")
		}
	}

	data, ok := embeddedschemas.BuiltInTypesByK8sVersion[kubernetesVersion]
	if !ok {
		available := getAvailableVersions()
		return nil, fmt.Errorf("kubernetes version %s not found in embedded schemas (available: %v)", kubernetesVersion, available)
	}

	// Load embedded schemas into map
	var rawSchemas map[string]spec.Schema
	if err := json.Unmarshal(data, &rawSchemas); err != nil {
		return nil, fmt.Errorf("unmarshaling embedded schema: %w", err)
	}

	defs := make(map[string]common.OpenAPIDefinition, len(rawSchemas))
	for key, schema := range rawSchemas {
		defs[key] = common.OpenAPIDefinition{Schema: schema}
	}

	// Load embedded scopes and resource names
	scopes, ok := embeddedschemas.ScopesByK8sVersion[kubernetesVersion]
	if !ok {
		return nil, fmt.Errorf("no scope data for kubernetes version %s", kubernetesVersion)
	}

	resourceNames, ok := embeddedschemas.ResourceNamesByK8sVersion[kubernetesVersion]
	if !ok {
		return nil, fmt.Errorf("no resource name data for kubernetes version %s", kubernetesVersion)
	}

	isNamespaced := make(map[string]bool, len(scopes))
	for key, scopeStr := range scopes {
		isNamespaced[key] = (scopeStr == "Namespaced")
	}

	resourceName := make(map[string]string, len(resourceNames))
	for key, name := range resourceNames {
		resourceName[key] = name
	}

	// Load CRDs from directory and add schemas, scopes, and resource names
	if crdDir != "" {
		if err := loadCRDsIntoResolver(crdDir, defs, isNamespaced, resourceName); err != nil {
			return nil, fmt.Errorf("loading CRDs from %s: %w", crdDir, err)
		}
	}

	return &OfflineSchemaResolver{
		defs:         defs,
		isNamespaced: isNamespaced,
		resourceName: resourceName,
	}, nil
}

func (r *OfflineSchemaResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	// Try k8s built-in format first: io.k8s.api.{group}.{version}.{Kind}
	// For core types without group: io.k8s.api.core.{version}.{Kind}
	group := gvk.Group
	if group == "" {
		group = "core"
	}

	key := fmt.Sprintf("io.k8s.api.%s.%s.%s", group, gvk.Version, gvk.Kind)
	if _, ok := r.defs[key]; !ok {
		// Try CRD format: {group}.{version}.{Kind}
		key = fmt.Sprintf("%s.%s.%s", gvk.Group, gvk.Version, gvk.Kind)
		if _, ok := r.defs[key]; !ok {
			return nil, fmt.Errorf("schema not found for %v", gvk)
		}
	}

	// Use k8s PopulateRefs to resolve $ref references
	schemaOf := func(ref string) (*spec.Schema, bool) {
		refKey := strings.TrimPrefix(ref, "#/definitions/")
		if refDef, ok := r.defs[refKey]; ok {
			return &refDef.Schema, true
		}
		return nil, false
	}

	resolved, err := resolver.PopulateRefs(schemaOf, "#/definitions/"+key)
	if err != nil {
		return nil, fmt.Errorf("resolving refs: %w", err)
	}
	return resolved, nil
}

// RESTMapping returns REST mapping for a given GroupKind (implements graph.ScopeResolver)
func (r *OfflineSchemaResolver) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	if len(versions) == 0 {
		return nil, fmt.Errorf("no version provided for %v", gk)
	}
	version := versions[0]

	key := fmt.Sprintf("%s/%s/%s", gk.Group, version, gk.Kind)
	isNs, ok := r.isNamespaced[key]
	if !ok {
		return nil, fmt.Errorf("no scope data for %v", schema.GroupVersionKind{
			Group:   gk.Group,
			Version: version,
			Kind:    gk.Kind,
		})
	}

	var scope meta.RESTScope
	if isNs {
		scope = meta.RESTScopeNamespace
	} else {
		scope = meta.RESTScopeRoot
	}

	// Get resource name (plural form)
	resourceNameStr := r.resourceName[key]

	return &meta.RESTMapping{
		Resource: schema.GroupVersionResource{
			Group:    gk.Group,
			Version:  version,
			Resource: resourceNameStr,
		},
		GroupVersionKind: schema.GroupVersionKind{Group: gk.Group, Version: version, Kind: gk.Kind},
		Scope:            scope,
	}, nil
}

// loadCRDsIntoResolver loads CRDs from a directory and adds schemas, scopes, and resource names
func loadCRDsIntoResolver(crdDir string, defs map[string]common.OpenAPIDefinition, isNamespaced map[string]bool, resourceName map[string]string) error {
	return filepath.WalkDir(crdDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		ext := filepath.Ext(path)
		if ext != ".yaml" && ext != ".yml" {
			fmt.Fprintf(os.Stderr, "Warning: skipping non-YAML file in CRD directory: %s\n", path)
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading CRD file %s: %w", path, err)
		}

		var crd extv1.CustomResourceDefinition
		if err := yaml.Unmarshal(data, &crd); err != nil {
			return fmt.Errorf("unmarshaling %s: %w", path, err)
		}

		if crd.Kind != "CustomResourceDefinition" {
			return fmt.Errorf("%s is not a CustomResourceDefinition (kind: %s)", path, crd.Kind)
		}

		// Determine scope once for this CRD
		isNs := (crd.Spec.Scope == extv1.NamespaceScoped)

		// Add each version's schema, scope, and resource name
		for _, version := range crd.Spec.Versions {
			scopeKey := fmt.Sprintf("%s/%s/%s", crd.Spec.Group, version.Name, crd.Spec.Names.Kind)
			isNamespaced[scopeKey] = isNs
			resourceName[scopeKey] = crd.Spec.Names.Plural

			if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
				continue
			}

			// Convert to spec.Schema and add to map
			// Note: CRDs use their actual group, not the io.k8s.api prefix
			specSchema, err := kroschema.ConvertJSONSchemaPropsToSpecSchema(version.Schema.OpenAPIV3Schema)
			if err != nil {
				return fmt.Errorf("converting schema for %s/%s/%s: %w", crd.Spec.Group, version.Name, crd.Spec.Names.Kind, err)
			}

			// Augment with standard Kubernetes metadata fields (kind, apiVersion, metadata.name, etc.)
			// This matches what the k8s API server does when serving CRD schemas
			specSchema = augmentWithKubernetesMetadata(specSchema)

			schemaKey := fmt.Sprintf("%s.%s.%s", crd.Spec.Group, version.Name, crd.Spec.Names.Kind)
			defs[schemaKey] = common.OpenAPIDefinition{Schema: *specSchema}
		}

		return nil
	})
}

// getLatestEmbeddedVersion returns the latest version from embedded schemas
func getLatestEmbeddedVersion() string {
	versions := getAvailableVersions()
	if len(versions) == 0 {
		return ""
	}
	return versions[len(versions)-1]
}

// getAvailableVersions returns sorted list of available embedded versions
func getAvailableVersions() []string {
	versions := make([]string, 0, len(embeddedschemas.BuiltInTypesByK8sVersion))
	for v := range embeddedschemas.BuiltInTypesByK8sVersion {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	return versions
}

// augmentWithKubernetesMetadata replaces CRD's minimal metadata field with a $ref to ObjectMeta.
// This matches what k8s.io/apiextensions-apiserver/pkg/controller/openapi/builder does.
func augmentWithKubernetesMetadata(s *spec.Schema) *spec.Schema {
	s.Properties["metadata"] = spec.Schema{
		SchemaProps: spec.SchemaProps{
			Ref: spec.MustCreateRef(objectMetaSchemaRef),
		},
	}
	return s
}
