package protocol

import (
	"sync"
)

type DocumentManager struct {
	documents sync.Map
}

type Document struct {
	URI     string
	Content string
	Version int32
}

func NewDocumentManager() *DocumentManager {
	return &DocumentManager{}
}

func (dm *DocumentManager) AddDocument(uri string, content string, version int32) {
	dm.documents.Store(uri, &Document{
		URI:     uri,
		Content: content,
		Version: version,
	})
}

func (dm *DocumentManager) GetDocument(uri string) (*Document, bool) {
	if doc, ok := dm.documents.Load(uri); ok {
		return doc.(*Document), true
	}
	return nil, false
}

func (dm *DocumentManager) UpdateDocument(uri string, content string, version int32) {
	dm.documents.Store(uri, &Document{
		URI:     uri,
		Content: content,
		Version: version,
	})
}

func (dm *DocumentManager) RemoveDocument(uri string) {
	dm.documents.Delete(uri)
}
