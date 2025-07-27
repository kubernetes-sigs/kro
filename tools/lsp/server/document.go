package main

import (
	"fmt"
	"strings"
	"sync"

	"github.com/tliron/commonlog"
	protocol "github.com/tliron/glsp/protocol_3_16"
)

type NotificationSender interface {
	PublishDiagnostics(uri string, diagnostics []protocol.Diagnostic)
}

type Document struct {
	URI     string
	Version int32
	Content string
}

type DocumentManager struct {
	logger             commonlog.Logger
	documents          map[string]*Document
	mutex              sync.RWMutex
	notificationSender NotificationSender
}

func NewDocumentManager(logger commonlog.Logger) *DocumentManager {
	return &DocumentManager{
		logger:    logger,
		documents: make(map[string]*Document),
	}
}

func (dm *DocumentManager) SetNotificationSender(sender NotificationSender) {
	dm.notificationSender = sender
}

func (dm *DocumentManager) OpenDocument(uri string, version int32, content string) {
	dm.mutex.Lock()
	dm.documents[uri] = &Document{
		URI:     uri,
		Version: version,
		Content: content,
	}
	dm.mutex.Unlock()
	dm.validateAndPublishDiagnostics(uri)
}

func (dm *DocumentManager) UpdateDocument(uri string, version int32, content string) {
	var shouldValidate bool
	dm.mutex.Lock()
	if doc, exists := dm.documents[uri]; exists {
		doc.Version = version
		doc.Content = content
		shouldValidate = true
	}
	dm.mutex.Unlock()

	if shouldValidate {
		dm.validateAndPublishDiagnostics(uri)
	}
}

func (dm *DocumentManager) CloseDocument(uri string) {
	dm.mutex.Lock()
	defer dm.mutex.Unlock()
	delete(dm.documents, uri)
	if dm.notificationSender != nil {
		dm.notificationSender.PublishDiagnostics(uri, []protocol.Diagnostic{})
	}
}

func (dm *DocumentManager) GetDocument(uri string) (*Document, bool) {
	dm.mutex.RLock()
	defer dm.mutex.RUnlock()
	doc, exists := dm.documents[uri]
	return doc, exists
}

func (dm *DocumentManager) validateAndPublishDiagnostics(uri string) {
	if dm.notificationSender == nil {
		return
	}

	doc, exists := dm.GetDocument(uri)
	if !exists {
		return
	}

	diagnostics := dm.validateDocument(doc)

	if len(diagnostics) > 0 {
		for i, diag := range diagnostics {
			dm.logger.Debugf("Diagnostic %d: Line %d, Message: %s", i+1, diag.Range.Start.Line, diag.Message)
		}
	} else {
		dm.logger.Debugf("No diagnostics found - clearing previous errors")
	}

	dm.notificationSender.PublishDiagnostics(uri, diagnostics)
}

/**
  No need to review this part. Will implement a proper validation manager. It is just temporary.
*/

func (dm *DocumentManager) validateDocument(doc *Document) []protocol.Diagnostic {
	var diagnostics []protocol.Diagnostic

	if err := dm.validateYAML(doc.Content); err != nil {
		diagnostics = append(diagnostics, protocol.Diagnostic{
			Range: protocol.Range{
				Start: protocol.Position{Line: 0, Character: 0},
				End:   protocol.Position{Line: 0, Character: 0},
			},
			Severity: severityPtr(protocol.DiagnosticSeverityError),
			Message:  fmt.Sprintf("YAML parsing error: %v", err),
		})
		return diagnostics
	}

	diagnostics = append(diagnostics, dm.validateKROIfApplicable(doc.Content)...)

	return diagnostics
}

// basic YAML file validation
func (dm *DocumentManager) validateYAML(content string) error {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.Contains(trimmed, ":") &&
			!strings.HasPrefix(trimmed, "-") &&
			!strings.HasPrefix(trimmed, "#") {
			return fmt.Errorf("invalid YAML syntax at line %d", i+1)
		}
	}
	return nil
}

func (dm *DocumentManager) validateKROIfApplicable(content string) []protocol.Diagnostic {
	lines := strings.Split(content, "\n")

	isKro := isKroRGD(lines)
	if !isKro {
		return nil
	}

	kindLine, isValidKind := parseKind(lines)
	metaFound, metaLine, hasName, nameLine := parseMetadata(lines)

	var diagnostics []protocol.Diagnostic

	if !isValidKind {
		line := kindLine
		if line == -1 {
			line = 0
		}
		diagnostics = append(diagnostics, protocol.Diagnostic{
			Range:    newRange(line, 0, line, 0),
			Severity: severityPtr(protocol.DiagnosticSeverityError),
			Message:  "KRO resource must have kind: ResourceGraphDefinition",
		})
	}

	if !metaFound {
		insertLine := kindLine
		if insertLine == -1 {
			insertLine = 0
		}
		diagnostics = append(diagnostics, protocol.Diagnostic{
			Range:    newRange(insertLine+1, 0, insertLine+1, 0),
			Severity: severityPtr(protocol.DiagnosticSeverityError),
			Message:  "Missing required 'metadata' section for KRO resource",
		})
	} else if !hasName {
		targetLine := nameLine
		if targetLine == -1 {
			targetLine = metaLine
		}
		diagnostics = append(diagnostics, protocol.Diagnostic{
			Range:    newRange(targetLine, 0, targetLine, 0),
			Severity: severityPtr(protocol.DiagnosticSeverityError),
			Message:  "Metadata section must include non-empty 'name' field",
		})
	}

	dm.logger.Debugf("KRO validation: kindOK=%v, metadataOK=%v, nameOK=%v",
		isValidKind, metaFound, hasName)

	return diagnostics
}

func isKroRGD(lines []string) bool {
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "apiVersion:") && strings.Contains(trimmed, "kro.run/v1alpha1") {
			return true
		}
	}
	return false
}

func parseKind(lines []string) (int, bool) {
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "kind:") {
			if strings.Contains(trimmed, "ResourceGraphDefinition") {
				return i, true
			}
			return i, false
		}
	}
	return -1, false
}

func parseMetadata(lines []string) (found bool, metaLine int, hasName bool, nameLine int) {
	var indent int
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "metadata:") {
			found = true
			metaLine = i
			indent = len(line) - len(strings.TrimLeft(line, " "))
			break
		}
	}

	if !found {
		return false, -1, false, -1
	}

	for i := metaLine + 1; i < len(lines); i++ {
		line := lines[i]
		currentIndent := len(line) - len(strings.TrimLeft(line, " "))
		trimmed := strings.TrimSpace(line)

		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if currentIndent <= indent {
			break
		}
		if strings.HasPrefix(trimmed, "name:") {
			nameValue := strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
			if nameValue != "" {
				return true, metaLine, true, i
			}
			return true, metaLine, false, i
		}
	}

	return true, metaLine, false, -1
}

func newRange(startLine, startChar, endLine, endChar int) protocol.Range {
	return protocol.Range{
		Start: protocol.Position{Line: uint32(startLine), Character: uint32(startChar)},
		End:   protocol.Position{Line: uint32(endLine), Character: uint32(endChar)},
	}
}

func severityPtr(sev protocol.DiagnosticSeverity) *protocol.DiagnosticSeverity {
	return &sev
}
