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

package resourcegroup

import (
	"fmt"
	"regexp"

	"github.com/aws-controllers-k8s/symphony/api/v1alpha1"
)

var (
	regex = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)
	// reservedKeyWords is a list of reserved words in Symphony.
	reservedKeyWords = []string{
		"apiVersion",
		"externalRef",
		"externalReference",
		"externalRefs",
		"externalReferences",
		"dependency",
		"dependencies",
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
		"variables",
	}
)

func isValidResourceName(name string) bool {
	return regex.MatchString(name)
}

func isSymphonyReservedWord(word string) bool {
	for _, w := range reservedKeyWords {
		if w == word {
			return true
		}
	}
	return false
}

// validateResource performs basic validation on a given resourcegroup.
// It checks that there are no duplicate resource names and that the
// resource names are conformant to the Symphony naming convention.
//
// The Symphony naming convention is as follows:
// - The name should start with a lowercase letter.
// - The name should only contain alphanumeric characters.
// - does not contain any special characters, underscores, or hyphens.
func validateRGResourceNames(rg *v1alpha1.ResourceGroup) error {
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
	apiVersion, apiVersionExists := obj["apiVersion"]
	if !apiVersionExists {
		return fmt.Errorf("apiVersion field not found")
	}
	_, isString := apiVersion.(string)
	if !isString {
		return fmt.Errorf("apiVersion field is not a string")
	}

	kind, kindExists := obj["kind"]
	if !kindExists {
		return fmt.Errorf("kind field not found")
	}
	_, isString = kind.(string)
	if !isString {
		return fmt.Errorf("kind field is not a string")
	}

	metadata, metadataExists := obj["metadata"]
	if !metadataExists {
		return fmt.Errorf("metadata field not found")
	}
	_, isMap := metadata.(map[string]interface{})
	if !isMap {
		return fmt.Errorf("metadata field is not a map")
	}

	return nil
}
