package diagnostics

import (
	protocol "github.com/tliron/glsp/protocol_3_16"
)

type DiagnosticManager struct {
	diagnostics map[string][]protocol.Diagnostic
}

func NewDiagnosticManager() *DiagnosticManager {
	return &DiagnosticManager{
		diagnostics: make(map[string][]protocol.Diagnostic),
	}
}

func (dm *DiagnosticManager) AddDiagnostic(uri string, diagnostic protocol.Diagnostic) {
	if _, ok := dm.diagnostics[uri]; !ok {
		dm.diagnostics[uri] = []protocol.Diagnostic{}
	}
	dm.diagnostics[uri] = append(dm.diagnostics[uri], diagnostic)
}

func (dm *DiagnosticManager) GetDiagnostics(uri string) []protocol.Diagnostic {
	return dm.diagnostics[uri]
}

func (dm *DiagnosticManager) ClearDiagnostics(uri string) {
	dm.diagnostics[uri] = []protocol.Diagnostic{}
}
