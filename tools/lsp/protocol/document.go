package protocol

import (
	"sync"
)

// DocumentManager handles tracking and management of open document state
// Uses sync.Map for concurrent access safety
type DocumentManager struct {
	documents sync.Map
}

// Document represents a text document being edited in the client
type Document struct {
	URI     string // Document URI identifier
	Content string // Current content of the document
	Version int32  // Document version, incremented on each change
}

// NewDocumentManager creates a new document manager instance
func NewDocumentManager() *DocumentManager {
	return &DocumentManager{}
}

// AddDocument adds a new document to the manager
func (dm *DocumentManager) AddDocument(uri string, content string, version int32) {
	dm.documents.Store(uri, &Document{
		URI:     uri,
		Content: content,
		Version: version,
	})
}

// GetDocument retrieves a document by URI
// Returns the document and a boolean indicating if it was found
func (dm *DocumentManager) GetDocument(uri string) (*Document, bool) {
	if doc, ok := dm.documents.Load(uri); ok {
		return doc.(*Document), true
	}
	return nil, false
}

// UpdateDocument updates an existing document with new content
func (dm *DocumentManager) UpdateDocument(uri string, content string, version int32) {
	dm.documents.Store(uri, &Document{
		URI:     uri,
		Content: content,
		Version: version,
	})
}

// RemoveDocument deletes a document from the manager
func (dm *DocumentManager) RemoveDocument(uri string) {
	dm.documents.Delete(uri)
}
