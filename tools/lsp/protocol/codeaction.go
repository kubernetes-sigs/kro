package protocol

import (
	"strings"

	"github.com/kro-run/kro/tools/lsp/server/validator"
	"github.com/tliron/commonlog"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

// provides code actions (quick fixes) for diagnostics
func HandleCodeAction(context *glsp.Context, params *protocol.CodeActionParams) (interface{}, error) {
	log := commonlog.GetLogger("codeaction")
	log.Infof("Code action requested for %s", params.TextDocument.URI)

	// Get document content
	doc, exists := documentManager.GetDocument(params.TextDocument.URI)
	if !exists {
		log.Warningf("Document not found: %s", params.TextDocument.URI)
		return []protocol.CodeAction{}, nil
	}

	// Get diagnostics for this URI and filter to requested range
	diagnostics := diagnosticManager.GetDiagnostics(params.TextDocument.URI)
	if len(diagnostics) == 0 {
		return []protocol.CodeAction{}, nil
	}

	diagnosticsInRange := filterDiagnosticsInRange(diagnostics, params.Range)
	if len(diagnosticsInRange) == 0 {
		return []protocol.CodeAction{}, nil
	}

	parser := validator.NewYAMLParser(doc.Content)
	yamlDoc, err := parser.ParseWithPositions()
	if err != nil {
		log.Warningf("Error parsing document: %v", err)

	}

	codeActions := []protocol.CodeAction{}

	for _, diag := range diagnosticsInRange {
		// Create appropriate code actions based on diagnostic message
		if strings.Contains(diag.Message, "apiVersion") {
			action := createApiVersionQuickFix(params.TextDocument.URI, diag, yamlDoc, doc.Content)
			if action != nil {
				codeActions = append(codeActions, *action)
			}
		}

		if strings.Contains(diag.Message, "kind") {
			action := createKindQuickFix(params.TextDocument.URI, diag, yamlDoc, doc.Content)
			if action != nil {
				codeActions = append(codeActions, *action)
			}
		}

		if strings.Contains(diag.Message, "metadata section is required") {
			action := createMetadataQuickFix(params.TextDocument.URI, diag, yamlDoc, doc.Content)
			if action != nil {
				codeActions = append(codeActions, *action)
			}
		}

		if strings.Contains(diag.Message, "metadata.name") {
			action := createMetadataNameQuickFix(params.TextDocument.URI, diag, yamlDoc, doc.Content)
			if action != nil {
				codeActions = append(codeActions, *action)
			}
		}
	}

	return codeActions, nil
}

// filterDiagnosticsInRange returns diagnostics that overlap with the given range
func filterDiagnosticsInRange(diagnostics []protocol.Diagnostic, rangeToFilter protocol.Range) []protocol.Diagnostic {
	var result []protocol.Diagnostic

	for _, diag := range diagnostics {
		if rangesOverlap(diag.Range, rangeToFilter) {
			result = append(result, diag)
		}
	}

	return result
}

// rangesOverlap checks if two ranges overlap
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

	return true
}

// lineContainsKeyPrefix checks if a line contains a specific key
func lineContainsKeyPrefix(content string, line uint32, key string) bool {
	lines := strings.Split(content, "\n")
	if int(line) >= len(lines) {
		return false
	}

	lineContent := strings.TrimSpace(lines[line])
	return strings.HasPrefix(lineContent, key+":") || strings.HasPrefix(lineContent, key+" :")
}

