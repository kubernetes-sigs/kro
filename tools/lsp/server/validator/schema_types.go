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

	if _, ok := doc.Data["spec"]; !ok {
		return errors
	}

	spec, ok := doc.Data["spec"].(map[string]interface{})
	if !ok {
		return errors
	}

	if _, ok := spec["schema"]; !ok {
		return errors
	}

	schema, ok := spec["schema"].(map[string]interface{})
	if !ok {
		return errors
	}

	if schemaSpec, ok := schema["spec"].(map[string]interface{}); ok {
		schemaFieldErrors := validateSchemaFields(schemaSpec, doc, "spec.schema.spec")
		errors = append(errors, schemaFieldErrors...)
	}

	if schemaStatus, ok := schema["status"].(map[string]interface{}); ok {
		statusFieldErrors := validateSchemaFields(schemaStatus, doc, "spec.schema.status")
		errors = append(errors, statusFieldErrors...)
	}

	return errors
}

// validates fields in a schema section
func validateSchemaFields(fields map[string]interface{}, doc *YAMLDocument, path string) []ValidationError {
	var errors []ValidationError

	for fieldName, fieldValue := range fields {
		fieldPath := fmt.Sprintf("%s.%s", path, fieldName)

		switch value := fieldValue.(type) {
		case string:
			if fieldErr := validateTypeString(value, fieldPath, doc); fieldErr != nil {
				errors = append(errors, *fieldErr)
			}
		case map[string]interface{}:
			nestedErrors := validateSchemaFields(value, doc, fieldPath)
			errors = append(errors, nestedErrors...)
		}
	}

	return errors
}

