package validator

import (
	"fmt"
	"regexp"
	"strings"
)

// Valid base types for schema fields
var validBaseTypes = []string{
	"string",
	"integer",
	"boolean",
	"number",
	"object",
}

// Valid modifiers for schema fields
var validModifiers = []string{
	"required",
	"default",
	"description",
	"minimum",
	"maximum",
	"enum",
	"format",
	"pattern",
	"minLength",
	"maxLength",
	"minItems",
	"maxItems",
	"uniqueItems",
}

// Regular expressions for validating type syntax
var typePatterns = map[string]*regexp.Regexp{
	"string":  regexp.MustCompile(`^string(\s*\|\s*.+)?$`),
	"integer": regexp.MustCompile(`^integer(\s*\|\s*.+)?$`),
	"boolean": regexp.MustCompile(`^boolean(\s*\|\s*.+)?$`),
	"number":  regexp.MustCompile(`^number(\s*\|\s*.+)?$`),
	"object":  regexp.MustCompile(`^object(\s*\|\s*.+)?$`),
	"array":   regexp.MustCompile(`^\[\](.+)$`),
	"map":     regexp.MustCompile(`^map\[(.+)\](.+)$`),
}

// ValidateSchemaTypes validates the types used in schema definition
func ValidateSchemaTypes(doc *YAMLDocument) []ValidationError {
	var errors []ValidationError

	// Check if spec.schema exists
	if _, ok := doc.Data["spec"]; !ok {
		return errors // Return early if spec is missing
	}

	spec, ok := doc.Data["spec"].(map[string]interface{})
	if !ok {
		return errors // Return early if spec is not a map
	}

	// Check if schema exists in spec
	if _, ok := spec["schema"]; !ok {
		return errors // Return early if schema is missing
	}

	schema, ok := spec["schema"].(map[string]interface{})
	if !ok {
		return errors // Return early if schema is not a map
	}

	// Validate spec section of the schema
	if schemaSpec, ok := schema["spec"].(map[string]interface{}); ok {
		schemaFieldErrors := validateSchemaFields(schemaSpec, doc, "spec.schema.spec")
		errors = append(errors, schemaFieldErrors...)
	}

	// Validate status section of the schema if it exists
	if schemaStatus, ok := schema["status"].(map[string]interface{}); ok {
		statusFieldErrors := validateSchemaFields(schemaStatus, doc, "spec.schema.status")
		errors = append(errors, statusFieldErrors...)
	}

	return errors
}

// validateSchemaFields recursively validates all fields in a schema section
func validateSchemaFields(fields map[string]interface{}, doc *YAMLDocument, path string) []ValidationError {
	var errors []ValidationError

	for fieldName, fieldValue := range fields {
		fieldPath := fmt.Sprintf("%s.%s", path, fieldName)

		switch value := fieldValue.(type) {
		case string:
			// This is a type definition string
			if fieldErr := validateTypeString(value, fieldPath, doc); fieldErr != nil {
				errors = append(errors, *fieldErr)
			}
		case map[string]interface{}:
			// This is a nested structure - recursively validate
			nestedErrors := validateSchemaFields(value, doc, fieldPath)
			errors = append(errors, nestedErrors...)
		}
	}

	return errors
}

// validateTypeString validates a type definition string according to Kro schema rules
func validateTypeString(typeStr string, path string, doc *YAMLDocument) *ValidationError {
	// Find the position of this field in the document
	fieldPosition, found := doc.NestedFields[path]

	// If not found through direct path lookup, try a more flexible approach
	if !found {
		// We don't have position - this shouldn't happen with our improved tracking
		return &ValidationError{
			Message: fmt.Sprintf("Invalid type definition at %s: %s", path, typeStr),
			Line:    0, // Default to start of document
			Column:  0,
			EndLine: 0,
			EndCol:  10,
		}
	}

	// Skip validation for CEL expressions (used in status fields)
	if strings.HasPrefix(typeStr, "${") && strings.HasSuffix(typeStr, "}") {
		return nil // Valid CEL expression
	}

	// Check for array type syntax: []type
	if strings.HasPrefix(typeStr, "[]") {
		subType := strings.TrimPrefix(typeStr, "[]")
		if !isValidBaseType(subType) && !isValidComplexType(subType) {
			return &ValidationError{
				Message: fmt.Sprintf("Invalid array element type '%s' at %s", subType, path),
				Line:    fieldPosition.Line,
				Column:  fieldPosition.Column,
				EndLine: fieldPosition.EndLine,
				EndCol:  fieldPosition.EndCol,
			}
		}
		return nil
	}

	// Check for map type syntax: map[keyType]valueType
	if strings.HasPrefix(typeStr, "map[") {
		mapRegex := typePatterns["map"]
		if !mapRegex.MatchString(typeStr) {
			return &ValidationError{
				Message: fmt.Sprintf("Invalid map type syntax '%s' at %s", typeStr, path),
				Line:    fieldPosition.Line,
				Column:  fieldPosition.Column,
				EndLine: fieldPosition.EndLine,
				EndCol:  fieldPosition.EndCol,
			}
		}

		matches := mapRegex.FindStringSubmatch(typeStr)
		if len(matches) >= 3 {
			keyType := matches[1]
			valueType := matches[2]

			// Key type should be simple (usually string)
			if !isValidBaseType(keyType) {
				return &ValidationError{
					Message: fmt.Sprintf("Invalid map key type '%s' at %s", keyType, path),
					Line:    fieldPosition.Line,
					Column:  fieldPosition.Column,
					EndLine: fieldPosition.EndLine,
					EndCol:  fieldPosition.EndCol,
				}
			}

			// Value type can be more complex
			if !isValidBaseType(valueType) && !isValidComplexType(valueType) {
				return &ValidationError{
					Message: fmt.Sprintf("Invalid map value type '%s' at %s", valueType, path),
					Line:    fieldPosition.Line,
					Column:  fieldPosition.Column,
					EndLine: fieldPosition.EndLine,
					EndCol:  fieldPosition.EndCol,
				}
			}
		}
		return nil
	}

	// Check for base type with modifiers
	typeWithModifiers := strings.Split(typeStr, "|")
	baseType := strings.TrimSpace(typeWithModifiers[0])

	// Check if the base type is valid
	if !isValidBaseType(baseType) {
		return &ValidationError{
			Message: fmt.Sprintf("Invalid type '%s' at %s. Must be one of: string, integer, boolean, number, object", baseType, path),
			Line:    fieldPosition.Line,
			Column:  fieldPosition.Column,
			EndLine: fieldPosition.EndLine,
			EndCol:  fieldPosition.EndCol,
		}
	}

	// If there are modifiers, validate them
	if len(typeWithModifiers) > 1 {
		for i := 1; i < len(typeWithModifiers); i++ {
			modifier := strings.TrimSpace(typeWithModifiers[i])
			if err := validateModifier(modifier, baseType); err != nil {
				return &ValidationError{
					Message: fmt.Sprintf("Invalid modifier '%s' for type '%s' at %s: %s", modifier, baseType, path, err),
					Line:    fieldPosition.Line,
					Column:  fieldPosition.Column,
					EndLine: fieldPosition.EndLine,
					EndCol:  fieldPosition.EndCol,
				}
			}
		}
	}

	return nil
}

