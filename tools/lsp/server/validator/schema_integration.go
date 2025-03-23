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

// validates a Kro document with position information
func ValidateKroFileWithPositions(doc *YAMLDocument) []ValidationError {
	var errors []ValidationError

	if err := validateAPIVersionAndKindWithPosition(doc); err != nil {
		errors = append(errors, err...)
	}

	if err := validateMetadataWithPosition(doc); err != nil {
		errors = append(errors, err...)
	}

	if _, specExists := doc.Positions["spec"]; !specExists {
		line := 2
		if metadataField, exists := doc.Positions["metadata"]; exists {
			line = metadataField.EndLine + 1
		}

		errors = append(errors, ValidationError{
			Message: "spec section is required",
			Line:    line,
			Column:  0,
			EndLine: line,
			EndCol:  4,
		})
		return errors
	}

	schemaTypeErrors := ValidateSchemaTypes(doc)
	errors = append(errors, schemaTypeErrors...)

	if len(errors) == 0 && doc.Data != nil {
		rgd, err := mapToResourceGraphDefinition(doc.Data)
		if err != nil {
			errors = append(errors, ValidationError{
				Message: fmt.Sprintf("Invalid format: %s", err.Error()),
				Line:    0,
				Column:  0,
				EndLine: 0,
				EndCol:  10,
			})
		} else if rgd != nil {
			validationErrors := validateResourceGraphDefinitionWithPositions(rgd, doc)
			errors = append(errors, validationErrors...)
		}
	}

	return errors
}

