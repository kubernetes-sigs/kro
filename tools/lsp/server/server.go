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
	"runtime/debug"

	"github.com/go-logr/logr"
	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"
	"github.com/tliron/glsp/server"
	"k8s.io/client-go/rest"
)

func getVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}

	// Prefer the module version if it's set (and not a local dev build)
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}

	// Fall back to git info if available
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}

	if revision != "" {
		version := fmt.Sprintf("git-%s", revision[:min(7, len(revision))])
		if modified == "true" {
			version += "-dirty"
		}
		return version
	}

	// Default for local or untagged builds
	return "dev"
}

// kroServer represents the main KRO Language Server instance that implements
// the Language Server Protocol (LSP) for KRO ResourceGraphDefinition files.
// It handles document lifecycle, validation, and communication with LSP clients.
type kroServer struct {
	logger          logr.Logger
	documentManager *DocumentManager
	server          *server.Server
	currentContext  *glsp.Context
}

// NewKroServer creates a new KRO Language Server instance with the provided configuration.
// Parameters:
//   - logger: Structured logger for server operations
//   - clientConfig: Kubernetes client configuration (nil for offline mode)
//   - lspServer: GLSP server instance (can be nil, will be set later)
func NewKroServer(logger logr.Logger, clientConfig *rest.Config, lspServer *server.Server) *kroServer {
	return &kroServer{
		logger:          logger,
		documentManager: NewDocumentManager(logger, clientConfig),
		server:          lspServer,
	}
}

// PublishDiagnostics sends validation diagnostics (errors, warnings) to the LSP client.
// This is used to display real-time validation results in the editor.
func (s *kroServer) PublishDiagnostics(uri string, diagnostics []protocol.Diagnostic) {
	if s.currentContext != nil {
		// Ensure diagnostics is never nil to avoid client issues
		if diagnostics == nil {
			diagnostics = []protocol.Diagnostic{}
		}

		// Send diagnostics notification to the LSP client (editor)
		s.currentContext.Notify(protocol.ServerTextDocumentPublishDiagnostics, protocol.PublishDiagnosticsParams{
			URI:         uri,         // Document URI
			Diagnostics: diagnostics, // Validation results (errors, warnings, info)
		})
	}
}

// Initialize handles the LSP initialization request from the client.
// This is the first method called when the LSP client connects.
func (s *kroServer) Initialize(context *glsp.Context, params *protocol.InitializeParams) (any, error) {
	s.logger.Info("Initializing Kro Language Server")
	s.currentContext = context

	// Define what capabilities this LSP server supports
	capabilities := s.createServerCapabilities()

	// Return initialization result with server capabilities and info
	return protocol.InitializeResult{
		Capabilities: capabilities, // What features this server supports
		ServerInfo: &protocol.InitializeResultServerInfo{
			Name:    "Kro Language Server",
			Version: stringPtr(getVersion()),
		},
	}, nil
}

// Initialized handles the initialized notification from the client.
// Called after successful initialization to indicate the server is ready.
func (s *kroServer) Initialized(context *glsp.Context, params *protocol.InitializedParams) error {
	s.logger.Info("Server initialized successfully")
	s.currentContext = context
	// Set up the document manager to send notifications through this server
	s.documentManager.SetNotificationSender(s)
	return nil
}

// Shutdown handles the shutdown request from the client.
// Performs cleanup before the server terminates.
func (s *kroServer) Shutdown(context *glsp.Context) error {
	s.logger.Info("Shutting down server")
	return nil
}

// SetTrace handles trace setting changes from the client.
// Used for debugging and logging level control.
func (s *kroServer) SetTrace(context *glsp.Context, params *protocol.SetTraceParams) error {
	return nil
}

// Document lifecycle methods - handle document state changes in the editor

// DidOpen handles when a document is opened in the editor.
// Triggers initial validation of the KRO ResourceGraphDefinition.
func (s *kroServer) DidOpen(context *glsp.Context, params *protocol.DidOpenTextDocumentParams) error {
	uri := params.TextDocument.URI         // Document identifier
	version := params.TextDocument.Version // Document version for change tracking
	content := params.TextDocument.Text    // Full document content
	s.currentContext = context

	// Register the document and trigger validation
	s.documentManager.OpenDocument(uri, version, content)
	return nil
}

