package validator

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kro-run/kro/api/v1alpha1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// ValidateKroFile validates a Kro resource definition file using the main Kro codebase validators
func ValidateKroFile(data map[string]interface{}) []error {
	var errors []error

	// 1. Basic structure validation
	if err := validateBasicStructure(data); err != nil {
		errors = append(errors, err...)
		return errors // Return early if basic structure validation fails
	}

	// 2. Convert to ResourceGraphDefinition
	rgd, err := mapToResourceGraphDefinition(data)
	if err != nil {
		errors = append(errors, fmt.Errorf("failed to convert to ResourceGraphDefinition: %w", err))
		return errors
	}

	// 3. Validate using custom validation logic
	if err := validateResourceGraphDefinition(rgd); err != nil {
		errors = append(errors, err)
	}

	return errors
}

// validateBasicStructure checks required fields in the Kro resource definition
func validateBasicStructure(data map[string]interface{}) []error {
	var errors []error

	// 1. Validate apiVersion and kind
	if err := validateAPIVersionAndKind(data); err != nil {
		errors = append(errors, err)
	}

	// 2. Validate metadata
	if err := validateMetadata(data); err != nil {
		errors = append(errors, err)
	}

	// 3. Validate that spec exists
	_, ok := data["spec"].(map[string]interface{})
	if !ok {
		errors = append(errors, fmt.Errorf("spec section is required"))
	}

	return errors
}

// validateAPIVersionAndKind validates the apiVersion and kind fields
func validateAPIVersionAndKind(data map[string]interface{}) error {
	// Check apiVersion
	apiVersion, ok := data["apiVersion"].(string)
	if !ok {
		return fmt.Errorf("apiVersion is required")
	}
	if !strings.HasPrefix(apiVersion, "kro.run/v1alpha") {
		return fmt.Errorf("apiVersion must start with 'kro.run/v1alpha', got '%s'", apiVersion)
	}

	// Check kind
	kind, ok := data["kind"].(string)
	if !ok {
		return fmt.Errorf("kind is required")
	}
	if kind != "ResourceGraphDefinition" {
		return fmt.Errorf("kind must be 'ResourceGraphDefinition', got '%s'", kind)
	}

	return nil
}

// validateMetadata validates the metadata section
func validateMetadata(data map[string]interface{}) error {
	metadata, ok := data["metadata"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("metadata section is required")
	}

	name, ok := metadata["name"].(string)
	if !ok || name == "" {
		return fmt.Errorf("metadata.name is required and must be a non-empty string")
	}

	return nil
}

// mapToResourceGraphDefinition converts a map to a ResourceGraphDefinition
func mapToResourceGraphDefinition(data map[string]interface{}) (*v1alpha1.ResourceGraphDefinition, error) {
	// Convert map to unstructured
	u := &unstructured.Unstructured{Object: data}

	// Create a new ResourceGraphDefinition
	rgd := &v1alpha1.ResourceGraphDefinition{}

	// Convert unstructured to ResourceGraphDefinition
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, rgd)
	if err != nil {
		return nil, fmt.Errorf("error converting to ResourceGraphDefinition: %w", err)
	}

	return rgd, nil
}

// validateResourceGraphDefinition implements custom validation for ResourceGraphDefinition
func validateResourceGraphDefinition(rgd *v1alpha1.ResourceGraphDefinition) error {
	// Validate naming conventions
	if err := validateNamingConventions(rgd); err != nil {
		return fmt.Errorf("naming convention validation failed: %w", err)
	}

	// Validate resources
	if err := validateResourceIDs(rgd); err != nil {
		return fmt.Errorf("resource validation failed: %w", err)
	}

	// Validate Kubernetes resources in templates
	for _, resource := range rgd.Spec.Resources {
		if resource.Template.Raw != nil {
			var obj map[string]interface{}
			if err := json.Unmarshal(resource.Template.Raw, &obj); err != nil {
				return fmt.Errorf("invalid template for resource '%s': %w", resource.ID, err)
			}

			if err := validateKubernetesObjectStructure(obj); err != nil {
				return fmt.Errorf("invalid Kubernetes object in resource '%s': %w", resource.ID, err)
			}
		}
	}

	// Validate schema
	if rgd.Spec.Schema != nil {
		if err := validateKubernetesVersion(rgd.Spec.Schema.APIVersion); err != nil {
			return fmt.Errorf("invalid schema apiVersion: %w", err)
		}
	}

	return nil
}

// validateNamingConventions checks if kind name follows UpperCamelCase
func validateNamingConventions(rgd *v1alpha1.ResourceGraphDefinition) error {
	if rgd.Spec.Schema == nil || rgd.Spec.Schema.Kind == "" {
		return fmt.Errorf("spec.schema.kind is required")
	}

	// Check if first letter is uppercase
	if len(rgd.Spec.Schema.Kind) > 0 && (rgd.Spec.Schema.Kind[0] < 'A' || rgd.Spec.Schema.Kind[0] > 'Z') {
		return fmt.Errorf("kind '%s' is not a valid KRO kind name: must be UpperCamelCase", rgd.Spec.Schema.Kind)
	}

	return nil
}

// validateResourceIDs checks for duplicate resource IDs and valid naming
func validateResourceIDs(rgd *v1alpha1.ResourceGraphDefinition) error {
	seen := make(map[string]struct{})
	for _, res := range rgd.Spec.Resources {
		// Check if ID is valid (starts with lowercase)
		if len(res.ID) > 0 && (res.ID[0] < 'a' || res.ID[0] > 'z') {
			return fmt.Errorf("id %s is not a valid KRO resource id: must be lower camelCase", res.ID)
		}

		if _, ok := seen[res.ID]; ok {
			return fmt.Errorf("found duplicate resource IDs %s", res.ID)
		}
		seen[res.ID] = struct{}{}
	}
	return nil
}

// validateKubernetesObjectStructure checks if the given object is a valid Kubernetes object
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

// validateKubernetesVersion checks if the given version is a valid Kubernetes version
func validateKubernetesVersion(version string) error {
	if !strings.HasPrefix(version, "v") {
		return fmt.Errorf("version %s is not a valid Kubernetes version, must start with 'v'", version)
	}
	return nil
}