// isValidBaseType checks if a type is one of the valid base types
func isValidBaseType(typeName string) bool {
	for _, validType := range validBaseTypes {
		if strings.TrimSpace(typeName) == validType {
			return true
		}
	}
	return false
}

// isValidComplexType checks if a type is a valid complex type (array or map)
func isValidComplexType(typeName string) bool {
	// Check for array syntax
	if strings.HasPrefix(typeName, "[]") {
		subType := strings.TrimPrefix(typeName, "[]")
		return isValidBaseType(subType) || isValidComplexType(subType)
	}

	// Check for map syntax
	if strings.HasPrefix(typeName, "map[") {
		return typePatterns["map"].MatchString(typeName)
	}

	return false
}

// validateModifier validates a type modifier
func validateModifier(modifier string, baseType string) error {
	// Check if there are multiple modifiers in one expression
	if strings.Contains(modifier, " ") {
		// Split by space and validate each modifier separately
		subModifiers := strings.Split(modifier, " ")
		for _, subMod := range subModifiers {
			if subMod == "" {
				continue
			}
			if err := validateModifier(subMod, baseType); err != nil {
				return err
			}
		}
		return nil
	}

	// Extract the modifier name and value
	parts := strings.SplitN(modifier, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("modifier must be in format 'name=value'")
	}

	modifierName := strings.TrimSpace(parts[0])
	modifierValue := strings.TrimSpace(parts[1])

	// Check if the modifier name is valid
	isValid := false
	for _, validMod := range validModifiers {
		if modifierName == validMod {
			isValid = true
			break
		}
	}

	if !isValid {
		return fmt.Errorf("unknown modifier '%s'", modifierName)
	}

	// Validate the modifier value based on the modifier type and base type
	switch modifierName {
	case "required":
		if modifierValue != "true" && modifierValue != "false" {
			return fmt.Errorf("required modifier must be 'true' or 'false'")
		}
	case "default":
		// Default value validation would depend on the base type
		// This could be extended with more detailed validation
	case "description":
		// Any string value is valid for description
		// Check if it's properly quoted
		if !isValidQuotedString(modifierValue) && !isSimpleString(modifierValue) {
			return fmt.Errorf("description value should be a valid string")
		}
	case "minimum", "maximum":
		if baseType != "integer" && baseType != "number" {
			return fmt.Errorf("%s modifier can only be used with numeric types", modifierName)
		}
		// Could add numeric validation here
	case "minLength", "maxLength":
		if baseType != "string" {
			return fmt.Errorf("%s modifier can only be used with string type", modifierName)
		}
		// Could add numeric validation here
	case "minItems", "maxItems":
		// These should only be used with array types, but we can't check that here
		// as the array syntax is checked separately
	case "enum":
		// Enum should have comma-separated values
		if !strings.Contains(modifierValue, ",") && len(modifierValue) <= 2 {
			return fmt.Errorf("enum should have multiple values separated by commas")
		}
	}

	return nil
}

// isValidQuotedString checks if a string is properly quoted
func isValidQuotedString(s string) bool {
	if len(s) < 2 {
		return false
	}

	// Check for double quotes
	if s[0] == '"' && s[len(s)-1] == '"' {
		return true
	}

	// Check for single quotes
	if s[0] == '\'' && s[len(s)-1] == '\'' {
		return true
	}

	return false
}

// isSimpleString checks if a string is a valid unquoted identifier
func isSimpleString(s string) bool {
	if len(s) == 0 {
		return false
	}

	// Check if it's a simple identifier (no spaces or special chars needed)
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}

	return true
}
