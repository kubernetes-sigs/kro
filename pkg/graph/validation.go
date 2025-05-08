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

package graph

import (
	"fmt"
	"regexp"

	"github.com/kro-run/kro/api/v1alpha1"
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

	// reservedKeyWords is a list of reserved words in kro.
	reservedKeyWords = []string{
		"apiVersion",
		"context",
		"dependency",
		"dependencies",
		"externalRef",
		"externalReference",
		"externalRefs",
		"externalReferences",
		"graph",
		"instance",
		"kind",
		"metadata",
		"namespace",
		"object",
		"resource",
		"resourcegraphdefinition",
		"resources",
		"runtime",
		"serviceAccountName",
		"schema",
		"spec",
		"status",
		"kro",
		"variables",
		"vars",
		"version",
	}
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
	for _, w := range reservedKeyWords {
		if w == word {
			return true
		}
	}
	return false
}

// validateResourceGraphDefinitionNamingConventions validates the naming conventions of
// the given resource graph definition.
func validateResourceGraphDefinitionNamingConventions(rgd *v1alpha1.ResourceGraphDefinition) error {
	if !isValidKindName(rgd.Spec.Schema.Kind) {
		return fmt.Errorf("%s: kind '%s' is not a valid KRO kind name: must be UpperCamelCase", ErrNamingConvention, rgd.Spec.Schema.Kind)
	}
	err := validateResourceIDs(rgd)
	if err != nil {
		return fmt.Errorf("%s: %w", ErrNamingConvention, err)
	}
	return nil
}

// validateResource performs basic validation on a given resourcegraphdefinition.
// It checks that there are no duplicate resource ids and that the
// resource ids are conformant to the KRO naming convention.
//
// The KRO naming convention is as follows:
// - The id should start with a lowercase letter.
// - The id should only contain alphanumeric characters.
// - Does not contain any special characters, underscores, or hyphens.
func validateResourceIDs(rgd *v1alpha1.ResourceGraphDefinition) error {
	seen := make(map[string]struct{})
	for _, res := range rgd.Spec.Resources {
		if isKROReservedWord(res.ID) {
			return fmt.Errorf("id %s is a reserved keyword in KRO", res.ID)
		}

		if !isValidResourceID(res.ID) {
			return fmt.Errorf("id %s is not a valid KRO resource id: must be lower camelCase", res.ID)
		}

		if _, ok := seen[res.ID]; ok {
			return fmt.Errorf("found duplicate resource IDs %s", res.ID)
		}
		seen[res.ID] = struct{}{}
	}
	return nil
}

// validateKubernetesObjectStructure checks if the given object is a Kubernetes object.
// This is done by checking if the object has the following fields:
// - apiVersion
// - kind
// - metadata
func validateKubernetesObjectStructure(obj map[string]interface{}) error {
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
	_, isMap := metadata.(map[string]interface{})
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
