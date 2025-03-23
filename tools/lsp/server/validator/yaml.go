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
	Data         map[string]interface{}
	Positions    map[string]YAMLField
	NestedFields map[string]YAMLField // For tracking fields like metadata.name
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
		Data:         make(map[string]interface{}),
		Positions:    make(map[string]YAMLField),
		NestedFields: make(map[string]YAMLField),
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
		case yaml.MappingNode:
			// For complex types like metadata, process nested fields
			value = map[string]interface{}{}
			err := valueNode.Decode(&value)
			if err != nil {
				return nil, err
			}

			// Process nested fields and attach position information
			processNestedFields(doc, valueNode, key, keyNode.Line-1)

			// Special handling for schema section
			if key == "spec" {
				// Look for spec.schema and process schema fields with precise positions
				findAndProcessSchemaNode(doc, valueNode, "spec")
			}
		case yaml.SequenceNode:
			// For arrays/lists
			value = []interface{}{}
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

// processNestedFields extracts position information for nested fields
func processNestedFields(doc *YAMLDocument, node *yaml.Node, prefix string, parentLine int) {
	if node.Kind != yaml.MappingNode {
		return
	}

	// Process each key-value pair in the mapping
	for i := 0; i < len(node.Content); i += 2 {
		if i+1 >= len(node.Content) {
			break
		}

		keyNode := node.Content[i]
		valueNode := node.Content[i+1]

		if keyNode.Kind != yaml.ScalarNode {
			continue
		}

		nestedKey := prefix + "." + keyNode.Value
		var value interface{}

		// Extract value based on node kind
		switch valueNode.Kind {
		case yaml.ScalarNode:
			value = valueNode.Value
		default:
			// For complex types, just get a placeholder
			value = make(map[string]interface{})
			valueNode.Decode(&value)
		}

		// Store the nested field position
		doc.NestedFields[nestedKey] = YAMLField{
			Value:   value,
			Line:    keyNode.Line - 1, // Convert to 0-based
			Column:  keyNode.Column - 1,
			EndLine: valueNode.Line - 1,
			EndCol:  valueNode.Column + len(valueNode.Value),
		}

		// Recursively process this node if it's a mapping
		if valueNode.Kind == yaml.MappingNode {
			processNestedFields(doc, valueNode, nestedKey, keyNode.Line-1)
		}
	}
}

// findAndProcessSchemaNode searches for the schema node in spec and processes its fields with positions
func findAndProcessSchemaNode(doc *YAMLDocument, specNode *yaml.Node, prefix string) {
	// We need to find the "schema" key in the spec node
	for i := 0; i < len(specNode.Content); i += 2 {
		if i+1 >= len(specNode.Content) {
			break
		}

		keyNode := specNode.Content[i]
		valueNode := specNode.Content[i+1]

		if keyNode.Kind != yaml.ScalarNode {
			continue
		}

		// Found schema key
		if keyNode.Value == "schema" && valueNode.Kind == yaml.MappingNode {
			// Process schema fields with positions
			processSchemaNode(doc, valueNode, prefix+".schema")
			break
		}
	}
}

// processSchemaNode processes schema contents including spec and status fields with positions
func processSchemaNode(doc *YAMLDocument, schemaNode *yaml.Node, prefix string) {
	// Process schema node pairs to find spec and status
	for i := 0; i < len(schemaNode.Content); i += 2 {
		if i+1 >= len(schemaNode.Content) {
			break
		}

		keyNode := schemaNode.Content[i]
		valueNode := schemaNode.Content[i+1]

		if keyNode.Kind != yaml.ScalarNode {
			continue
		}

		// We found either spec or status in schema
		if (keyNode.Value == "spec" || keyNode.Value == "status") && valueNode.Kind == yaml.MappingNode {
			sectionPrefix := prefix + "." + keyNode.Value
			processSchemaFields(doc, valueNode, sectionPrefix)
		}
	}
}

// processSchemaFields processes schema fields and records their exact positions
func processSchemaFields(doc *YAMLDocument, fieldNode *yaml.Node, prefix string) {
	// Process each key-value pair in the mapping
	for i := 0; i < len(fieldNode.Content); i += 2 {
		if i+1 >= len(fieldNode.Content) {
			break
		}

		keyNode := fieldNode.Content[i]
		valueNode := fieldNode.Content[i+1]

		if keyNode.Kind != yaml.ScalarNode {
			continue
		}

		nestedKey := prefix + "." + keyNode.Value
		var fieldValue interface{}

		// Determine the value based on node kind
		switch valueNode.Kind {
		case yaml.ScalarNode:
			// Store the value for type definitions
			fieldValue = valueNode.Value
		case yaml.MappingNode:
			// For nested structures
			var mapValue map[string]interface{}
			valueNode.Decode(&mapValue)
			fieldValue = mapValue

			// Record the field then process deeper nested fields
			field := YAMLField{
				Value:   fieldValue,
				Line:    keyNode.Line - 1, // Convert to 0-based
				Column:  keyNode.Column - 1,
				EndLine: valueNode.Line - 1,
				EndCol:  valueNode.Column + len(valueNode.Value),
			}
			doc.NestedFields[nestedKey] = field

			// Continue processing deeper nested fields
			processSchemaFields(doc, valueNode, nestedKey)
			continue // Skip the assignment at the end since we already did it
		case yaml.SequenceNode:
			// For arrays
			var arrayValue []interface{}
			valueNode.Decode(&arrayValue)
			fieldValue = arrayValue
		}

		// Create and store the complete field
		field := YAMLField{
			Value:   fieldValue,
			Line:    keyNode.Line - 1, // Convert to 0-based
			Column:  keyNode.Column - 1,
			EndLine: valueNode.Line - 1,
			EndCol:  valueNode.Column + len(valueNode.Value),
		}
		doc.NestedFields[nestedKey] = field
	}
}
