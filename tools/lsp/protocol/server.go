package protocol

import (
	"github.com/kro-run/kro/tools/lsp/server/diagnostics"
	"github.com/kro-run/kro/tools/lsp/server/validator"

	"reflect"
	"strings"

	"github.com/tliron/commonlog"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

var documentManager = NewDocumentManager()
var diagnosticManager = diagnostics.NewDiagnosticManager()

func TextDocumentDidOpen(context *glsp.Context, params *protocol.DidOpenTextDocumentParams) error {
	log := commonlog.GetLogger("server")
	log.Infof("Document opened: %s", params.TextDocument.URI)

	// Store document
	documentManager.AddDocument(
		params.TextDocument.URI,
		params.TextDocument.Text,
		params.TextDocument.Version,
	)

	// Validate document
	validateDocument(context, params.TextDocument.URI, params.TextDocument.Text)

	return nil
}

func TextDocumentDidChange(context *glsp.Context, params *protocol.DidChangeTextDocumentParams) error {
	log := commonlog.GetLogger("server")
	log.Infof("Document changed: %s", params.TextDocument.URI)

	// Get the latest content
	var content string
	if len(params.ContentChanges) > 0 {
		// Try different ways to extract the content
		log.Infof("Content changes found: %d changes", len(params.ContentChanges))

		// Try type assertion with protocol.TextDocumentContentChangeEvent
		if contentChange, ok := params.ContentChanges[0].(protocol.TextDocumentContentChangeEvent); ok {
			content = contentChange.Text
			log.Infof("Extracted content from TextDocumentContentChangeEvent: %d chars", len(content))
		} else {
			// Try as a map
			switch v := params.ContentChanges[0].(type) {
			case map[string]interface{}:
				log.Infof("Change is a map with keys: %v", getMapKeys(v))
				if text, ok := v["text"]; ok {
					log.Infof("Map has text key with type: %T", text)
					if textStr, ok := text.(string); ok {
						content = textStr
						log.Infof("Successfully extracted text from map: %d chars", len(content))
					}
				}
			default:
				log.Infof("Change is of type: %T, trying to access Text field", v)

				// Try reflection
				contentValue := reflect.ValueOf(params.ContentChanges[0])
				if contentValue.Kind() == reflect.Struct {
					textField := contentValue.FieldByName("Text")
					if textField.IsValid() && textField.Kind() == reflect.String {
						content = textField.String()
						log.Infof("Extracted content via reflection: %d chars", len(content))
					}
				}
			}
		}

		// If all extraction attempts failed, get existing document
		if content == "" {
			log.Infof("All extraction attempts failed, getting existing document")
			doc, exists := documentManager.GetDocument(params.TextDocument.URI)
			if exists {
				log.Infof("Using existing document content with length: %d", len(doc.Content))
				content = doc.Content
			} else {
				log.Errorf("No existing document found for URI: %s", params.TextDocument.URI)
			}
		}
	} else {
		log.Infof("No content changes found in didChange notification")
	}

	// Store updated document
	if content != "" {
		log.Infof("Updating document with content length: %d", len(content))
		documentManager.UpdateDocument(params.TextDocument.URI, content, params.TextDocument.Version)

		// Validate document
		validateDocument(context, params.TextDocument.URI, content)
	} else {
		log.Errorf("No content to update for URI: %s", params.TextDocument.URI)
	}

	return nil
}

// Helper function to get map keys
func getMapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TextDocumentDidClose(context *glsp.Context, params *protocol.DidCloseTextDocumentParams) error {
	documentManager.RemoveDocument(params.TextDocument.URI)
	return nil
}

// Update the function signature to include context parameter
func validateDocument(ctx *glsp.Context, uri string, content string) {
	log := commonlog.GetLogger("server")
	log.Infof("Validating document: %s, content length: %d", uri, len(content))

	// Clear existing diagnostics
	diagnosticManager.ClearDiagnostics(uri)
	log.Infof("Cleared existing diagnostics")

	// Parse YAML
	parser := validator.NewYAMLParser(content)
	log.Infof("Created YAML parser")

	// First try the enhanced position-aware parsing
	yamlDoc, err := parser.ParseWithPositions()
	if err != nil {
		// If position-aware parsing fails, fall back to regular error
		log.Errorf("Position-aware parsing failed: %v", err)
		diagnostic := CreateDiagnostic(
			err.Error(),
			protocol.DiagnosticSeverityError,
			CreateErrorRange(content, err.Error()),
		)
		diagnosticManager.AddDiagnostic(uri, diagnostic)
	} else {
		log.Infof("Position-aware parsing succeeded")

		// Log the parsed content
		if len(yamlDoc.Data) > 0 {
			log.Infof("Parsed content has %d top-level keys", len(yamlDoc.Data))
			for key := range yamlDoc.Data {
				log.Infof("Found key: %s", key)
			}

			// Specifically log apiVersion and kind values
			if apiVersion, ok := yamlDoc.Data["apiVersion"]; ok {
				log.Infof("apiVersion value: %v", apiVersion)
			} else {
				log.Infof("apiVersion key not found")
			}

			if kind, ok := yamlDoc.Data["kind"]; ok {
				log.Infof("kind value: %v", kind)
			} else {
				log.Infof("kind key not found")
			}
		} else {
			log.Infof("Parsed content has no top-level keys")
		}

		// Use the position-aware validation
		validationErrors := validator.ValidateKroFileWithPositions(yamlDoc)
		log.Infof("Position-aware validation found %d errors", len(validationErrors))

		// Add diagnostics for each error with precise positions
		for i, validationErr := range validationErrors {
			log.Infof("Validation error %d: %s at line %d, col %d",
				i, validationErr.Message, validationErr.Line, validationErr.Column)

			// Create range directly from the validation error's position info
			rng := protocol.Range{
				Start: protocol.Position{
					Line:      uint32(validationErr.Line),
					Character: uint32(validationErr.Column),
				},
				End: protocol.Position{
					Line:      uint32(validationErr.EndLine),
					Character: uint32(validationErr.EndCol),
				},
			}

			diagnostic := CreateDiagnostic(
				validationErr.Message,
				protocol.DiagnosticSeverityError,
				rng,
			)
			diagnosticManager.AddDiagnostic(uri, diagnostic)
		}

		// Also perform regular validation for other errors
		data, parseErr := parser.Parse()
		if parseErr != nil {
			log.Errorf("Regular parsing failed: %v", parseErr)
		} else if data != nil {
			log.Infof("Regular parsing succeeded")
			errors := validator.ValidateKroFile(data)
			log.Infof("Regular validation found %d errors", len(errors))

			// Add diagnostics for each error
			for i, err := range errors {
				// Skip validation errors related to apiVersion and kind since we handled those above
				if strings.Contains(err.Error(), "apiVersion") || strings.Contains(err.Error(), "kind") {
					log.Infof("Skipping regular validation error %d about apiVersion/kind: %s", i, err.Error())
					continue
				}

				log.Infof("Regular validation error %d: %s", i, err.Error())
				diagnostic := CreateDiagnostic(
					err.Error(),
					protocol.DiagnosticSeverityError,
					CreateErrorRange(content, err.Error()),
				)
				diagnosticManager.AddDiagnostic(uri, diagnostic)
			}
		}
	}

	// Get all diagnostics for this URI
	diagnostics := diagnosticManager.GetDiagnostics(uri)
	log.Infof("Publishing %d diagnostics for URI: %s", len(diagnostics), uri)

	// Ensure diagnostics are properly formatted with all required fields
	formattedDiagnostics := make([]protocol.Diagnostic, 0, len(diagnostics))
	for _, diag := range diagnostics {
		// Create a clean copy with just the essential fields
		formattedDiag := protocol.Diagnostic{
			Range:    diag.Range,
			Severity: diag.Severity,
			Message:  diag.Message,
		}
		// Only add source if not nil - avoids potential serialization issues
		if diag.Source != nil {
			formattedDiag.Source = diag.Source
		}
		formattedDiagnostics = append(formattedDiagnostics, formattedDiag)
	}

	// Create publish params with minimal fields
	params := &protocol.PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: formattedDiagnostics,
	}

	// Simple logging to avoid serialization issues
	log.Infof("Publishing %d diagnostics for %s", len(formattedDiagnostics), uri)

	// Publish diagnostics directly
	ctx.Notify(protocol.ServerTextDocumentPublishDiagnostics, params)
	log.Infof("Diagnostics published")
}

// DidChangeWatchedFiles handles workspace/didChangeWatchedFiles notifications
func DidChangeWatchedFiles(context *glsp.Context, params *protocol.DidChangeWatchedFilesParams) error {
	log := commonlog.GetLogger("server")
	log.Infof("Watched files changed, count: %d", len(params.Changes))

	// Process each changed file
	for _, change := range params.Changes {
		log.Infof("File changed: %s, type: %d", change.URI, change.Type)

		// If it's a content change, try to re-validate
		if change.Type == protocol.FileChangeTypeChanged {
			// Get document if it exists
			doc, exists := documentManager.GetDocument(change.URI)
			if exists {
				log.Infof("Re-validating changed file: %s", change.URI)
				validateDocument(context, change.URI, doc.Content)
			}
		}
	}

	return nil
}
