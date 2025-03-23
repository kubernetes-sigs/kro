package validator

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// YAMLParser handles parsing and position tracking for YAML documents
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
	NestedFields map[string]YAMLField // Tracks nested fields like metadata.name
}

// creates a new parser instance
func NewYAMLParser(content string) *YAMLParser {
	return &YAMLParser{
		content: content,
	}
}

// Parse converts YAML content into a map
func (p *YAMLParser) Parse() (map[string]interface{}, error) {
	var data map[string]interface{}
	err := yaml.Unmarshal([]byte(p.content), &data)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// ParseWithPositions parses YAML and tracks field positions
func (p *YAMLParser) ParseWithPositions() (*YAMLDocument, error) {
	var node yaml.Node
	err := yaml.Unmarshal([]byte(p.content), &node)
	if err != nil {
		return nil, err
	}

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

		switch valueNode.Kind {
		case yaml.ScalarNode:
			value = valueNode.Value
		case yaml.MappingNode:
			value = map[string]interface{}{}
			err := valueNode.Decode(&value)
			if err != nil {
				return nil, err
			}

			processNestedFields(doc, valueNode, key, keyNode.Line-1)

			if key == "spec" {
				findAndProcessSchemaNode(doc, valueNode, "spec")
				findAndProcessResourcesNode(doc, valueNode, "spec")
			}
		case yaml.SequenceNode:
			value = []interface{}{}
			err := valueNode.Decode(&value)
			if err != nil {
				return nil, err
			}

			if key == "resources" {
				processArrayElements(doc, valueNode, "spec.resources")
			}
		}

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

// extracts position information for nested fields
func processNestedFields(doc *YAMLDocument, node *yaml.Node, prefix string, parentLine int) {
	if node.Kind != yaml.MappingNode {
		return
	}

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

		switch valueNode.Kind {
		case yaml.ScalarNode:
			value = valueNode.Value
		default:
			value = make(map[string]interface{})
			valueNode.Decode(&value)
		}

		doc.NestedFields[nestedKey] = YAMLField{
			Value:   value,
			Line:    keyNode.Line - 1,
			Column:  keyNode.Column - 1,
			EndLine: valueNode.Line - 1,
			EndCol:  valueNode.Column + len(valueNode.Value),
		}

		if valueNode.Kind == yaml.MappingNode {
			processNestedFields(doc, valueNode, nestedKey, keyNode.Line-1)
		}
	}
}

// locates the schema node in spec and processes its fields
func findAndProcessSchemaNode(doc *YAMLDocument, specNode *yaml.Node, prefix string) {
	for i := 0; i < len(specNode.Content); i += 2 {
		if i+1 >= len(specNode.Content) {
			break
		}

		keyNode := specNode.Content[i]
		valueNode := specNode.Content[i+1]

		if keyNode.Kind != yaml.ScalarNode {
			continue
		}

		if keyNode.Value == "schema" && valueNode.Kind == yaml.MappingNode {
			processSchemaNode(doc, valueNode, prefix+".schema")
			break
		}
	}
}

// processes schema contents including spec and status fields
func processSchemaNode(doc *YAMLDocument, schemaNode *yaml.Node, prefix string) {
	for i := 0; i < len(schemaNode.Content); i += 2 {
		if i+1 >= len(schemaNode.Content) {
			break
		}

		keyNode := schemaNode.Content[i]
		valueNode := schemaNode.Content[i+1]

		if keyNode.Kind != yaml.ScalarNode {
			continue
		}

		if (keyNode.Value == "spec" || keyNode.Value == "status") && valueNode.Kind == yaml.MappingNode {
			sectionPrefix := prefix + "." + keyNode.Value
			processSchemaFields(doc, valueNode, sectionPrefix)
		}
	}
}

// records exact positions of schema fields
func processSchemaFields(doc *YAMLDocument, fieldNode *yaml.Node, prefix string) {
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

		switch valueNode.Kind {
		case yaml.ScalarNode:
			fieldValue = valueNode.Value
		case yaml.MappingNode:
			var mapValue map[string]interface{}
			valueNode.Decode(&mapValue)
			fieldValue = mapValue

			field := YAMLField{
				Value:   fieldValue,
				Line:    keyNode.Line - 1,
				Column:  keyNode.Column - 1,
				EndLine: valueNode.Line - 1,
				EndCol:  valueNode.Column + len(valueNode.Value),
			}
			doc.NestedFields[nestedKey] = field

			processSchemaFields(doc, valueNode, nestedKey)
			continue
		case yaml.SequenceNode:
			var arrayValue []interface{}
			valueNode.Decode(&arrayValue)
			fieldValue = arrayValue
		}

		doc.NestedFields[nestedKey] = YAMLField{
			Value:   fieldValue,
			Line:    keyNode.Line - 1,
			Column:  keyNode.Column - 1,
			EndLine: valueNode.Line - 1,
			EndCol:  valueNode.Column + len(valueNode.Value),
		}
	}
}

