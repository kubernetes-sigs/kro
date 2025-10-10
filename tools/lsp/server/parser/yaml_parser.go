// Copyright 2025 The Kubernetes Authors.
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

package parser

import (
	"strings"

	"github.com/go-logr/logr"
	protocol "github.com/tliron/glsp/protocol_3_16"
	"gopkg.in/yaml.v3"
)

// YAMLParser handles parsing and analysis of YAML documents for the KRO LSP server.
// It provides YAML syntax validation and KRO resource detection capabilities,
// serving as the first stage in the document validation pipeline.
type YAMLParser struct {
	logger logr.Logger
}

// ParseResult represents the outcome of YAML parsing operations.
// It contains the parsed YAML node structure, any parsing diagnostics,
// and an error status for use by the LSP validation pipeline.
type ParseResult struct {
	Node        *yaml.Node
	Diagnostics []protocol.Diagnostic
	HasErrors   bool
}

// NewYAMLParser creates a new YAML parser instance with the provided logger.
// The parser uses yaml.v3 for enhanced node positioning and error reporting capabilities.
func NewYAMLParser(logger logr.Logger) *YAMLParser {
	return &YAMLParser{
		logger: logger,
	}
}

// Parse performs YAML syntax validation and parsing on the provided content.
// Returns a ParseResult containing either the parsed AST or diagnostic errors.
// This is the primary entry point for YAML validation in the LSP pipeline.
func (p *YAMLParser) Parse(content string) ParseResult {
	// Initialize empty result
	result := ParseResult{
		Diagnostics: []protocol.Diagnostic{},
		HasErrors:   false,
	}

	// Skip parsing for empty or whitespace-only content
	if strings.TrimSpace(content) == "" {
		return result
	}

	// Parse YAML content into AST using yaml.v3
	// yaml.v3 provides better error reporting and node positioning than yaml.v2
	var node yaml.Node
	err := yaml.Unmarshal([]byte(content), &node)
	if err != nil {
		// Convert YAML parsing error to LSP diagnostic
		diagnostic := p.createYAMLErrorDiagnostic(err)
		result.Diagnostics = append(result.Diagnostics, diagnostic)
		result.HasErrors = true
		return result
	}

	// Parsing successful - store the AST node
	result.Node = &node
	return result
}

// IsKROResource determines if the parsed YAML represents a KRO ResourceGraphDefinition.
// This method analyzes the YAML structure to identify KRO-specific resources,
// enabling the LSP to apply KRO-specific validation only to relevant documents.
func (p *YAMLParser) IsKROResource(node *yaml.Node) bool {
	if node == nil {
		return false
	}

	// Navigate to the root mapping node of the YAML document
	rootNode := p.getRootMappingNode(node)
	if rootNode == nil {
		return false // Not a valid YAML mapping structure
	}

	// Extract apiVersion and kind fields for KRO resource identification
	apiVersion := p.getFieldValue(rootNode, "apiVersion")
	kind := p.getFieldValue(rootNode, "kind")

	// Check if this is a KRO ResourceGraphDefinition
	// KRO resources have apiVersion containing "kro.run" and kind "ResourceGraphDefinition"
	isKROResource := strings.Contains(apiVersion, "kro.run") && kind == "ResourceGraphDefinition"

	return isKROResource
}

// Note: This Parser will be improved in the future with proper diagnostics positioning
func (p *YAMLParser) getRootMappingNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}

	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil
		}
		node = node.Content[0]
	}

	if node.Kind != yaml.MappingNode {
		return nil
	}

	return node
}

func (p *YAMLParser) getFieldValue(mappingNode *yaml.Node, fieldName string) string {
	if mappingNode == nil || mappingNode.Kind != yaml.MappingNode {
		return ""
	}

	for i := 0; i < len(mappingNode.Content); i += 2 {
		if i+1 >= len(mappingNode.Content) {
			continue
		}

		keyNode := mappingNode.Content[i]
		valueNode := mappingNode.Content[i+1]

		if keyNode.Kind == yaml.ScalarNode &&
			valueNode.Kind == yaml.ScalarNode &&
			keyNode.Value == fieldName {
			return valueNode.Value
		}
	}

	return ""
}

func (p *YAMLParser) createYAMLErrorDiagnostic(err error) protocol.Diagnostic {
	return protocol.Diagnostic{
		Range: protocol.Range{
			Start: protocol.Position{Line: 0, Character: 0},
			End:   protocol.Position{Line: 0, Character: 10},
		},
		Severity: &[]protocol.DiagnosticSeverity{protocol.DiagnosticSeverityError}[0],
		Message:  "YAML parsing error: " + err.Error(),
		Source:   &[]string{"kro-lsp"}[0],
	}
}