// validates apiVersion and kind fields with position info
func validateAPIVersionAndKindWithPosition(doc *YAMLDocument) []ValidationError {
	var errors []ValidationError

	apiVersionField, apiVersionExists := doc.Positions["apiVersion"]
	if !apiVersionExists {
		errors = append(errors, ValidationError{
			Message: "apiVersion is required and must be the first field",
			Line:    0,
			Column:  0,
			EndLine: 0,
			EndCol:  10,
		})
	} else {
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

	kindField, kindExists := doc.Positions["kind"]
	if !kindExists {
		errors = append(errors, ValidationError{
			Message: "kind is required and must be the second field",
			Line:    1,
			Column:  0,
			EndLine: 1,
			EndCol:  4,
		})
	} else {
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

// validates metadata section with position info
func validateMetadataWithPosition(doc *YAMLDocument) []ValidationError {
	var errors []ValidationError

	metadataField, metadataExists := doc.Positions["metadata"]
	if !metadataExists {
		kindLine := 1
		if kindField, exists := doc.Positions["kind"]; exists {
			kindLine = kindField.Line
		}

		errors = append(errors, ValidationError{
			Message: "metadata section is required",
			Line:    kindLine + 1,
			Column:  0,
			EndLine: kindLine + 1,
			EndCol:  8,
		})
		return errors
	}

	metadataValue, ok := metadataField.Value.(map[string]interface{})
	if !ok {
		errors = append(errors, ValidationError{
			Message: "metadata must be a mapping",
			Line:    metadataField.Line,
			Column:  0,
			EndLine: metadataField.Line,
			EndCol:  100,
		})
		return errors
	}

	nameField, nameExists := doc.NestedFields["metadata.name"]
	_, nameInMap := metadataValue["name"]

	if !nameExists && !nameInMap {
		metadataLine := metadataField.Line

		errors = append(errors, ValidationError{
			Message: "metadata.name is required and must be a non-empty string",
			Line:    metadataLine + 1,
			Column:  2,
			EndLine: metadataLine + 1,
			EndCol:  6,
		})
	} else if nameExists && (nameField.Value == nil || nameField.Value == "") {
		errors = append(errors, ValidationError{
			Message: "metadata.name must be a non-empty string",
			Line:    nameField.Line,
			Column:  0,
			EndLine: nameField.Line,
			EndCol:  100,
		})
	} else if nameInMap && !nameExists {
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

// converts a map to a ResourceGraphDefinition
func mapToResourceGraphDefinition(data map[string]interface{}) (*v1alpha1.ResourceGraphDefinition, error) {
	u := &unstructured.Unstructured{Object: data}
	rgd := &v1alpha1.ResourceGraphDefinition{}

	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, rgd)
	if err != nil {
		return nil, fmt.Errorf("error converting to ResourceGraphDefinition: %w", err)
	}

	return rgd, nil
}

// performs validation with position tracking
func validateResourceGraphDefinitionWithPositions(rgd *v1alpha1.ResourceGraphDefinition, doc *YAMLDocument) []ValidationError {
	var errors []ValidationError

	if rgd.Spec.Schema != nil {
		if rgd.Spec.Schema.Kind != "" {
			kindField, found := doc.NestedFields["spec.schema.kind"]
			if !found {
				kindField = findClosestField(doc, "spec.schema")
			}

			if len(rgd.Spec.Schema.Kind) > 0 && (rgd.Spec.Schema.Kind[0] < 'A' || rgd.Spec.Schema.Kind[0] > 'Z') {
				errors = append(errors, ValidationError{
					Message: fmt.Sprintf("kind '%s' is not a valid KRO kind name: must be UpperCamelCase", rgd.Spec.Schema.Kind),
					Line:    kindField.Line,
					Column:  kindField.Column,
					EndLine: kindField.EndLine,
					EndCol:  kindField.EndCol,
				})
			}
		} else {
			schemaField := findClosestField(doc, "spec.schema")
			errors = append(errors, ValidationError{
				Message: "spec.schema.kind is required",
				Line:    schemaField.Line,
				Column:  schemaField.Column,
				EndLine: schemaField.EndLine,
				EndCol:  schemaField.EndCol,
			})
		}

		if rgd.Spec.Schema.APIVersion != "" {
			apiVersionField, found := doc.NestedFields["spec.schema.apiVersion"]
			if !found {
				apiVersionField = findClosestField(doc, "spec.schema")
			}

			if !strings.HasPrefix(rgd.Spec.Schema.APIVersion, "v") {
				errors = append(errors, ValidationError{
					Message: fmt.Sprintf("apiVersion '%s' is not a valid Kubernetes version, must start with 'v'", rgd.Spec.Schema.APIVersion),
					Line:    apiVersionField.Line,
					Column:  apiVersionField.Column,
					EndLine: apiVersionField.EndLine,
					EndCol:  apiVersionField.EndCol,
				})
			}
		}
	}

	if rgd.Spec.Resources != nil && len(rgd.Spec.Resources) > 0 {
		seen := make(map[string]struct{})

		for i, resource := range rgd.Spec.Resources {
			resourcePath := fmt.Sprintf("spec.resources[%d]", i)
			resourceIDPath := fmt.Sprintf("spec.resources[%d].id", i)
			resourceTemplatePath := fmt.Sprintf("spec.resources[%d].template", i)

			resourceIDField, idFound := doc.NestedFields[resourceIDPath]
			resourceField := findClosestField(doc, resourcePath)

			if len(resource.ID) > 0 && (resource.ID[0] < 'a' || resource.ID[0] > 'z') {
				if idFound {
					errors = append(errors, ValidationError{
						Message: fmt.Sprintf("id '%s' is not a valid KRO resource id: must be lower camelCase", resource.ID),
						Line:    resourceIDField.Line,
						Column:  resourceIDField.Column,
						EndLine: resourceIDField.EndLine,
						EndCol:  resourceIDField.EndCol,
					})
				} else {
					errors = append(errors, ValidationError{
						Message: fmt.Sprintf("id '%s' is not a valid KRO resource id: must be lower camelCase", resource.ID),
						Line:    resourceField.Line,
						Column:  resourceField.Column,
						EndLine: resourceField.EndLine,
						EndCol:  resourceField.EndCol,
					})
				}
			}

			if _, ok := seen[resource.ID]; ok {
				if idFound {
					errors = append(errors, ValidationError{
						Message: fmt.Sprintf("found duplicate resource ID '%s'", resource.ID),
						Line:    resourceIDField.Line,
						Column:  resourceIDField.Column,
						EndLine: resourceIDField.EndLine,
						EndCol:  resourceIDField.EndCol,
					})
				} else {
					errors = append(errors, ValidationError{
						Message: fmt.Sprintf("found duplicate resource ID '%s'", resource.ID),
						Line:    resourceField.Line,
						Column:  resourceField.Column,
						EndLine: resourceField.EndLine,
						EndCol:  resourceField.EndCol,
					})
				}
			}
			seen[resource.ID] = struct{}{}

			if resource.Template.Raw != nil {
				var obj map[string]interface{}
				if err := json.Unmarshal(resource.Template.Raw, &obj); err != nil {
					templateField, found := doc.NestedFields[resourceTemplatePath]
					if !found {
						templateField = resourceField
					}

					errors = append(errors, ValidationError{
						Message: fmt.Sprintf("invalid template for resource '%s': %s", resource.ID, err.Error()),
						Line:    templateField.Line,
						Column:  templateField.Column,
						EndLine: templateField.EndLine,
						EndCol:  templateField.EndCol,
					})
					continue
				}

				if err := validateKubernetesObjectStructure(obj); err != nil {
					templateField, found := doc.NestedFields[resourceTemplatePath]
					if !found {
						templateField = resourceField
					}

					errors = append(errors, ValidationError{
						Message: fmt.Sprintf("invalid Kubernetes object in resource '%s': %s", resource.ID, err.Error()),
						Line:    templateField.Line,
						Column:  templateField.Column,
						EndLine: templateField.EndLine,
						EndCol:  templateField.EndCol,
					})
				}
			}
		}
	}

	return errors
}

// finds the closest parent field if the exact field is not found
func findClosestField(doc *YAMLDocument, path string) YAMLField {
	if field, found := doc.NestedFields[path]; found {
		return field
	}

	parts := strings.Split(path, ".")
	for i := len(parts) - 1; i > 0; i-- {
		parentPath := strings.Join(parts[:i], ".")
		if field, found := doc.NestedFields[parentPath]; found {
			return field
		}
	}

	if field, found := doc.Positions["spec"]; found {
		return field
	}

	return YAMLField{
		Line:    0,
		Column:  0,
		EndLine: 0,
		EndCol:  10,
	}
}

// checks if the given object is a valid Kubernetes object
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
