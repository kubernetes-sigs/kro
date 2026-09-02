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
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/cel/ast"
	"github.com/kubernetes-sigs/kro/pkg/graph/parser"
	"github.com/kubernetes-sigs/kro/pkg/graph/schema"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

var (
	// ErrNamingConvention is the base error message for naming convention violations
	ErrNamingConvention = "naming convention violation"
)

var (
	// lowerCamelCaseRegex
	lowerCamelCaseRegex = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)
	// UpperCamelCaseRegex
	upperCamelCaseRegex = regexp.MustCompile(`^[A-Z][a-zA-Z0-9]*$`)
	// kubernetesVersionRegex
	kubernetesVersionRegex = regexp.MustCompile(`^v\d+(?:(?:alpha|beta)\d+)?$`)

	// celReservedSymbols is a list of RESERVED symbols defined in the CEL lexer.
	// No identifiers are allowed to collide with these symbols.
	// https://github.com/google/cel-spec/blob/master/doc/langdef.md#syntax
	celReservedSymbols = sets.NewString(
		"true", "false", "null", "in",
		"as", "break", "const", "continue", "else",
		"for", "function", "if", "import", "let",
		"loop", "package", "namespace", "return",
		"var", "void", "while",
	)

	// kroReservedKeyWords is a list of reserved words in kro.
	kroReservedKeyWords = sets.NewString(
		"apiVersion",
		"context",
		"dependency",
		"dependencies",
		"each", // Reserved for per-item readiness in collections
		"externalRef",
		"externalReference",
		"externalRefs",
		"externalReferences",
		"graph",
		"graphengine",
		"instance",
		"item",
		"items",
		"kind",
		"kro",
		"metadata",
		"namespace",
		"object",
		"resource",
		"resourcegraphdefinition",
		"resourceGraphDefinition",
		"resources",
		"root",
		"runtime",
		"schema",
		"self",
		"serviceAccountName",
		"spec",
		"status",
		"this",
		"variables",
		"vars",
		"version",
	)

	reservedKeyWords = kroReservedKeyWords.Union(celReservedSymbols)
)

// isValidResourceID checks if the given id is a valid KRO resource id (loawercase)
func isValidResourceID(id string) bool {
	return lowerCamelCaseRegex.MatchString(id)
}

// isValidKindName checks if the given name is a valid KRO kind name (uppercase)
func isValidKindName(name string) bool {
	return upperCamelCaseRegex.MatchString(name)
}

// isKROReservedWord checks if the given word is a reserved word in KRO.
func isKROReservedWord(word string) bool {
	return reservedKeyWords.Has(word)
}

// IsKROReservedWord checks if the given word is a reserved word in KRO.
func IsKROReservedWord(word string) bool {
	return isKROReservedWord(word)
}

// validateResourceGraphDefinition validates the naming conventions of
// the given resource graph definition, the resources defined in them, and the constraints
// defined in rgdConfig for resource collections.
func validateResourceGraphDefinition(rgd *v1alpha1.ResourceGraphDefinition, rgdConfig Config) error {
	if !isValidKindName(rgd.Spec.Schema.Kind) {
		return fmt.Errorf("%s: kind '%s' is not a valid KRO kind name: must be UpperCamelCase", ErrNamingConvention, rgd.Spec.Schema.Kind)
	}

	ids := make([]string, 0, len(rgd.Spec.Resources))
	for _, res := range rgd.Spec.Resources {
		ids = append(ids, res.ID)
	}
	if err := validateResourceIDs(ids); err != nil {
		return fmt.Errorf("%s: %w", ErrNamingConvention, err)
	}

	// Validate forEach iterators after collecting all resource IDs
	resourceIDs := sets.NewString(ids...)
	for _, res := range rgd.Spec.Resources {
		dims := make([]map[string]string, len(res.ForEach))
		for i, d := range res.ForEach {
			dims[i] = d
		}
		if err := validateForEachDimensions(res.ID, dims, resourceIDs, rgdConfig.MaxCollectionDimensionSize); err != nil {
			return err
		}
	}
	return nil
}

