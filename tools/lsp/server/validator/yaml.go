package validator

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

type YAMLParser struct {
	content string
}

// YAMLField contains a field's value and its position in the document
type YAMLField struct {
	Value   interface{}
	Line    int
	Column  int
	EndLine int
	EndCol  int
}

// YAMLDocument contains parsed data with position information
type YAMLDocument struct {
	Data      map[string]interface{}
	Positions map[string]YAMLField
}

func NewYAMLParser(content string) *YAMLParser {
	return &YAMLParser{
		content: content,
	}
}

// Parse parses YAML content into a map
func (p *YAMLParser) Parse() (map[string]interface{}, error) {
	var data map[string]interface{}
	err := yaml.Unmarshal([]byte(p.content), &data)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// ParseWithPositions parses YAML and keeps track of field positions
func (p *YAMLParser) ParseWithPositions() (*YAMLDocument, error) {
	var node yaml.Node
	err := yaml.Unmarshal([]byte(p.content), &node)
	if err != nil {
		return nil, err
	}

	// The actual document is the first content node
	if len(node.Content) == 0 {
		return nil, fmt.Errorf("empty YAML document")
	}

	docNode := node.Content[0]
	if docNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping as root node")
	}

	doc := &YAMLDocument{
		Data:      make(map[string]interface{}),
		Positions: make(map[string]YAMLField),
	}

	// Process mapping node pairs (key-value)
	for i := 0; i < len(docNode.Content); i += 2 {
		if i+1 >= len(docNode.Content) {
			break
		}

		keyNode := docNode.Content[i]
		valueNode := docNode.Content[i+1]

		if keyNode.Kind != yaml.ScalarNode {
			continue
		}

		key := keyNode.Value
		var value interface{}

		// Extract based on node kind
		switch valueNode.Kind {
		case yaml.ScalarNode:
			value = valueNode.Value
		case yaml.MappingNode, yaml.SequenceNode:
			// For complex types, just use a placeholder for now
			// In a real implementation, you'd recursively process these
			value = map[string]interface{}{}
			err := valueNode.Decode(&value)
			if err != nil {
				return nil, err
			}
		}

		// Store in both data and positions
		doc.Data[key] = value
		doc.Positions[key] = YAMLField{
			Value:   value,
			Line:    keyNode.Line - 1, // Convert to 0-based
			Column:  keyNode.Column - 1,
			EndLine: valueNode.Line - 1,
			EndCol:  valueNode.Column + len(valueNode.Value),
		}
	}

	return doc, nil
}
