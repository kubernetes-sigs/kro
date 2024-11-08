// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package graph

import (
	"fmt"
	"regexp"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/awslabs/symphony/api/v1alpha1"
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

	// reservedKeyWords is a list of reserved words in Symphony.
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
		"resourcegroup",
		"resources",
		"runtime",
		"serviceAccountName",
		"spec",
		"status",
		"symphony",
		"variables",
		"vars",
		"version",
	}
)

// isValidResourceName checks if the given name is a valid Symphony resource name (loawercase)
func isValidResourceName(name string) bool {
	return lowerCamelCaseRegex.MatchString(name)
}

// isValidKindName checks if the given name is a valid Symphony kind name (uppercase)
func isValidKindName(name string) bool {
	return upperCamelCaseRegex.MatchString(name)
}

// isSymphonyReservedWord checks if the given word is a reserved word in Symphony.
func isSymphonyReservedWord(word string) bool {
	for _, w := range reservedKeyWords {
		if w == word {
			return true
		}
	}
	return false
}

// validateResourceGroupNamingConventions validates the naming conventions of
// the given resource group.
func validateResourceGroupNamingConventions(rg *v1alpha1.ResourceGroup) error {
	if !isValidKindName(rg.Spec.Schema.Kind) {
		return fmt.Errorf("%s: kind '%s' is not a valid Symphony kind name: must be UpperCamelCase", ErrNamingConvention, rg.Spec.Schema.Kind)
	}
	err := validateResourceNames(rg)
	if err != nil {
		return fmt.Errorf("%s: %w", ErrNamingConvention, err)
	}
	return nil
}

// validateResource performs basic validation on a given resourcegroup.
// It checks that there are no duplicate resource names and that the
// resource names are conformant to the Symphony naming convention.
//
// The Symphony naming convention is as follows:
// - The name should start with a lowercase letter.
// - The name should only contain alphanumeric characters.
// - does not contain any special characters, underscores, or hyphens.
func validateResourceNames(rg *v1alpha1.ResourceGroup) error {
	seen := make(map[string]struct{})
	for _, res := range rg.Spec.Resources {
		if isSymphonyReservedWord(res.Name) {
			return fmt.Errorf("name %s is a reserved keyword in Symphony", res.Name)
		}

		if !isValidResourceName(res.Name) {
			return fmt.Errorf("name %s is not a valid Symphony resource name: must be lower camelCase", res.Name)
		}

		if _, ok := seen[res.Name]; ok {
			return fmt.Errorf("found duplicate resource name %s", res.Name)
		}
		seen[res.Name] = struct{}{}
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

	groupVersion, err := schema.ParseGroupVersion(apiVersion.(string))
	if err != nil {
		return fmt.Errorf("apiVersion field is not a valid Kubernetes group version: %w", err)
	}
	if groupVersion.Version != "" {
		// Only validate the version if it is not empty. Empty version is allowed.
		if err := validateKubernetesVersion(groupVersion.Version); err != nil {
			return fmt.Errorf("apiVersion field does not have a valid version: %w", err)
		}
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