// validateResourceIDs checks that resource ids are unique and conform to the
// KRO naming convention:
// - The id should start with a lowercase letter.
// - The id should only contain alphanumeric characters.
// - Does not contain any special characters, underscores, or hyphens.
func validateResourceIDs(ids []string) error {
	seen := make(map[string]struct{})
	for _, id := range ids {
		if isKROReservedWord(id) {
			return fmt.Errorf("id %s is a reserved keyword in KRO", id)
		}

		if !isValidResourceID(id) {
			return fmt.Errorf("id %s is not a valid KRO resource id: must be lower camelCase", id)
		}

		if _, ok := seen[id]; ok {
			return fmt.Errorf("found duplicate resource IDs %s", id)
		}
		seen[id] = struct{}{}
	}

	return nil
}

// validateForEachDimensions validates the forEach iterators for a resource.
// It checks that:
// - Iterator names are valid identifiers (lowerCamelCase)
// - Iterator names are not reserved keywords
// - Iterator names do not conflict with resource IDs
// - Iterator names are unique within the same resource
func validateForEachDimensions(resourceID string, forEach []map[string]string, resourceIDs sets.String, maxDimensions int) error {
	if len(forEach) > maxDimensions {
		return fmt.Errorf("resource %q: forEach cannot have more "+
			"than %d dimensions, got %d", resourceID, maxDimensions, len(forEach))
	}

	if len(forEach) == 0 {
		return nil
	}

	seenIterators := sets.NewString()
	for _, iterMap := range forEach {
		for iterName := range iterMap {
			// Check if iterator name is a valid identifier
			if !isValidResourceID(iterName) {
				return fmt.Errorf("resource %q: forEach iterator name %q is not valid: must be lowerCamelCase", resourceID, iterName)
			}

			// Check if iterator name is a reserved keyword
			if isKROReservedWord(iterName) {
				return fmt.Errorf("resource %q: forEach iterator name %q is a reserved keyword", resourceID, iterName)
			}

			// Check if iterator name conflicts with a resource ID
			if resourceIDs.Has(iterName) {
				return fmt.Errorf("resource %q: forEach iterator name %q conflicts with resource ID", resourceID, iterName)
			}

			// Check for duplicate iterator names within the same resource
			if seenIterators.Has(iterName) {
				return fmt.Errorf("resource %q: duplicate forEach iterator name %q", resourceID, iterName)
			}
			seenIterators.Insert(iterName)
		}
	}

	return nil
}

// validateKubernetesObjectStructure checks if the given object is a Kubernetes object.
// This is done by checking if the object has the following fields:
// - apiVersion
// - kind
// - metadata
func validateKubernetesObjectStructure(obj map[string]any) error {
	apiVersion, exists := obj["apiVersion"]
	if !exists {
		return fmt.Errorf("apiVersion field not found")
	}
	_, isString := apiVersion.(string)
	if !isString {
		return fmt.Errorf("apiVersion field is not a string")
	}

	kind, exists := obj["kind"]
	if !exists {
		return fmt.Errorf("kind field not found")
	}
	_, isString = kind.(string)
	if !isString {
		return fmt.Errorf("kind field is not a string")
	}

	metadata, exists := obj["metadata"]
	if !exists {
		return fmt.Errorf("metadata field not found")
	}
	_, isMap := metadata.(map[string]any)
	if !isMap {
		return fmt.Errorf("metadata field is not a map")
	}

	return nil
}

// validateKubernetesVersion checks if the given version is a valid Kubernetes
// version. e.g v1, v1alpha1, v1beta1..
func validateKubernetesVersion(version string) error {
	if !kubernetesVersionRegex.MatchString(version) {
		return fmt.Errorf("version %s is not a valid Kubernetes version", version)
	}
	return nil
}

