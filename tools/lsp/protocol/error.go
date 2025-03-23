package protocol

import (
	"strings"

	lspProtocol "github.com/tliron/glsp/protocol_3_16"
)

func CreateDiagnostic(message string, severity lspProtocol.DiagnosticSeverity, rng lspProtocol.Range) lspProtocol.Diagnostic {
	source := "kro-language-server"
	severityValue := severity

	// Create a minimalist diagnostic with only required fields
	return lspProtocol.Diagnostic{
		Range:    rng,
		Severity: &severityValue,
		Source:   &source,
		Message:  message,
	}
}

// CreateErrorRange creates a range for an error, attempting to find the relevant position in the content
func CreateErrorRange(content string, errorMessage string) lspProtocol.Range {
	// Default position at the start of the document
	startLine := uint32(0)
	startChar := uint32(0)
	endLine := uint32(0)
	endChar := uint32(0)

	// Try to extract field name or key from error message
	fieldName := extractFieldName(errorMessage)

	if fieldName != "" && content != "" {
		// Find the line containing the field
		lines := strings.Split(content, "\n")

		// First try to find exact field match
		for i, line := range lines {
			trimmedLine := strings.TrimSpace(line)
			// Check for field: or "field": pattern
			fieldPattern := fieldName + ":"
			if strings.Contains(trimmedLine, fieldPattern) {
				startLine = uint32(i)
				startChar = uint32(strings.Index(line, fieldName))
				endLine = startLine
				endChar = startChar + uint32(len(fieldName))
				return createRange(startLine, startChar, endLine, endChar)
			}
		}

		// If not found, try to find partial matches for nested fields
		if strings.Contains(errorMessage, "spec.schema") {
			// Look for schema section
			inSchemaSection := false
			for i, line := range lines {
				trimmedLine := strings.TrimSpace(line)
				if strings.Contains(trimmedLine, "schema:") {
					inSchemaSection = true
					startLine = uint32(i)
					startChar = uint32(strings.Index(line, "schema"))
					endLine = startLine
					endChar = startChar + uint32(len("schema"))
				} else if inSchemaSection && strings.Contains(trimmedLine, fieldName+":") {
					// Found the specific field within schema section
					startLine = uint32(i)
					startChar = uint32(strings.Index(line, fieldName))
					endLine = startLine
					endChar = startChar + uint32(len(fieldName))
					return createRange(startLine, startChar, endLine, endChar)
				}
			}

			// If we found schema but not the specific field, return schema position
			if inSchemaSection {
				return createRange(startLine, startChar, endLine, endChar)
			}
		}

		// Fallback to any line containing the field name
		for i, line := range lines {
			if strings.Contains(line, fieldName) {
				startLine = uint32(i)
				startChar = uint32(strings.Index(line, fieldName))
				endLine = startLine
				endChar = startChar + uint32(len(fieldName))
				return createRange(startLine, startChar, endLine, endChar)
			}
		}
	}

	// Fallback to beginning of document
	return createRange(startLine, startChar, endLine, endChar)
}

// Helper function to create a range
func createRange(startLine, startChar, endLine, endChar uint32) lspProtocol.Range {
	return lspProtocol.Range{
		Start: lspProtocol.Position{
			Line:      startLine,
			Character: startChar,
		},
		End: lspProtocol.Position{
			Line:      endLine,
			Character: endChar,
		},
	}
}

// CreateSimpleErrorRange creates a range for an error at a specific line
func CreateSimpleErrorRange(line int, startChar int, endChar int) lspProtocol.Range {
	return lspProtocol.Range{
		Start: lspProtocol.Position{
			Line:      uint32(line),
			Character: uint32(startChar),
		},
		End: lspProtocol.Position{
			Line:      uint32(line),
			Character: uint32(endChar),
		},
	}
}

// extractFieldName attempts to extract a field name from an error message
func extractFieldName(errorMessage string) string {
	// Check for specific error patterns first
	if strings.Contains(errorMessage, "spec.schema.apiVersion is required") {
		return "schema"
	}

	// Check for type errors
	if strings.Contains(errorMessage, "invalid type") {
		// Extract field name from "invalid type 'X' for field 'Y'"
		parts := strings.Split(errorMessage, "for field '")
		if len(parts) > 1 {
			fieldPart := parts[1]
			if idx := strings.Index(fieldPart, "'"); idx > 0 {
				return fieldPart[:idx]
			}
		}
	}

	// Common error patterns
	patterns := []string{
		"field '",
		"missing ",
		"invalid ",
		"required field '",
	}

	for _, pattern := range patterns {
		if strings.Contains(errorMessage, pattern) {
			// Extract field name after pattern
			parts := strings.Split(errorMessage, pattern)
			if len(parts) > 1 {
				// Extract until next space, quote, or comma
				fieldPart := parts[1]
				for _, sep := range []string{" ", "'", ",", ":", "."} {
					if idx := strings.Index(fieldPart, sep); idx > 0 {
						fieldPart = fieldPart[:idx]
					}
				}
				return fieldPart
			}
		}
	}

	// Special case for apiVersion and kind
	if strings.Contains(errorMessage, "apiVersion") {
		return "apiVersion"
	}
	if strings.Contains(errorMessage, "kind") {
		return "kind"
	}

	return ""
}
