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

package graph

import (
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

type validator struct {
	maxCollectionDimensionSize int
	customDimensionLimit       bool
}

func newValidator() Validator { return &validator{} }

func newValidatorWithConfig(cfg RGDConfig) Validator {
	return &validator{
		maxCollectionDimensionSize: cfg.MaxCollectionDimensionSize,
		customDimensionLimit:       true,
	}
}

var (
	lowerCamelCase    = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)
	upperCamelCase    = regexp.MustCompile(`^[A-Z][a-zA-Z0-9]*$`)
	kubernetesVersion = regexp.MustCompile(`^v\d+(?:(?:alpha|beta)\d+)?$`)

	celReservedSymbols = sets.NewString(
		"true", "false", "null", "in",
		"as", "break", "const", "continue", "else",
		"for", "function", "if", "import", "let",
		"loop", "package", "namespace", "return",
		"var", "void", "while",
	)

	kroReservedWords = sets.NewString(
		"apiVersion", "context", "dependency", "dependencies",
		"each", "externalRef", "externalReference", "externalRefs",
		"externalReferences", "graph", "instance", "item", "items",
		"kind", "kro", "metadata", "namespace", "object", "resource",
		"resourcegraphdefinition", "resourceGraphDefinition", "resources",
		"root", "runtime", "schema", "self", "serviceAccountName",
		"spec", "status", "this", "variables", "vars", "version",
	)

	reservedWords = kroReservedWords.Union(celReservedSymbols)
)

func (v *validator) Validate(parsed *ParsedRGD) error {
	if err := v.validateInstance(parsed.Instance); err != nil {
		return terminal("validator", err)
	}

	allIDs := sets.NewString()
	for _, n := range parsed.Nodes {
		allIDs.Insert(n.ID)
	}

	seen := map[string]struct{}{}
	for _, n := range parsed.Nodes {
		if err := v.validateNodeID(n.ID, seen); err != nil {
			return terminal("validator", err)
		}
		seen[n.ID] = struct{}{}
	}

	for _, n := range parsed.Nodes {
		if err := v.validateNode(n, allIDs); err != nil {
			return terminal("validator", err)
		}
	}
	return nil
}

func (v *validator) validateInstance(inst *ParsedInstance) error {
	if !upperCamelCase.MatchString(inst.Kind) {
		return fmt.Errorf("kind %q is not a valid KRO kind name (must be UpperCamelCase)", inst.Kind)
	}
	if !kubernetesVersion.MatchString(inst.APIVersion) {
		return fmt.Errorf("apiVersion %q is not valid", inst.APIVersion)
	}
	return nil
}

func (v *validator) validateNodeID(id string, seen map[string]struct{}) error {
	if _, dup := seen[id]; dup {
		return fmt.Errorf("found duplicate resource IDs: %q", id)
	}
	if reservedWords.Has(id) {
		return fmt.Errorf("resource ID %q: naming convention violation (reserved word)", id)
	}
	if !lowerCamelCase.MatchString(id) {
		return fmt.Errorf("resource ID %q: naming convention violation (must be lowerCamelCase)", id)
	}
	return nil
}

func (v *validator) validateNode(r *ParsedNode, allIDs sets.String) error {
	if !r.HasTemplate && !r.HasExternalRef {
		return fmt.Errorf("resource %q: must have template or externalRef", r.ID)
	}
	if r.HasTemplate && r.HasExternalRef {
		return fmt.Errorf("resource %q: cannot use externalRef with template", r.ID)
	}
	if err := validateObjectStructure(r.Template); err != nil {
		return fmt.Errorf("resource %q: %w", r.ID, err)
	}
	if r.HasExternalRef && len(r.ForEach) > 0 {
		return fmt.Errorf("resource %q: cannot use externalRef with forEach", r.ID)
	}
	if err := v.validateIterators(r, allIDs); err != nil {
		return err
	}
	if err := validateNoKROLabels(r.ID, r.Template); err != nil {
		return err
	}
	if err := validateCRDExpressions(r); err != nil {
		return err
	}
	return nil
}

func (v *validator) validateIterators(r *ParsedNode, allIDs sets.String) error {
	limit := defaultRGDConfig().MaxCollectionDimensionSize
	if v.customDimensionLimit {
		limit = v.maxCollectionDimensionSize
	}
	if len(r.ForEach) > limit {
		return fmt.Errorf(
			"resource %q: forEach cannot have more than %d dimensions, got %d",
			r.ID, limit, len(r.ForEach),
		)
	}

	seen := sets.NewString()
	for _, fe := range r.ForEach {
		if !lowerCamelCase.MatchString(fe.Name) {
			return fmt.Errorf("resource %q: forEach %q must be lowerCamelCase", r.ID, fe.Name)
		}
		if reservedWords.Has(fe.Name) {
			return fmt.Errorf("resource %q: forEach %q is a reserved keyword", r.ID, fe.Name)
		}
		if allIDs.Has(fe.Name) {
			return fmt.Errorf("resource %q: forEach %q conflicts with resource ID", r.ID, fe.Name)
		}
		if seen.Has(fe.Name) {
			return fmt.Errorf("resource %q: duplicate forEach %q", r.ID, fe.Name)
		}
		seen.Insert(fe.Name)
	}
	return nil
}

func validateObjectStructure(obj map[string]interface{}) error {
	if av, ok := obj["apiVersion"]; !ok {
		return fmt.Errorf("is not a valid Kubernetes object: missing apiVersion")
	} else if _, ok := av.(string); !ok {
		return fmt.Errorf("is not a valid Kubernetes object: apiVersion must be string")
	}
	if k, ok := obj["kind"]; !ok {
		return fmt.Errorf("is not a valid Kubernetes object: missing kind")
	} else if _, ok := k.(string); !ok {
		return fmt.Errorf("is not a valid Kubernetes object: kind must be string")
	}
	if md, ok := obj["metadata"]; !ok {
		return fmt.Errorf("metadata field not found")
	} else if _, ok := md.(map[string]interface{}); !ok {
		return fmt.Errorf("metadata field not found: metadata must be map")
	}
	return nil
}

// validateCRDExpressions enforces that CRDs (apiextensions.k8s.io/v1 CustomResourceDefinition)
// only have CEL expressions in metadata fields. Expressions in spec/status fields of CRDs
// could create bootstrap problems.
func validateCRDExpressions(r *ParsedNode) error {
	apiVersion, _ := r.Template["apiVersion"].(string)
	kind, _ := r.Template["kind"].(string)
	if apiVersion != "apiextensions.k8s.io/v1" || kind != "CustomResourceDefinition" {
		return nil
	}
	for _, f := range r.Fields {
		if !strings.HasPrefix(f.Path, "metadata.") {
			return fmt.Errorf("resource %q: CEL expressions in CRDs are only supported for metadata fields, found in path %q", r.ID, f.Path)
		}
	}
	return nil
}

func validateNoKROLabels(id string, obj map[string]interface{}) error {
	md, _ := obj["metadata"].(map[string]interface{})
	labels, _ := md["labels"].(map[string]interface{})
	for key := range labels {
		if strings.HasPrefix(key, metadata.LabelKROPrefix) {
			return fmt.Errorf("resource %q: label prefix %q is reserved", id, metadata.LabelKROPrefix)
		}
	}
	return nil
}