// validateCombinableResourceFields checks that certain fields in a resource
// are not used together in an invalid combination, and that required fields are present.
func validateCombinableResourceFields(id string, hasTemplate, hasExternalRef bool, forEachLen int) error {
	if !hasTemplate && !hasExternalRef {
		return fmt.Errorf("resource %q: exactly one of template or externalRef must be provided", id)
	}
	if hasExternalRef && hasTemplate {
		return fmt.Errorf("resource %q: cannot use externalRef with template", id)
	}
	if hasExternalRef && forEachLen > 0 {
		return fmt.Errorf("resource %q: cannot use externalRef with forEach", id)
	}
	return nil
}

func validateExternalRefMetadata(metadata v1alpha1.ExternalRefMetadata) error {
	if metadata.HasName() == metadata.HasSelector() {
		return fmt.Errorf("exactly one of name or selector must be provided")
	}

	if !metadata.HasSelector() {
		return nil
	}

	return validateSelector(metadata.Selector.Raw)
}

// validateSelector validates the structure of externalRef.metadata.selector.
// The field is schemaless, so it may hold either a literal LabelSelector or a
// CEL expression that resolves to one.
func validateSelector(raw []byte) error {
	var decoded interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Errorf("invalid selector: %w", err)
	}

	switch selector := decoded.(type) {
	case string:
		standalone, err := parser.IsStandaloneExpression(selector)
		if err != nil {
			return fmt.Errorf("invalid selector expression: %w", err)
		}
		if !standalone {
			return errSelectorShape
		}
		return nil
	case map[string]interface{}:
		expressions, err := parser.New(schema.NewCache()).
			ParseResourceAtPath(selector, &schema.LabelSelectorSchema, "metadata.selector")
		if err != nil {
			return err
		}
		if len(expressions) > 0 {
			// Some values are only known at instance reconcile time, so label
			// syntax and operator validity are left to LabelSelectorAsSelector
			// when the collection is listed.
			return nil
		}
		return validateLiteralSelector(raw)
	default:
		return errSelectorShape
	}
}

// validateLiteralSelector applies Kubernetes' own LabelSelector validation to a
// selector that holds no CEL expressions. Everything it checks — operator
// validity, values matching the operator, label key and value syntax — is known
// statically, so it is reported when the RGD is admitted rather than deferred to
// LabelSelectorAsSelector at list time.
func validateLiteralSelector(raw []byte) error {
	var selector metav1.LabelSelector
	if err := json.Unmarshal(raw, &selector); err != nil {
		return fmt.Errorf("invalid selector object: %w", err)
	}

	errs := metav1validation.ValidateLabelSelector(
		&selector,
		metav1validation.LabelSelectorValidationOptions{},
		field.NewPath("metadata", "selector"),
	)
	if len(errs) != 0 {
		return fmt.Errorf("invalid label selector: %v", errs)
	}

	return nil
}

var errSelectorShape = fmt.Errorf(
	"selector must be a Kubernetes LabelSelector object or a CEL expression that resolves to one")

// validateTemplateConstraints enforces template-level constraints before parsing expressions.
// Keep this small and focused on invariants that must hold regardless of CEL.
func validateTemplateConstraints(
	id string,
	isExternalCollection bool,
	resourceObject map[string]any,
	resourceNamespaced bool,
	instanceNamespaced bool,
) error {
	namespaceValue, found, err := unstructured.NestedFieldNoCopy(resourceObject, "metadata", "namespace")
	if err != nil {
		return fmt.Errorf("resource %q has invalid metadata.namespace: %w", id, err)
	}

	if !resourceNamespaced {
		if found {
			return fmt.Errorf("resource %q is cluster-scoped and must not set metadata.namespace", id)
		}
	}
	if resourceNamespaced && !instanceNamespaced {
		// External collection refs (selector-based) are allowed to omit namespace
		// on cluster-scoped instances — this means "list across all namespaces".
		if !isExternalCollection {
			if !found {
				return fmt.Errorf("resource %q is namespaced and must set metadata.namespace when the instance CRD is cluster-scoped", id)
			}
			if ns, ok := namespaceValue.(string); !ok || strings.TrimSpace(ns) == "" {
				return fmt.Errorf("resource %q is namespaced and must set metadata.namespace when the instance CRD is cluster-scoped", id)
			}
		}
	}

	// Validate that users don't set KRO-owned metadata.
	if err := validateNoKROOwnedLabels(id, resourceObject); err != nil {
		return err
	}
	if err := validateNoKROOwnedAnnotations(id, resourceObject); err != nil {
		return err
	}

	return nil
}

