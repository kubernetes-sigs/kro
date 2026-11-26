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

// Package rgdfile reads ResourceGraphDefinition files for the kro CLI
// commands.
package rgdfile

import (
	"fmt"
	"os"
	"slices"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	kjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// scopes are the values the RGD CRD allows for spec.schema.scope.
var scopes = []v1alpha1.ResourceScope{v1alpha1.ResourceScopeNamespaced, v1alpha1.ResourceScopeCluster}

// Read reads the ResourceGraphDefinition in path and checks it the way the
// API server checks one on create, so the commands work on the RGD that the
// cluster would run:
//
//   - Unknown and duplicate fields are errors, as with kubectl's default
//     strict field validation, so a misspelled field isn't dropped.
//   - A spec.schema.scope that is set must be Namespaced or Cluster, as the
//     CRD's enum requires. An omitted or null scope is Namespaced.
//   - An omitted, null or empty spec.schema.group is kro.run. The API server
//     keeps an empty group, but the controller builds the instance CRD in
//     kro.run for it too.
func Read(path string) (*v1alpha1.ResourceGraphDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read ResourceGraphDefinition file: %w", err)
	}
	return decode(data)
}

// scopeField records whether a file sets spec.schema.scope. A nil pointer
// means the field is omitted or null.
type scopeField struct {
	Spec struct {
		Schema struct {
			Scope *string `json:"scope"`
		} `json:"schema"`
	} `json:"spec"`
}

func decode(data []byte) (*v1alpha1.ResourceGraphDefinition, error) {
	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal ResourceGraphDefinition: %w", err)
	}

	var rgd v1alpha1.ResourceGraphDefinition
	strictErrs, err := kjson.UnmarshalStrict(jsonData, &rgd)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal ResourceGraphDefinition: %w", err)
	}
	if len(strictErrs) > 0 {
		return nil, fmt.Errorf("failed to unmarshal ResourceGraphDefinition: %w",
			runtime.NewStrictDecodingError(strictErrs))
	}
	schema := rgd.Spec.Schema
	if schema == nil {
		// The graph builder reports the missing schema.
		return &rgd, nil
	}

	var set scopeField
	if err := kjson.UnmarshalCaseSensitivePreserveInts(jsonData, &set); err != nil {
		return nil, fmt.Errorf("failed to unmarshal ResourceGraphDefinition: %w", err)
	}
	if set.Spec.Schema.Scope != nil && !slices.Contains(scopes, schema.Scope) {
		return nil, fmt.Errorf("invalid ResourceGraphDefinition: %w",
			field.NotSupported(field.NewPath("spec", "schema", "scope"), string(schema.Scope), scopes))
	}
	if schema.Group == "" {
		schema.Group = v1alpha1.KRODomainName
	}
	if schema.Scope == "" {
		schema.Scope = v1alpha1.ResourceScopeNamespaced
	}
	return &rgd, nil
}
