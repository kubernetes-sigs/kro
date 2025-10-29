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
	"sync"

	"github.com/go-logr/logr"
	protocol "github.com/tliron/glsp/protocol_3_16"
	"k8s.io/client-go/rest"
)

// NotificationSender defines the interface for sending LSP notifications to clients.
// This abstraction allows the DocumentManager to send diagnostics without being
// tightly coupled to the specific LSP server implementation.
type NotificationSender interface {
	PublishDiagnostics(uri string, diagnostics []protocol.Diagnostic)
}

// Document represents a text document being managed by the LSP server.
// It tracks the document's identity, version for change synchronization,
// and current content for validation purposes.
type Document struct {
	URI     string
	Version int32
	Content string
}

// DocumentManager handles the lifecycle and validation of documents in the LSP server.
// It maintains document state, coordinates validation, and publishes diagnostics to clients.
// The manager is thread-safe and supports both online and offline validation modes.
type DocumentManager struct {
	logger             logr.Logger
	documents          map[string]*Document
	mutex              sync.RWMutex
	notificationSender NotificationSender
	validationManager  *ValidationManager
}

// NewDocumentManager creates a new document manager with validation capabilities.
// The validation manager is created based on the provided client configuration:
// - If clientConfig is valid: enables online validation with cluster connectivity
// - If clientConfig is nil/invalid: enables offline validation mode
// - If validation manager creation fails: operates without validation
func NewDocumentManager(logger logr.Logger, clientConfig *rest.Config) *DocumentManager {
	// Attempt to create validation manager with the provided configuration
	validationManager, err := NewValidationManager(logger, clientConfig)
	if err != nil {
		// Log error but continue without validation rather than failing entirely
		logger.Error(err, "Failed to create validation manager")
		validationManager = nil // Graceful degradation - no validation available
	}

	return &DocumentManager{
		logger:            logger,
		documents:         make(map[string]*Document),
		validationManager: validationManager,
	}
}

// SetNotificationSender sets the notification sender for publishing diagnostics.
// This is typically called during LSP server initialization to establish the
// communication channel between document validation and client notifications.
func (dm *DocumentManager) SetNotificationSender(sender NotificationSender) {
	dm.notificationSender = sender
}

// OpenDocument handles the LSP textDocument/didOpen notification.
// Creates a new document entry, stores it in memory, and triggers initial validation.
// This is called when a user opens a KRO ResourceGraphDefinition file in their editor.
func (dm *DocumentManager) OpenDocument(uri string, version int32, content string) {
	// Thread-safe document creation and storage
	dm.mutex.Lock()
	dm.documents[uri] = &Document{
		URI:     uri,
		Version: version,
		Content: content,
	}
	dm.mutex.Unlock()

	// Trigger validation and send diagnostics to the client
	// This provides immediate feedback when a document is opened
	dm.validateAndPublishDiagnostics(uri)
}

// UpdateDocument handles the LSP textDocument/didChange notification.
// Updates the document content and version, then triggers re-validation.
// This is called when a user modifies a KRO ResourceGraphDefinition file,
// providing real-time validation feedback as they type.
func (dm *DocumentManager) UpdateDocument(uri string, version int32, content string) {
	var shouldValidate bool

	// Thread-safe document update
	dm.mutex.Lock()
	if doc, exists := dm.documents[uri]; exists {
		doc.Version = version
		doc.Content = content
		shouldValidate = true
	}
	dm.mutex.Unlock()

	// Only validate if the document was successfully updated
	// This provides real-time validation feedback on content changes
	if shouldValidate {
		dm.validateAndPublishDiagnostics(uri)
	}
}

// CloseDocument handles the LSP textDocument/didClose notification.
// Removes the document from memory and clears any displayed diagnostics.
// This is called when a user closes a KRO ResourceGraphDefinition file in their editor.
func (dm *DocumentManager) CloseDocument(uri string) {
	dm.mutex.Lock()
	defer dm.mutex.Unlock()

	// Remove document from memory to free resources
	delete(dm.documents, uri)

	// Clear diagnostics in the client by sending an empty diagnostics array
	// This ensures no stale error markers remain after closing the document
	if dm.notificationSender != nil {
		dm.notificationSender.PublishDiagnostics(uri, []protocol.Diagnostic{})
	}
}

// GetDocument retrieves a document by URI in a thread-safe manner.
// Returns the document and a boolean indicating if it was found.
// Used internally for validation and by external components for document access.
func (dm *DocumentManager) GetDocument(uri string) (*Document, bool) {
	dm.mutex.RLock() // Use read lock for concurrent access
	defer dm.mutex.RUnlock()
	doc, exists := dm.documents[uri]
	return doc, exists
}

// validateAndPublishDiagnostics performs validation on a document and sends results to the client.
// This is the core method that orchestrates validation and diagnostic reporting.
// It handles the complete pipeline from document retrieval to client notification.
func (dm *DocumentManager) validateAndPublishDiagnostics(uri string) {
	// Early return if no notification channel is available
	if dm.notificationSender == nil {
		return
	}

	// Retrieve the document to validate
	doc, exists := dm.GetDocument(uri)
	if !exists {
		return // Document not found, nothing to validate
	}

	// Perform validation and collect diagnostics
	diagnostics := dm.validateDocument(doc)

	// Log validation results for debugging and monitoring
	if len(diagnostics) > 0 {
		// Log each diagnostic with detailed information
		for i, diag := range diagnostics {
			dm.logger.V(1).Info("Diagnostic",
				"index", i+1,
				"line", diag.Range.Start.Line,
				"message", diag.Message)
		}
	} else {
		// Log successful validation (no errors found)
		dm.logger.V(1).Info("No diagnostics found - clearing previous errors")
	}

	// Send diagnostics to the LSP client for display in the editor
	// Empty array clears previous diagnostics, non-empty array shows new ones
	dm.notificationSender.PublishDiagnostics(uri, diagnostics)
}

// validateDocument performs the actual validation of a document's content.
// This method delegates to the ValidationManager for the heavy lifting of
// YAML parsing, KRO resource detection, and semantic validation.
// Returns an array of LSP diagnostics representing validation results.
func (dm *DocumentManager) validateDocument(doc *Document) []protocol.Diagnostic {
	var diagnostics []protocol.Diagnostic

	// Only perform validation if a validation manager is available
	// This handles graceful degradation when validation setup fails
	if dm.validationManager != nil {
		// Delegate to ValidationManager for comprehensive validation
		// This includes YAML parsing, KRO structure validation, and
		// either online (cluster-connected) or offline validation
		result := dm.validationManager.ValidateDocument(doc.Content)
		diagnostics = append(diagnostics, result.Diagnostics...)
	}
	// If validationManager is nil, return empty diagnostics (no validation)

	return diagnostics
}