// validateIdentityFields checks that omit() is not used on resource identity
// fields. These fields are special to kro's ownership model — omitting them
// silently breaks SSA ownership and resource tracking, unlike schema-required
// fields which fail loudly on server-side apply.
//
// Identity fields:
//   - metadata.name: always required for any Kubernetes resource
//   - metadata.namespace: required when the instance is cluster-scoped and the
//     resource itself is namespaced (no instance namespace to inherit)
func validateIdentityFields(nodes map[string]*Node, inspector *ast.Inspector, isInstanceNamespaced bool) error {
	for _, node := range nodes {
		for _, v := range node.Variables {
			if !isRequiredIdentityField(v.Path, node.Meta.Namespaced, isInstanceNamespaced) {
				continue
			}
			result, err := inspector.Inspect(v.Expression.Original)
			if err != nil {
				return fmt.Errorf("resource %q: failed to inspect expression at path %q: %w", node.Meta.ID, v.Path, err)
			}
			if result.UsesOmit() {
				return fmt.Errorf("resource %q: omit() cannot be used at path %q — the field is required and must resolve to a concrete value", node.Meta.ID, v.Path)
			}
		}
	}
	return nil
}

// isRequiredIdentityField reports whether the given field path is a resource
// identity field that must resolve to a concrete value. metadata.name is always
// required. metadata.namespace is required only when the resource is namespaced
// and the instance is cluster-scoped (no namespace to inherit).
func isRequiredIdentityField(path string, resourceNamespaced, instanceNamespaced bool) bool {
	switch path {
	case MetadataNamePath:
		return true
	case MetadataNamespacePath:
		return resourceNamespaced && !instanceNamespaced
	default:
		return false
	}
}

// validateNoKROOwnedLabels enforces that resource templates do not define
// labels in either controller-owned namespace.
func validateNoKROOwnedLabels(resourceID string, resourceObject map[string]any) error {
	labelsRaw, found, err := unstructured.NestedFieldNoCopy(resourceObject, "metadata", "labels")
	if err != nil || !found {
		return nil
	}

	labelsMap, ok := labelsRaw.(map[string]any)
	if !ok {
		return nil
	}

	for key := range labelsMap {
		for _, prefix := range []string{metadata.KROPrefix, metadata.InternalKROPrefix} {
			if strings.HasPrefix(key, prefix) {
				return fmt.Errorf("invalid label for resource %q. labels with prefix %q are reserved for internal use", resourceID, prefix)
			}
		}
	}

	return nil
}

// validateNoKROOwnedAnnotations prevents resource templates from overriding
// annotations used as persisted controller state.
func validateNoKROOwnedAnnotations(resourceID string, resourceObject map[string]any) error {
	annotationsRaw, found, err := unstructured.NestedFieldNoCopy(resourceObject, "metadata", "annotations")
	if err != nil || !found {
		return nil
	}

	annotationsMap, ok := annotationsRaw.(map[string]any)
	if !ok {
		return nil
	}

	for key := range annotationsMap {
		for _, prefix := range []string{metadata.KROPrefix, metadata.InternalKROPrefix} {
			if strings.HasPrefix(key, prefix) {
				return fmt.Errorf(
					"invalid annotation for resource %q. annotations with prefix %q are reserved for internal use",
					resourceID,
					prefix,
				)
			}
		}
	}

	return nil
}