// locates and processes resource elements in the spec node
func findAndProcessResourcesNode(doc *YAMLDocument, specNode *yaml.Node, prefix string) {
	for i := 0; i < len(specNode.Content); i += 2 {
		if i+1 >= len(specNode.Content) {
			break
		}

		keyNode := specNode.Content[i]
		valueNode := specNode.Content[i+1]

		if keyNode.Kind != yaml.ScalarNode {
			continue
		}

		if keyNode.Value == "resources" && valueNode.Kind == yaml.SequenceNode {
			processArrayElements(doc, valueNode, prefix+".resources")
			break
		}
	}
}

// tracks positions of array elements and their fields
func processArrayElements(doc *YAMLDocument, arrayNode *yaml.Node, prefix string) {
	// Track array position
	doc.NestedFields[prefix] = YAMLField{
		Value:   nil,
		Line:    arrayNode.Line - 1,
		Column:  arrayNode.Column - 1,
		EndLine: arrayNode.Line - 1,
		EndCol:  arrayNode.Column + 10,
	}

	for i, elemNode := range arrayNode.Content {
		elemPath := fmt.Sprintf("%s[%d]", prefix, i)

		doc.NestedFields[elemPath] = YAMLField{
			Value:   nil,
			Line:    elemNode.Line - 1,
			Column:  elemNode.Column - 1,
			EndLine: elemNode.Line - 1,
			EndCol:  elemNode.Column + 10,
		}

		if elemNode.Kind == yaml.MappingNode {
			for j := 0; j < len(elemNode.Content); j += 2 {
				if j+1 >= len(elemNode.Content) {
					break
				}

				fieldKeyNode := elemNode.Content[j]
				fieldValueNode := elemNode.Content[j+1]

				if fieldKeyNode.Kind != yaml.ScalarNode {
					continue
				}

				fieldKey := fieldKeyNode.Value
				fieldPath := fmt.Sprintf("%s.%s", elemPath, fieldKey)

				var fieldValue interface{}
				switch fieldValueNode.Kind {
				case yaml.ScalarNode:
					fieldValue = fieldValueNode.Value
				case yaml.MappingNode:
					mapValue := map[string]interface{}{}
					fieldValueNode.Decode(&mapValue)
					fieldValue = mapValue

					if fieldKey == "template" {
						processResourceTemplate(doc, fieldValueNode, fieldPath)
					}
				}

				doc.NestedFields[fieldPath] = YAMLField{
					Value:   fieldValue,
					Line:    fieldKeyNode.Line - 1,
					Column:  fieldKeyNode.Column - 1,
					EndLine: fieldValueNode.Line - 1,
					EndCol:  fieldValueNode.Column + 10,
				}
			}
		}
	}
}

// handles Kubernetes resource template fields
func processResourceTemplate(doc *YAMLDocument, templateNode *yaml.Node, prefix string) {
	for i := 0; i < len(templateNode.Content); i += 2 {
		if i+1 >= len(templateNode.Content) {
			break
		}

		keyNode := templateNode.Content[i]
		valueNode := templateNode.Content[i+1]

		if keyNode.Kind != yaml.ScalarNode {
			continue
		}

		key := keyNode.Value
		fieldPath := fmt.Sprintf("%s.%s", prefix, key)

		var fieldValue interface{}
		switch valueNode.Kind {
		case yaml.ScalarNode:
			fieldValue = valueNode.Value
		case yaml.MappingNode:
			mapValue := map[string]interface{}{}
			valueNode.Decode(&mapValue)
			fieldValue = mapValue

			if key == "metadata" {
				processNestedFields(doc, valueNode, fieldPath, keyNode.Line-1)
			}
		}

		doc.NestedFields[fieldPath] = YAMLField{
			Value:   fieldValue,
			Line:    keyNode.Line - 1,
			Column:  keyNode.Column - 1,
			EndLine: valueNode.Line - 1,
			EndCol:  valueNode.Column + 10,
		}
	}
}
