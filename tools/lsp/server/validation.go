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

package main

import (
	"fmt"

	"github.com/go-logr/logr"
	"github.com/kro-run/kro/api/v1alpha1"
	"github.com/kro-run/kro/pkg/graph"
	"github.com/kro-run/kro/tools/lsp/server/parser"
	protocol "github.com/tliron/glsp/protocol_3_16"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

// ValidationManager handles comprehensive validation of KRO ResourceGraphDefinition documents.
// It supports both online validation (with cluster connectivity for CRD and CEL validation)
// and offline validation (basic structure and YAML syntax validation only).
type ValidationManager struct {
	logger     logr.Logger
	yamlParser *parser.YAMLParser
	builder    *graph.Builder
}

// ValidationResult represents the outcome of document validation, containing
// diagnostics (errors, warnings) and an overall error status for LSP clients.
type ValidationResult struct {
	Diagnostics []protocol.Diagnostic
	HasErrors   bool
}

// NewValidationManager creates a new validation manager with the appropriate mode.
// If clientConfig is provided and valid, enables online mode with full KRO validation.
// If clientConfig is nil or invalid, falls back to offline mode with basic validation.
func NewValidationManager(logger logr.Logger, clientConfig *rest.Config) (*ValidationManager, error) {
	// Always create YAML parser for basic syntax validation
	yamlParser := parser.NewYAMLParser(logger)

	// If no client config is provided, operate in offline mode
	if clientConfig == nil {
		return &ValidationManager{
			logger:     logger,
			yamlParser: yamlParser,
			builder:    nil, // nil builder indicates offline mode
		}, nil
	}

	// Try to create KRO Builder for online validation
	// This enables CRD schema validation and CEL expression evaluation
	builder, err := graph.NewBuilder(clientConfig)
	if err != nil {
		// If builder creation fails, gracefully fall back to offline mode
		return &ValidationManager{
			logger:     logger,
			yamlParser: yamlParser,
			builder:    nil, // Offline mode fallback
		}, nil
	}

	// Successfully created builder - online mode enabled
	return &ValidationManager{
		logger:     logger,
		yamlParser: yamlParser,
		builder:    builder, // Online mode with full validation capabilities
	}, nil
}

// ValidateDocument performs comprehensive validation of a KRO ResourceGraphDefinition document.
// Validation occurs in multiple stages, from basic YAML syntax to advanced KRO semantics.
// Returns validation results suitable for LSP diagnostic reporting.
func (vm *ValidationManager) ValidateDocument(content string) ValidationResult {
	// Initialize empty result
	result := ValidationResult{
		Diagnostics: []protocol.Diagnostic{},
		HasErrors:   false,
	}

	// Skip validation for empty documents
	if content == "" {
		return result
	}

	// Stage 1: YAML parsing and syntax validation
	// Checks for basic YAML syntax errors and structural issues
	parseResult := vm.yamlParser.Parse(content)
	result.Diagnostics = append(result.Diagnostics, parseResult.Diagnostics...)
	result.HasErrors = parseResult.HasErrors

	// If YAML parsing failed, cannot proceed with further validation
	if parseResult.HasErrors {
		for i, diag := range parseResult.Diagnostics {
			vm.logger.V(1).Info("YAML parsing error",
				"error_index", i,
				"message", diag.Message,
				"line", diag.Range.Start.Line)
		}
		return result
	}

	// Stage 2: Check if this is a KRO ResourceGraphDefinition
	// Only proceed with KRO-specific validation for KRO resources
	if !vm.yamlParser.IsKROResource(parseResult.Node) {
		return result // Not a KRO resource, no further validation needed
	}

	// Stage 3: Parse YAML to RGD struct
	// Convert YAML to strongly-typed KRO ResourceGraphDefinition structure
	rgd, err := vm.parseRGDToStruct(content)
	if err != nil {
		diagnostic := vm.createErrorDiagnostic(0, 0, fmt.Sprintf("Failed to parse RGD: %s", err.Error()))
		result.Diagnostics = append(result.Diagnostics, diagnostic)
		result.HasErrors = true
		return result
	}

	// Stage 4: KRO validation (online vs offline mode)
	if vm.builder != nil {
		// Online mode: Full KRO validation including:
		// - CRD schema validation against live cluster
		// - CEL expression validation with cluster context
		// - Cross-resource dependency validation
		// - Field type validation against OpenAPI schemas
		_, err := vm.builder.NewResourceGraphDefinition(rgd)
		if err != nil {
			diagnostic := vm.createErrorDiagnostic(0, 0, fmt.Sprintf("KRO validation failed: %s", err.Error()))
			result.Diagnostics = append(result.Diagnostics, diagnostic)
			result.HasErrors = true
		}
	} else {
		// Offline mode: Basic structure validation only
		// Limited to static checks without cluster connectivity
		if err := vm.validateBasicStructure(rgd); err != nil {
			diagnostic := vm.createErrorDiagnostic(0, 0, fmt.Sprintf("Structure validation failed: %s", err.Error()))
			result.Diagnostics = append(result.Diagnostics, diagnostic)
			result.HasErrors = true
		}
	}

	return result
}

// parseRGDToStruct converts YAML content to a strongly-typed ResourceGraphDefinition struct.
// This enables programmatic access to RGD fields for validation and analysis.
func (vm *ValidationManager) parseRGDToStruct(content string) (*v1alpha1.ResourceGraphDefinition, error) {
	var rgd v1alpha1.ResourceGraphDefinition
	if err := yaml.Unmarshal([]byte(content), &rgd); err != nil {
		return nil, fmt.Errorf("failed to unmarshal RGD: %w", err)
	}

	return &rgd, nil
}

// validateBasicStructure performs offline validation of RGD structure.
// Checks for essential fields and basic structural requirements without cluster connectivity.
// This is a fallback validation method when online validation is not available.
func (vm *ValidationManager) validateBasicStructure(rgd *v1alpha1.ResourceGraphDefinition) error {
	// Validate required spec.schema section
	if rgd.Spec.Schema == nil {
		return fmt.Errorf("missing spec.schema section")
	}

	// Validate required schema.kind field
	if rgd.Spec.Schema.Kind == "" {
		return fmt.Errorf("missing spec.schema.kind")
	}

	// Additional basic validations could be added here:
	// - Check for required resources section
	// - Validate resource IDs are unique
	// - Check for basic template structure

	return nil
}

// createErrorDiagnostic creates an LSP diagnostic for validation errors.
// Diagnostics are used by LSP clients (editors) to display errors, warnings, and info messages.
// Currently creates error-level diagnostics at position (0,0) - this could be enhanced
// to provide precise line/column positions using YAML node information.
func (vm *ValidationManager) createErrorDiagnostic(line, character int, message string) protocol.Diagnostic {
	return protocol.Diagnostic{
		Range: protocol.Range{
			Start: protocol.Position{Line: uint32(line), Character: uint32(character)},
			End:   protocol.Position{Line: uint32(line), Character: uint32(character)},
		},
		Severity: &[]protocol.DiagnosticSeverity{protocol.DiagnosticSeverityError}[0], // Error severity
		Message:  message,                                                             // Human-readable error message
		Source:   &[]string{"kro-lsp"}[0],                                             // Source identifier for the LSP client
	}
}