// DidChange handles when document content changes in the editor.
// Triggers re-validation with the updated content for real-time feedback.
func (s *kroServer) DidChange(context *glsp.Context, params *protocol.DidChangeTextDocumentParams) error {
	uri := params.TextDocument.URI         // Document identifier
	version := params.TextDocument.Version // New document version
	s.currentContext = context

	// No changes to process
	if len(params.ContentChanges) == 0 {
		return nil
	}

	var finalContent string
	var hasContent bool

	// Process all content changes - LSP can send multiple change events
	for _, change := range params.ContentChanges {
		var newContent string

		// Handle different types of change events (incremental vs full document)
		if changeEvent, ok := change.(protocol.TextDocumentContentChangeEvent); ok {
			newContent = changeEvent.Text
		} else if changeEvent, ok := change.(protocol.TextDocumentContentChangeEventWhole); ok {
			newContent = changeEvent.Text
		} else if changeMap, ok := change.(map[string]interface{}); ok {
			// Fallback for generic change format
			if text, textOk := changeMap["text"].(string); textOk {
				newContent = text
			}
		}

		// Keep the last valid content change
		if newContent != "" {
			finalContent = newContent
			hasContent = true
		}
	}

	// Update document with new content and trigger validation
	if hasContent {
		s.documentManager.UpdateDocument(uri, version, finalContent)
	}

	return nil
}

// DidClose handles when a document is closed in the editor.
// Cleans up document state and stops validation.
func (s *kroServer) DidClose(context *glsp.Context, params *protocol.DidCloseTextDocumentParams) error {
	uri := params.TextDocument.URI
	s.currentContext = context
	// Remove document from management and clear diagnostics
	s.documentManager.CloseDocument(uri)
	return nil
}

// DidSave handles when a document is saved in the editor.
// Triggers re-validation to ensure saved content is valid.
func (s *kroServer) DidSave(context *glsp.Context, params *protocol.DidSaveTextDocumentParams) error {
	uri := params.TextDocument.URI
	s.currentContext = context

	// Re-validate the document with current content
	if doc, exists := s.documentManager.GetDocument(uri); exists {
		s.documentManager.UpdateDocument(uri, doc.Version, doc.Content)
	}

	return nil
}

// createServerCapabilities defines what LSP features this server supports.
// Currently supports full document synchronization and save notifications.
func (s *kroServer) createServerCapabilities() protocol.ServerCapabilities {
	// Use full document sync (client sends entire document on changes)
	syncKind := protocol.TextDocumentSyncKindFull
	capabilities := protocol.ServerCapabilities{
		TextDocumentSync: protocol.TextDocumentSyncOptions{
			OpenClose: boolPtr(true), // Handle document open/close events
			Change:    &syncKind,     // Handle document change events (full sync)
			Save: &protocol.SaveOptions{
				IncludeText: boolPtr(true), // Include document text in save events
			},
		},
		// Future capabilities can be added here:
		// - CompletionProvider (auto-completion)
		// - HoverProvider (hover information)
		// - DefinitionProvider (go-to-definition)
		// - CodeActionProvider (quick fixes)
	}

	return capabilities
}

// DidChangeWatchedFiles handles file system change notifications.
// Currently not implemented but required by the LSP protocol.
func (s *kroServer) DidChangeWatchedFiles(_ *glsp.Context, _ *protocol.DidChangeWatchedFilesParams) error {
	return nil
}

// createHandler creates the LSP protocol handler with all supported methods.
// This maps LSP protocol methods to server implementation functions.
func (s *kroServer) createHandler() *protocol.Handler {
	handler := &protocol.Handler{
		// Lifecycle methods - server startup and shutdown
		Initialize:  s.Initialize,
		Initialized: s.Initialized,
		Shutdown:    s.Shutdown,

		// Document synchronization methods - handle document state changes
		TextDocumentDidOpen:   s.DidOpen,
		TextDocumentDidChange: s.DidChange,
		TextDocumentDidClose:  s.DidClose,
		TextDocumentDidSave:   s.DidSave,

		// Workspace methods - handle workspace-level changes
		WorkspaceDidChangeWatchedFiles: s.DidChangeWatchedFiles,

		// Optional notifications
		SetTrace: s.SetTrace,

		// Language feature methods would be added here:
		// TextDocumentCompletion: s.Completion,
		// TextDocumentHover: s.Hover,
		// TextDocumentDefinition: s.Definition,
	}

	return handler
}

// Utility functions for creating pointers to primitive types
// Required because LSP protocol uses pointers for optional fields

// stringPtr creates a pointer to a string value
func stringPtr(s string) *string {
	return &s
}

// boolPtr creates a pointer to a boolean value
func boolPtr(b bool) *bool {
	return &b
}