// validates a type definition and its modifiers
func validateTypeString(typeStr string, path string, doc *YAMLDocument) *ValidationError {
	fieldPosition := getFieldPosition(path, doc)

	// Skip validation for CEL expressions
	if strings.HasPrefix(typeStr, "${") && strings.HasSuffix(typeStr, "}") {
		return nil
	}

	// Validate array type
	if strings.HasPrefix(typeStr, "[]") {
		elementType := strings.TrimPrefix(typeStr, "[]")

		if !isValidBaseType(elementType) && !isValidComplexType(elementType) {
			return &ValidationError{
				Message: fmt.Sprintf("Invalid array element type '%s' at %s", elementType, path),
				Line:    fieldPosition.Line,
				Column:  fieldPosition.Column,
				EndLine: fieldPosition.EndLine,
				EndCol:  fieldPosition.EndCol,
			}
		}
		return nil
	}

	// Validate map type
	if strings.HasPrefix(typeStr, "map[") && strings.Contains(typeStr, "]") {
		if !typePatterns["map"].MatchString(typeStr) {
			return &ValidationError{
				Message: fmt.Sprintf("Invalid map type syntax '%s' at %s", typeStr, path),
				Line:    fieldPosition.Line,
				Column:  fieldPosition.Column,
				EndLine: fieldPosition.EndLine,
				EndCol:  fieldPosition.EndCol,
			}
		}

		matches := typePatterns["map"].FindStringSubmatch(typeStr)
		if len(matches) >= 3 {
			keyType := matches[1]
			valueType := matches[2]

			if keyType != "string" {
				return &ValidationError{
					Message: fmt.Sprintf("Invalid map key type '%s' at %s. Only 'string' is supported as key type.", keyType, path),
					Line:    fieldPosition.Line,
					Column:  fieldPosition.Column,
					EndLine: fieldPosition.EndLine,
					EndCol:  fieldPosition.EndCol,
				}
			}

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

	// Validate base type with modifiers
	typeWithModifiers := splitOutsideQuotes(typeStr, '|')
	baseType := strings.TrimSpace(typeWithModifiers[0])

	if !isValidBaseType(baseType) {
		return &ValidationError{
			Message: fmt.Sprintf("Invalid type '%s' at %s. Must be one of: string, integer, boolean, number, object", baseType, path),
			Line:    fieldPosition.Line,
			Column:  fieldPosition.Column,
			EndLine: fieldPosition.EndLine,
			EndCol:  fieldPosition.EndCol,
		}
	}

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

// checks if a type is a supported primitive type
func isValidBaseType(typeName string) bool {
	for _, validType := range validBaseTypes {
		if strings.TrimSpace(typeName) == validType {
			return true
		}
	}
	return false
}

// checks if a type is a valid complex type (array or map)
func isValidComplexType(typeName string) bool {
	if strings.HasPrefix(typeName, "[]") {
		subType := strings.TrimPrefix(typeName, "[]")
		return isValidBaseType(subType) || isValidComplexType(subType)
	}

	if strings.HasPrefix(typeName, "map[") {
		return typePatterns["map"].MatchString(typeName)
	}

	return false
}

// validates a single type modifier
func validateModifier(modifier string, baseType string) error {
	// Handle quoted description values specially
	if strings.HasPrefix(modifier, "description=") &&
		(strings.Contains(modifier, "\"") || strings.Contains(modifier, "'")) {
		return validateDescriptionModifier(modifier)
	}

	// Process multiple modifiers
	if strings.Contains(modifier, " ") {
		subModifiers := splitOutsideQuotes(modifier, ' ')
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

	parts := strings.SplitN(modifier, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("modifier must be in format 'name=value'")
	}

	modifierName := strings.TrimSpace(parts[0])
	modifierValue := strings.TrimSpace(parts[1])

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

	switch modifierName {
	case "required":
		if modifierValue != "true" && modifierValue != "false" {
			return fmt.Errorf("required modifier must be 'true' or 'false'")
		}
	case "description":
		if !isValidQuotedString(modifierValue) && !isSimpleString(modifierValue) {
			return fmt.Errorf("description value should be a valid string")
		}
	case "minimum", "maximum":
		if baseType != "integer" && baseType != "number" {
			return fmt.Errorf("%s modifier can only be used with numeric types", modifierName)
		}
	case "minLength", "maxLength":
		if baseType != "string" {
			return fmt.Errorf("%s modifier can only be used with string type", modifierName)
		}
	case "enum":
		if !strings.Contains(modifierValue, ",") && len(modifierValue) <= 2 {
			return fmt.Errorf("enum should have multiple values separated by commas")
		}
	}

	return nil
}

// handles the special case of quoted description values
func validateDescriptionModifier(modifier string) error {
	if !strings.HasPrefix(modifier, "description=") {
		return fmt.Errorf("expected description modifier")
	}

	value := strings.TrimPrefix(modifier, "description=")

	if len(value) < 2 {
		return fmt.Errorf("description value should be a valid string")
	}

	if (value[0] == '"' && value[len(value)-1] == '"') ||
		(value[0] == '\'' && value[len(value)-1] == '\'') {
		return nil
	}

	return fmt.Errorf("description value should be properly quoted")
}

// verifies that a string is properly quoted
func isValidQuotedString(s string) bool {
	if len(s) < 2 {
		return false
	}

	if s[0] == '"' && s[len(s)-1] == '"' {
		return true
	}

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

	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}

	return true
}

// splitOutsideQuotes splits a string using the given delimiter but ignores delimiters inside quotes.
// It keeps text inside single (”) or double ("") quotes together as one piece.
func splitOutsideQuotes(s string, delimiter rune) []string {
	var result []string
	var builder strings.Builder
	inSingleQuotes := false
	inDoubleQuotes := false

	for _, r := range s {
		if r == '\'' && !inDoubleQuotes {
			inSingleQuotes = !inSingleQuotes
			builder.WriteRune(r)
		} else if r == '"' && !inSingleQuotes {
			inDoubleQuotes = !inDoubleQuotes
			builder.WriteRune(r)
		} else if r == delimiter && !inSingleQuotes && !inDoubleQuotes {
			result = append(result, builder.String())
			builder.Reset()
		} else {
			builder.WriteRune(r)
		}
	}

	if builder.Len() > 0 {
		result = append(result, builder.String())
	}

	return result
}

// finds the position information for a field in the document
func getFieldPosition(path string, doc *YAMLDocument) YAMLField {
	if fieldPosition, found := doc.NestedFields[path]; found {
		return fieldPosition
	}

	parts := strings.Split(path, ".")
	for i := len(parts) - 1; i >= 0; i-- {
		parentPath := strings.Join(parts[:i], ".")
		if fieldPosition, found := doc.NestedFields[parentPath]; found {
			return fieldPosition
		}
	}

	return YAMLField{
		Line:    0,
		Column:  0,
		EndLine: 0,
		EndCol:  10,
	}
}
