package main

import (
	"github.com/tliron/commonlog"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
	"github.com/tliron/glsp/server"
)

type kroServer struct {
	logger          commonlog.Logger
	documentManager *DocumentManager
	server          *server.Server
	currentContext  *glsp.Context
}

func NewKroServer(logger commonlog.Logger, lspServer *server.Server) *kroServer {
	return &kroServer{
		logger:          logger,
		documentManager: NewDocumentManager(logger),
		server:          lspServer,
	}
}

func (s *kroServer) PublishDiagnostics(uri string, diagnostics []protocol.Diagnostic) {
	if s.currentContext != nil {
		if diagnostics == nil {
			diagnostics = []protocol.Diagnostic{}
		}

		s.currentContext.Notify(protocol.ServerTextDocumentPublishDiagnostics, protocol.PublishDiagnosticsParams{
			URI:         uri,
			Diagnostics: diagnostics,
		})
	}
}

func (s *kroServer) Initialize(context *glsp.Context, params *protocol.InitializeParams) (any, error) {
	s.logger.Infof("Initializing Kro Language Server")
	s.currentContext = context

	capabilities := s.createServerCapabilities()

	return protocol.InitializeResult{
		Capabilities: capabilities,
		ServerInfo: &protocol.InitializeResultServerInfo{
			Name:    "Kro Language Server",
			Version: stringPtr("0.0.1"),
		},
	}, nil
}

func (s *kroServer) Initialized(context *glsp.Context, params *protocol.InitializedParams) error {
	s.logger.Infof("Server initialized successfully")
	s.currentContext = context
	s.documentManager.SetNotificationSender(s)
	return nil
}

func (s *kroServer) Shutdown(context *glsp.Context) error {
	s.logger.Info("Shutting down server")
	return nil
}

func (s *kroServer) SetTrace(context *glsp.Context, params *protocol.SetTraceParams) error {
	return nil
}

// Document lifecycle methods
func (s *kroServer) DidOpen(context *glsp.Context, params *protocol.DidOpenTextDocumentParams) error {
	uri := params.TextDocument.URI
	version := params.TextDocument.Version
	content := params.TextDocument.Text
	s.currentContext = context

	s.documentManager.OpenDocument(uri, version, content)
	return nil
}

func (s *kroServer) DidChange(context *glsp.Context, params *protocol.DidChangeTextDocumentParams) error {
	uri := params.TextDocument.URI
	version := params.TextDocument.Version
	s.currentContext = context

	if len(params.ContentChanges) == 0 {
		return nil
	}

	var finalContent string
	var hasContent bool

	for _, change := range params.ContentChanges {
		var newContent string

		if changeEvent, ok := change.(protocol.TextDocumentContentChangeEvent); ok {
			newContent = changeEvent.Text
		} else if changeEvent, ok := change.(protocol.TextDocumentContentChangeEventWhole); ok {
			newContent = changeEvent.Text
		} else if changeMap, ok := change.(map[string]interface{}); ok {
			if text, textOk := changeMap["text"].(string); textOk {
				newContent = text
			}
		}

		if newContent != "" {
			finalContent = newContent
			hasContent = true
		}
	}

	if hasContent {
		s.documentManager.UpdateDocument(uri, version, finalContent)
	}

	return nil
}

func (s *kroServer) DidClose(context *glsp.Context, params *protocol.DidCloseTextDocumentParams) error {
	uri := params.TextDocument.URI
	s.currentContext = context
	s.documentManager.CloseDocument(uri)
	return nil
}

func (s *kroServer) DidSave(context *glsp.Context, params *protocol.DidSaveTextDocumentParams) error {
	uri := params.TextDocument.URI
	s.currentContext = context

	if doc, exists := s.documentManager.GetDocument(uri); exists {
		s.documentManager.UpdateDocument(uri, doc.Version, doc.Content)
	}

	return nil
}

func (s *kroServer) createServerCapabilities() protocol.ServerCapabilities {
	syncKind := protocol.TextDocumentSyncKindFull
	capabilities := protocol.ServerCapabilities{
		TextDocumentSync: protocol.TextDocumentSyncOptions{
			OpenClose: boolPtr(true),
			Change:    &syncKind,
			Save: &protocol.SaveOptions{
				IncludeText: boolPtr(true),
			},
		},
	}

	return capabilities
}

func (s *kroServer) DidChangeWatchedFiles(_ *glsp.Context, _ *protocol.DidChangeWatchedFilesParams) error {
	return nil
}

func (s *kroServer) createHandler() *protocol.Handler {
	handler := &protocol.Handler{
		// Lifecycle methods
		Initialize:  s.Initialize,
		Initialized: s.Initialized,
		Shutdown:    s.Shutdown,

		// Document synchronization methods
		TextDocumentDidOpen:   s.DidOpen,
		TextDocumentDidChange: s.DidChange,
		TextDocumentDidClose:  s.DidClose,
		TextDocumentDidSave:   s.DidSave,

		// Workspace methods
		WorkspaceDidChangeWatchedFiles: s.DidChangeWatchedFiles,

		// Optional notifications
		SetTrace: s.SetTrace,

		// Language feature methods
	}

	return handler
}

func stringPtr(s string) *string {
	return &s
}

func boolPtr(b bool) *bool {
	return &b
}
