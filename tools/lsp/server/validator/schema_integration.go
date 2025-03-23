package validator

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kro-run/kro/api/v1alpha1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// ValidationError stores error information with line position
type ValidationError struct {
	Message string
	Line    int
	Column  int
	EndLine int
	EndCol  int
}

func (e ValidationError) Error() string {
	return e.Message
}

// ValidateKroFile validates a Kro resource definition file using the main Kro codebase validators
func ValidateKroFile(data map[string]interface{}) []error {
	var errors []error

	// 1. Basic structure validation
	if err := validateBasicStructure(data); err != nil {
		errors = append(errors, err...)
		return errors // Return early if basic structure validation fails
	}

	// // 2. Convert to ResourceGraphDefinition
	rgd, err := mapToResourceGraphDefinition(data)
	if err != nil {
		errors = append(errors, fmt.Errorf("failed to convert to ResourceGraphDefinition: %w", err))
		return errors
	}

	// // 3. Validate using custom validation logic
	if err := validateResourceGraphDefinition(rgd); err != nil {
		errors = append(errors, err)
	}

	return errors
}

// ValidateKroFileWithPositions validates a Kro document with position information
func ValidateKroFileWithPositions(doc *YAMLDocument) []ValidationError {
	var errors []ValidationError

	// 1. Validate apiVersion and kind
	if err := validateAPIVersionAndKindWithPosition(doc); err != nil {
		errors = append(errors, err...)
	}

	// 2. Validate metadata
	if err := validateMetadataWithPosition(doc); err != nil {
		errors = append(errors, err...)
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
	// if err := validateMetadata(data); err != nil {
	// 	errors = append(errors, err)
	// }

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

// validateAPIVersionAndKindWithPosition validates apiVersion and kind fields with position info
func validateAPIVersionAndKindWithPosition(doc *YAMLDocument) []ValidationError {
	var errors []ValidationError

	// Check if apiVersion exists
	apiVersionField, apiVersionExists := doc.Positions["apiVersion"]
	if !apiVersionExists {
		// Since apiVersion is missing, create error at start of document
		errors = append(errors, ValidationError{
			Message: "apiVersion is required and must be the first field",
			Line:    0,
			Column:  0,
			EndLine: 0,
			EndCol:  10,
		})
	} else {
		// Check apiVersion value
		apiVersionValue, ok := apiVersionField.Value.(string)
		if !ok || !strings.HasPrefix(apiVersionValue, "kro.run/v1alpha") {
			errors = append(errors, ValidationError{
				Message: fmt.Sprintf("apiVersion must be 'kro.run/v1alpha1', got '%v'", apiVersionField.Value),
				Line:    apiVersionField.Line,
				Column:  apiVersionField.Column,
				EndLine: apiVersionField.EndLine,
				EndCol:  apiVersionField.EndCol,
			})
		}

		// Check if apiVersion is the first field (line 0 or 1 in the document)
		if apiVersionField.Line > 1 {
			errors = append(errors, ValidationError{
				Message: "apiVersion must be the first field in the document",
				Line:    apiVersionField.Line,
				Column:  apiVersionField.Column,
				EndLine: apiVersionField.EndLine,
				EndCol:  apiVersionField.EndCol,
			})
		}
	}

	// Check if kind exists
	kindField, kindExists := doc.Positions["kind"]
	if !kindExists {
		// Since kind is missing, create error at start of document
		errors = append(errors, ValidationError{
			Message: "kind is required and must be the second field",
			Line:    1, // Assume it should be on the second line
			Column:  0,
			EndLine: 1,
			EndCol:  4,
		})
	} else {
		// Check kind value
		kindValue, ok := kindField.Value.(string)
		if !ok || kindValue != "ResourceGraphDefinition" {
			errors = append(errors, ValidationError{
				Message: fmt.Sprintf("kind must be 'ResourceGraphDefinition', got '%v'", kindField.Value),
				Line:    kindField.Line,
				Column:  kindField.Column,
				EndLine: kindField.EndLine,
				EndCol:  kindField.EndCol,
			})
		}

		// Check if kind is the second field
		if kindField.Line != 1 && apiVersionExists && kindField.Line != apiVersionField.Line+1 {
			errors = append(errors, ValidationError{
				Message: "kind must be the second field after apiVersion",
				Line:    kindField.Line,
				Column:  kindField.Column,
				EndLine: kindField.EndLine,
				EndCol:  kindField.EndCol,
			})
		}
	}

	return errors
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

// validateMetadataWithPosition validates metadata section with position info
func validateMetadataWithPosition(doc *YAMLDocument) []ValidationError {
	var errors []ValidationError

	// Check if metadata exists
	metadataField, metadataExists := doc.Positions["metadata"]
	if !metadataExists {
		// Metadata is missing, report error at line after kind
		kindLine := 1 // Default position if we don't have kind data
		if kindField, exists := doc.Positions["kind"]; exists {
			kindLine = kindField.Line
		}

		errors = append(errors, ValidationError{
			Message: "metadata section is required",
			Line:    kindLine + 1, // Assume it should be right after kind
			Column:  0,
			EndLine: kindLine + 1,
			EndCol:  8, // Length of "metadata"
		})
		return errors // Return early since we can't check metadata.name
	}

	// Extract metadata value
	metadataValue, ok := metadataField.Value.(map[string]interface{})
	if !ok {
		// Metadata is not a map, report error
		errors = append(errors, ValidationError{
			Message: "metadata must be a mapping",
			Line:    metadataField.Line,
			Column:  0,
			EndLine: metadataField.Line,
			EndCol:  100,
		})
		return errors // Return early since we can't check metadata.name
	}

	// Check if name exists in metadata
	nameField, nameExists := doc.NestedFields["metadata.name"]
	_, nameInMap := metadataValue["name"]

	if !nameExists && !nameInMap {
		// Name is missing completely, determine where it should be
		metadataLine := metadataField.Line

		errors = append(errors, ValidationError{
			Message: "metadata.name is required and must be a non-empty string",
			Line:    metadataLine + 1, // Assume name should be on next line after metadata
			Column:  2,                // Assume indentation of 2 spaces
			EndLine: metadataLine + 1,
			EndCol:  6, // Length of "name"
		})
	} else if nameExists && (nameField.Value == nil || nameField.Value == "") {
		// Name exists but is empty
		errors = append(errors, ValidationError{
			Message: "metadata.name must be a non-empty string",
			Line:    nameField.Line,
			Column:  0,
			EndLine: nameField.Line,
			EndCol:  100,
		})
	} else if nameInMap && !nameExists {
		// Name exists in the map but not in nested fields
		// This is a fallback case if our nested field tracking didn't work correctly
		errors = append(errors, ValidationError{
			Message: "metadata.name must be a valid string",
			Line:    metadataField.Line + 1,
			Column:  2,
			EndLine: metadataField.Line + 1,
			EndCol:  6,
		})
	}

	return errors
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