// creates a quick fix for apiVersion errors
func createApiVersionQuickFix(uri string, diagnostic protocol.Diagnostic, yamlDoc *validator.YAMLDocument, content string) *protocol.CodeAction {
	correctValue := "kro.run/v1alpha1"
	title := "Change to correct apiVersion: " + correctValue

	lines := strings.Split(content, "\n")
	if int(diagnostic.Range.Start.Line) >= len(lines) {
		return nil
	}

	originalLine := lines[diagnostic.Range.Start.Line]
	leadingSpaces := len(originalLine) - len(strings.TrimLeft(originalLine, " \t"))
	indentation := originalLine[:leadingSpaces]

	// Create a range for the entire line
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

	var newLine string
	if !strings.Contains(originalLine, ":") {
		newLine = indentation + "apiVersion: " + correctValue
	} else {
		colonIndex := strings.Index(originalLine, ":")
		beforeColon := originalLine[:colonIndex+1]
		newLine = beforeColon + " " + correctValue
	}

	edit := protocol.TextEdit{
		Range:   fullLineRange,
		NewText: newLine,
	}

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

// creates a quick fix for kind errors
func createKindQuickFix(uri string, diagnostic protocol.Diagnostic, yamlDoc *validator.YAMLDocument, content string) *protocol.CodeAction {
	correctValue := "ResourceGraphDefinition"
	title := "Change to correct kind: " + correctValue

	lines := strings.Split(content, "\n")
	if int(diagnostic.Range.Start.Line) >= len(lines) {
		return nil
	}

	originalLine := lines[diagnostic.Range.Start.Line]
	leadingSpaces := len(originalLine) - len(strings.TrimLeft(originalLine, " \t"))
	indentation := originalLine[:leadingSpaces]

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

	var newLine string
	if !strings.Contains(originalLine, ":") {
		newLine = indentation + "kind: " + correctValue
	} else {
		colonIndex := strings.Index(originalLine, ":")
		beforeColon := originalLine[:colonIndex+1]
		newLine = beforeColon + " " + correctValue
	}

	edit := protocol.TextEdit{
		Range:   fullLineRange,
		NewText: newLine,
	}

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

// createMetadataQuickFix creates a quick fix for missing metadata section
func createMetadataQuickFix(uri string, diagnostic protocol.Diagnostic, yamlDoc *validator.YAMLDocument, content string) *protocol.CodeAction {
	title := "Add metadata section"

	// Get line content and prepare edit
	lines := strings.Split(content, "\n")
	if int(diagnostic.Range.Start.Line) >= len(lines) {
		return nil
	}

	// Get indentation from original line
	originalLine := lines[diagnostic.Range.Start.Line]
	leadingSpaces := len(originalLine) - len(strings.TrimLeft(originalLine, " \t"))
	indentation := originalLine[:leadingSpaces]

	// Create insertion point
	insertRange := protocol.Range{
		Start: protocol.Position{
			Line:      diagnostic.Range.Start.Line,
			Character: 0,
		},
		End: protocol.Position{
			Line:      diagnostic.Range.Start.Line,
			Character: 0,
		},
	}

	// Create insertion text
	newText := indentation + "metadata:\n" + indentation + "  name: default-name\n"

	// Create text edit and workspace edit
	edit := protocol.TextEdit{
		Range:   insertRange,
		NewText: newText,
	}

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

// createMetadataNameQuickFix creates a quick fix for missing metadata.name field
func createMetadataNameQuickFix(uri string, diagnostic protocol.Diagnostic, yamlDoc *validator.YAMLDocument, content string) *protocol.CodeAction {
	title := "Add metadata.name field"

	lines := strings.Split(content, "\n")
	if int(diagnostic.Range.Start.Line) >= len(lines) {
		return nil
	}

	originalLine := lines[diagnostic.Range.Start.Line]
	leadingSpaces := len(originalLine) - len(strings.TrimLeft(originalLine, " \t"))
	indentation := originalLine[:leadingSpaces]

	insertRange := protocol.Range{
		Start: protocol.Position{
			Line:      diagnostic.Range.Start.Line,
			Character: 0,
		},
		End: protocol.Position{
			Line:      diagnostic.Range.Start.Line,
			Character: 0,
		},
	}

	isInsideMetadata := false
	metadataIndentation := ""

	for _, line := range lines {
		if strings.TrimSpace(line) == "metadata:" || strings.HasPrefix(strings.TrimSpace(line), "metadata:") {
			isInsideMetadata = true
			metadataIndentation = line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			break
		}
	}

	var newText string
	if isInsideMetadata {
		// Inside metadata section, add name field with proper indentation
		newText = metadataIndentation + "  name: default-name\n"
	} else {
		// Metadata section not found, add both metadata and name
		newText = indentation + "metadata:\n" + indentation + "  name: default-name\n"
	}

	// Create text edit and workspace edit
	edit := protocol.TextEdit{
		Range:   insertRange,
		NewText: newText,
	}

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
