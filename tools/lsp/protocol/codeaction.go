package protocol

import (
	"strings"

	"github.com/kro-run/kro/tools/lsp/server/validator"
	"github.com/tliron/commonlog"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

// HandleCodeAction provides code actions (quick fixes) for diagnostics
func HandleCodeAction(context *glsp.Context, params *protocol.CodeActionParams) (interface{}, error) {
	log := commonlog.GetLogger("codeaction")
	log.Infof("Code action requested for %s", params.TextDocument.URI)

	// Get document content to analyze the document structure
	doc, exists := documentManager.GetDocument(params.TextDocument.URI)
	if !exists {
		log.Warningf("Document not found: %s", params.TextDocument.URI)
		return []protocol.CodeAction{}, nil
	}

	log.Infof("Document content length: %d", len(doc.Content))
	contentLines := strings.Split(doc.Content, "\n")
	log.Infof("Document has %d lines", len(contentLines))

	// Get all diagnostics for this URI
	diagnostics := diagnosticManager.GetDiagnostics(params.TextDocument.URI)
	if len(diagnostics) == 0 {
		log.Infof("No diagnostics found for URI: %s", params.TextDocument.URI)
		return []protocol.CodeAction{}, nil
	}

	log.Infof("Found %d diagnostics for URI: %s", len(diagnostics), params.TextDocument.URI)

	// Create code actions for diagnostics
	codeActions := []protocol.CodeAction{}

	// Filter diagnostics to those in range (if specified)
	diagnosticsInRange := filterDiagnosticsInRange(diagnostics, params.Range)
	log.Infof("Found %d diagnostics in the specified range", len(diagnosticsInRange))

	// Parse document to determine field formatting
	parser := validator.NewYAMLParser(doc.Content)
	yamlDoc, err := parser.ParseWithPositions()
	if err != nil {
		log.Warningf("Error parsing document: %v", err)
		// Continue with best-effort approach
	}

	for i, diag := range diagnosticsInRange {
		log.Infof("Processing diagnostic %d: %s", i, diag.Message)
		log.Infof("  Range: line %d:%d to %d:%d",
			diag.Range.Start.Line, diag.Range.Start.Character,
			diag.Range.End.Line, diag.Range.End.Character)

		// Log the actual line content for debugging
		if int(diag.Range.Start.Line) < len(contentLines) {
			line := contentLines[diag.Range.Start.Line]
			log.Infof("  Line content: '%s'", line)
		}

		// Create code actions based on diagnostic message content
		if strings.Contains(diag.Message, "apiVersion") {
			log.Infof("  Creating apiVersion quick fix")
			action := createApiVersionQuickFix(params.TextDocument.URI, diag, yamlDoc, doc.Content)
			if action != nil {
				codeActions = append(codeActions, *action)
				log.Infof("  Added apiVersion quick fix with title: %s", action.Title)
				// Log the edit that will be applied
				if action.Edit != nil && action.Edit.Changes != nil {
					for _, edits := range action.Edit.Changes {
						for _, edit := range edits {
							log.Infof("  Edit range: line %d:%d to %d:%d",
								edit.Range.Start.Line, edit.Range.Start.Character,
								edit.Range.End.Line, edit.Range.End.Character)
							log.Infof("  New text: '%s'", edit.NewText)
						}
					}
				}
			}
		}

		if strings.Contains(diag.Message, "kind") {
			log.Infof("  Creating kind quick fix")
			action := createKindQuickFix(params.TextDocument.URI, diag, yamlDoc, doc.Content)
			if action != nil {
				codeActions = append(codeActions, *action)
				log.Infof("  Added kind quick fix with title: %s", action.Title)
				// Log the edit that will be applied
				if action.Edit != nil && action.Edit.Changes != nil {
					for _, edits := range action.Edit.Changes {
						for _, edit := range edits {
							log.Infof("  Edit range: line %d:%d to %d:%d",
								edit.Range.Start.Line, edit.Range.Start.Character,
								edit.Range.End.Line, edit.Range.End.Character)
							log.Infof("  New text: '%s'", edit.NewText)
						}
					}
				}
			}
		}
	}

	log.Infof("Returning %d code actions", len(codeActions))
	return codeActions, nil
}

// Helper function to filter diagnostics to those within a specified range
func filterDiagnosticsInRange(diagnostics []protocol.Diagnostic, rangeToFilter protocol.Range) []protocol.Diagnostic {
	var result []protocol.Diagnostic

	for _, diag := range diagnostics {
		if rangesOverlap(diag.Range, rangeToFilter) {
			result = append(result, diag)
		}
	}

	return result
}

// Helper function to check if two ranges overlap
func rangesOverlap(r1, r2 protocol.Range) bool {
	// Check if one range is completely before the other
	if r1.End.Line < r2.Start.Line ||
		(r1.End.Line == r2.Start.Line && r1.End.Character < r2.Start.Character) {
		return false
	}

	if r2.End.Line < r1.Start.Line ||
		(r2.End.Line == r1.Start.Line && r2.End.Character < r1.Start.Character) {
		return false
	}

	// If neither range is completely before the other, they must overlap
	return true
}

// Helper function to check if line contains key:value format for a specific key
func lineContainsKeyPrefix(content string, line uint32, key string) bool {
	lines := strings.Split(content, "\n")
	if int(line) >= len(lines) {
		return false
	}

	lineContent := strings.TrimSpace(lines[line])
	return strings.HasPrefix(lineContent, key+":") || strings.HasPrefix(lineContent, key+" :")
}

// Create quick fix for apiVersion errors
func createApiVersionQuickFix(uri string, diagnostic protocol.Diagnostic, yamlDoc *validator.YAMLDocument, content string) *protocol.CodeAction {
	correctValue := "kro.run/v1alpha1"
	title := "Change to correct apiVersion: " + correctValue

	// Get the line content to analyze if it's a standalone value or already has key
	lines := strings.Split(content, "\n")
	if int(diagnostic.Range.Start.Line) >= len(lines) {
		return nil
	}

	// Get the original line and its indentation
	originalLine := lines[diagnostic.Range.Start.Line]
	leadingSpaces := len(originalLine) - len(strings.TrimLeft(originalLine, " \t"))
	indentation := originalLine[:leadingSpaces]

	// Create a range that covers the entire line
	fullLineRange := protocol.Range{
		Start: protocol.Position{
			Line:      diagnostic.Range.Start.Line,
			Character: 0,
		},
		End: protocol.Position{
			Line:      diagnostic.Range.Start.Line,
			Character: uint32(len(originalLine)),
		},
	}

	// Check if this looks like a standalone value (no colon in the line)
	if !strings.Contains(originalLine, ":") {
		newLine := indentation + "apiVersion: " + correctValue

		// Create text edit to replace the entire line
		edit := protocol.TextEdit{
			Range:   fullLineRange,
			NewText: newLine,
		}

		// Create document changes directly with a WorkspaceEdit
		workspaceEdit := &protocol.WorkspaceEdit{
			Changes: map[string][]protocol.TextEdit{
				uri: {edit},
			},
		}

		// Create code action
		kind := protocol.CodeActionKindQuickFix
		return &protocol.CodeAction{
			Title:       title,
			Kind:        &kind,
			Diagnostics: []protocol.Diagnostic{diagnostic},
			Edit:        workspaceEdit,
		}
	}

	// Handle case where the line has a key but incorrect value
	colonIndex := strings.Index(originalLine, ":")
	if colonIndex > 0 {
		// Extract the part before the colon (key + indentation)
		beforeColon := originalLine[:colonIndex+1]

		// Create a new line with the existing key and correct value
		newLine := beforeColon + " " + correctValue

		// Create text edit to replace the entire line
		edit := protocol.TextEdit{
			Range:   fullLineRange,
			NewText: newLine,
		}

		// Create document changes directly with a WorkspaceEdit
		workspaceEdit := &protocol.WorkspaceEdit{
			Changes: map[string][]protocol.TextEdit{
				uri: {edit},
			},
		}

		// Create code action
		kind := protocol.CodeActionKindQuickFix
		return &protocol.CodeAction{
			Title:       title,
			Kind:        &kind,
			Diagnostics: []protocol.Diagnostic{diagnostic},
			Edit:        workspaceEdit,
		}
	}

	return nil
}

// Create quick fix for kind errors
func createKindQuickFix(uri string, diagnostic protocol.Diagnostic, yamlDoc *validator.YAMLDocument, content string) *protocol.CodeAction {
	correctValue := "ResourceGraphDefinition"
	title := "Change to correct kind: " + correctValue

	// Get the line content to analyze if it's a standalone value or already has key
	lines := strings.Split(content, "\n")
	if int(diagnostic.Range.Start.Line) >= len(lines) {
		return nil
	}

	// Get the original line and its indentation
	originalLine := lines[diagnostic.Range.Start.Line]
	leadingSpaces := len(originalLine) - len(strings.TrimLeft(originalLine, " \t"))
	indentation := originalLine[:leadingSpaces]

	// Create a range that covers the entire line
	fullLineRange := protocol.Range{
		Start: protocol.Position{
			Line:      diagnostic.Range.Start.Line,
			Character: 0,
		},
		End: protocol.Position{
			Line:      diagnostic.Range.Start.Line,
			Character: uint32(len(originalLine)),
		},
	}

	// Check if this looks like a standalone value (no colon in the line)
	if !strings.Contains(originalLine, ":") {
		newLine := indentation + "kind: " + correctValue

		// Create text edit to replace the entire line
		edit := protocol.TextEdit{
			Range:   fullLineRange,
			NewText: newLine,
		}

		// Create document changes directly with a WorkspaceEdit
		workspaceEdit := &protocol.WorkspaceEdit{
			Changes: map[string][]protocol.TextEdit{
				uri: {edit},
			},
		}

		// Create code action
		kind := protocol.CodeActionKindQuickFix
		return &protocol.CodeAction{
			Title:       title,
			Kind:        &kind,
			Diagnostics: []protocol.Diagnostic{diagnostic},
			Edit:        workspaceEdit,
		}
	}

	// Handle case where the line has a key but incorrect value
	colonIndex := strings.Index(originalLine, ":")
	if colonIndex > 0 {
		// Extract the part before the colon (key + indentation)
		beforeColon := originalLine[:colonIndex+1]

		// Create a new line with the existing key and correct value
		newLine := beforeColon + " " + correctValue

		// Create text edit to replace the entire line
		edit := protocol.TextEdit{
			Range:   fullLineRange,
			NewText: newLine,
		}

		// Create document changes directly with a WorkspaceEdit
		workspaceEdit := &protocol.WorkspaceEdit{
			Changes: map[string][]protocol.TextEdit{
				uri: {edit},
			},
		}

		// Create code action
		kind := protocol.CodeActionKindQuickFix
		return &protocol.CodeAction{
			Title:       title,
			Kind:        &kind,
			Diagnostics: []protocol.Diagnostic{diagnostic},
			Edit:        workspaceEdit,
		}
	}

	return nil
}
