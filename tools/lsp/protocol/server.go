package protocol

import (
	"github.com/kro-run/kro/tools/lsp/server/diagnostics"
	"github.com/kro-run/kro/tools/lsp/server/validator"

	"reflect"

	"github.com/tliron/commonlog"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

var documentManager = NewDocumentManager()
var diagnosticManager = diagnostics.NewDiagnosticManager()

// TextDocumentDidOpen handles document open notifications
func TextDocumentDidOpen(context *glsp.Context, params *protocol.DidOpenTextDocumentParams) error {
	log := commonlog.GetLogger("server")
	log.Infof("Document opened: %s", params.TextDocument.URI)

	documentManager.AddDocument(
		params.TextDocument.URI,
		params.TextDocument.Text,
		params.TextDocument.Version,
	)

	validateDocument(context, params.TextDocument.URI, params.TextDocument.Text)
	return nil
}

// TextDocumentDidChange handles document change notifications
func TextDocumentDidChange(context *glsp.Context, params *protocol.DidChangeTextDocumentParams) error {
	log := commonlog.GetLogger("server")
	log.Infof("Document changed: %s", params.TextDocument.URI)

	var content string
	if len(params.ContentChanges) > 0 {
		// Try type assertion with protocol.TextDocumentContentChangeEvent
		if contentChange, ok := params.ContentChanges[0].(protocol.TextDocumentContentChangeEvent); ok {
			content = contentChange.Text
		} else {
			// Try as a map
			switch v := params.ContentChanges[0].(type) {
			case map[string]interface{}:
				if text, ok := v["text"]; ok {
					if textStr, ok := text.(string); ok {
						content = textStr
					}
				}
			default:
				// Try reflection
				contentValue := reflect.ValueOf(params.ContentChanges[0])
				if contentValue.Kind() == reflect.Struct {
					textField := contentValue.FieldByName("Text")
					if textField.IsValid() && textField.Kind() == reflect.String {
						content = textField.String()
					}
				}
			}
		}

		// If all extraction attempts failed, get existing document
		if content == "" {
			doc, exists := documentManager.GetDocument(params.TextDocument.URI)
			if exists {
				content = doc.Content
			} else {
				log.Errorf("No document found for URI: %s", params.TextDocument.URI)
			}
		}
	}

	if content != "" {
		documentManager.UpdateDocument(
			params.TextDocument.URI,
			content,
			params.TextDocument.Version,
		)
		validateDocument(context, params.TextDocument.URI, content)
	} else {
		log.Errorf("Failed to extract content for %s", params.TextDocument.URI)
	}

	return nil
}

// getMapKeys extracts keys from a map for debugging purposes
func getMapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TextDocumentDidClose handles document close notifications
func TextDocumentDidClose(context *glsp.Context, params *protocol.DidCloseTextDocumentParams) error {
	log := commonlog.GetLogger("server")
	log.Infof("Document closed: %s", params.TextDocument.URI)
	documentManager.RemoveDocument(params.TextDocument.URI)
	return nil
}

// validateDocument performs validation and publishes diagnostics
func validateDocument(ctx *glsp.Context, uri string, content string) {
	log := commonlog.GetLogger("server")
	log.Infof("Validating document: %s", uri)

	diagnosticManager.ClearDiagnostics(uri)

	// Parse YAML with position tracking
	parser := validator.NewYAMLParser(content)
	yamlDoc, err := parser.ParseWithPositions()
	if err != nil {
		// Report parsing error
		diagnostic := CreateDiagnostic(
			err.Error(),
			protocol.DiagnosticSeverityError,
			CreateErrorRange(content, err.Error()),
		)
		diagnosticManager.AddDiagnostic(uri, diagnostic)
	} else {
		// Validate document structure and schema
		validationErrors := validator.ValidateKroFileWithPositions(yamlDoc)

		// Create diagnostics for validation errors
		for _, validationErr := range validationErrors {
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
	}

	// Get formatted diagnostics and publish
	diagnostics := diagnosticManager.GetDiagnostics(uri)
	formattedDiagnostics := formatDiagnostics(diagnostics)

	ctx.Notify(protocol.ServerTextDocumentPublishDiagnostics, &protocol.PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: formattedDiagnostics,
	})
}

// formatDiagnostics ensures consistent format for diagnostics
func formatDiagnostics(diagnostics []protocol.Diagnostic) []protocol.Diagnostic {
	formattedDiagnostics := make([]protocol.Diagnostic, 0, len(diagnostics))
	for _, diag := range diagnostics {
		// Ensure a clean diagnostic object without nil fields
		formattedDiag := protocol.Diagnostic{
			Range:    diag.Range,
			Severity: diag.Severity,
			Message:  diag.Message,
		}
		if diag.Source != nil {
			formattedDiag.Source = diag.Source
		}
		formattedDiagnostics = append(formattedDiagnostics, formattedDiag)
	}
	return formattedDiagnostics
}

// DidChangeWatchedFiles handles file system change notifications
func DidChangeWatchedFiles(context *glsp.Context, params *protocol.DidChangeWatchedFilesParams) error {
	log := commonlog.GetLogger("server")
	log.Infof("Watched files changed: %d events", len(params.Changes))

	for _, change := range params.Changes {
		if change.Type == protocol.FileChangeTypeChanged {
			doc, exists := documentManager.GetDocument(change.URI)
			if exists {
				validateDocument(context, change.URI, doc.Content)
			}
		}
	}

	return nil
}
