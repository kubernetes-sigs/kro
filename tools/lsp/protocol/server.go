package protocol

import (
	"github.com/kro-run/kro/tools/lsp/server/diagnostics"
	"github.com/kro-run/kro/tools/lsp/server/validator"

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
		// Extract the text from the content change
		contentChange, ok := params.ContentChanges[0].(protocol.TextDocumentContentChangeEvent)
		if ok {
			content = contentChange.Text
		} else {
			// If type assertion fails, try to get existing document
			doc, exists := documentManager.GetDocument(params.TextDocument.URI)
			if exists {
				content = doc.Content
			}
		}
	}

	// Store updated document
	if content != "" {
		documentManager.UpdateDocument(params.TextDocument.URI, content, params.TextDocument.Version)

		// Validate document
		validateDocument(context, params.TextDocument.URI, content)
	}

	return nil
}

func TextDocumentDidClose(context *glsp.Context, params *protocol.DidCloseTextDocumentParams) error {
	documentManager.RemoveDocument(params.TextDocument.URI)
	return nil
}

// Update the function signature to include context parameter
func validateDocument(ctx *glsp.Context, uri string, content string) {
	// Clear existing diagnostics
	diagnosticManager.ClearDiagnostics(uri)

	// Parse YAML
	parser := validator.NewYAMLParser(content)

	// First try the enhanced position-aware parsing
	yamlDoc, err := parser.ParseWithPositions()
	if err != nil {
		// If position-aware parsing fails, fall back to regular error
		diagnostic := CreateDiagnostic(
			err.Error(),
			protocol.DiagnosticSeverityError,
			CreateErrorRange(content, err.Error()),
		)
		diagnosticManager.AddDiagnostic(uri, diagnostic)
	} else {
		// Use the position-aware validation
		validationErrors := validator.ValidateKroFileWithPositions(yamlDoc)

		// Add diagnostics for each error with precise positions
		for _, validationErr := range validationErrors {
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
		data, _ := parser.Parse()
		if data != nil {
			errors := validator.ValidateKroFile(data)

			// Add diagnostics for each error
			for _, err := range errors {
				// Skip validation errors related to apiVersion and kind since we handled those above
				if strings.Contains(err.Error(), "apiVersion") || strings.Contains(err.Error(), "kind") {
					continue
				}

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

	// Create publish params
	params := &protocol.PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: diagnostics,
	}

	// Publish diagnostics directly
	ctx.Notify(protocol.ServerTextDocumentPublishDiagnostics, params)
}
