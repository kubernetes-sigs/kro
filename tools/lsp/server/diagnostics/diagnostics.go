package diagnostics

import (
	protocol "github.com/tliron/glsp/protocol_3_16"
)

// maintains collections of diagnostics for open documents.
type DiagnosticManager struct {
	diagnostics map[string][]protocol.Diagnostic // Maps document URIs to their diagnostics
}

// creates a new diagnostic manager instance with an initialized diagnostics map.
func NewDiagnosticManager() *DiagnosticManager {
	return &DiagnosticManager{
		diagnostics: make(map[string][]protocol.Diagnostic),
	}
}

// appends a diagnostic to the collection for the specified document URI.
func (dm *DiagnosticManager) AddDiagnostic(uri string, diagnostic protocol.Diagnostic) {
	if _, ok := dm.diagnostics[uri]; !ok {
		dm.diagnostics[uri] = []protocol.Diagnostic{}
	}
	dm.diagnostics[uri] = append(dm.diagnostics[uri], diagnostic)
}

// retrieves all diagnostics for the specified document URI.
func (dm *DiagnosticManager) GetDiagnostics(uri string) []protocol.Diagnostic {
	return dm.diagnostics[uri]
}

// removes all diagnostics for the specified document URI
func (dm *DiagnosticManager) ClearDiagnostics(uri string) {
	dm.diagnostics[uri] = []protocol.Diagnostic{}
}
