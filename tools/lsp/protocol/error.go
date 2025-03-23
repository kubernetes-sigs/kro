package protocol

import (
	"strings"

	lspProtocol "github.com/tliron/glsp/protocol_3_16"
)

// constructs a diagnostic notification with standard fields
func CreateDiagnostic(message string, severity lspProtocol.DiagnosticSeverity, rng lspProtocol.Range) lspProtocol.Diagnostic {
	source := "kro-language-server"
	severityValue := severity

	return lspProtocol.Diagnostic{
		Range:    rng,
		Severity: &severityValue,
		Source:   &source,
		Message:  message,
	}
}

// creates a range for an error by attempting to find the relevant position in the content
func CreateErrorRange(content string, errorMessage string) lspProtocol.Range {
	// Default position at the start of the document
	startLine := uint32(0)
	startChar := uint32(0)
	endLine := uint32(0)
	endChar := uint32(0)

	// Extract field name from error message
	fieldName := extractFieldName(errorMessage)

	if fieldName != "" && content != "" {
		lines := strings.Split(content, "\n")

		// First try to find exact field match
		for i, line := range lines {
			trimmedLine := strings.TrimSpace(line)
			fieldPattern := fieldName + ":"
			if strings.Contains(trimmedLine, fieldPattern) {
				startLine = uint32(i)
				startChar = uint32(strings.Index(line, fieldName))
				endLine = startLine
				endChar = startChar + uint32(len(fieldName))
				return createRange(startLine, startChar, endLine, endChar)
			}
		}

		// Handle nested fields in schema section
		if strings.Contains(errorMessage, "spec.schema") {
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
					startLine = uint32(i)
					startChar = uint32(strings.Index(line, fieldName))
					endLine = startLine
					endChar = startChar + uint32(len(fieldName))
					return createRange(startLine, startChar, endLine, endChar)
				}
			}

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

	return createRange(startLine, startChar, endLine, endChar)
}

// createRange creates a protocol Range object from line and character positions
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

// CreateSimpleErrorRange creates a range for an error at a specific line position
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

// extractFieldName extracts a field name from an error message using pattern matching strategies
func extractFieldName(errorMessage string) string {
	if strings.Contains(errorMessage, "spec.schema.apiVersion is required") {
		return "schema"
	}

	if strings.Contains(errorMessage, "invalid type") {
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
			parts := strings.Split(errorMessage, pattern)
			if len(parts) > 1 {
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
