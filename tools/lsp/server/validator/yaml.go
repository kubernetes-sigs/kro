package validator

import (
	"gopkg.in/yaml.v3"
)

type YAMLParser struct {
	content string
}

func NewYAMLParser(content string) *YAMLParser {
	return &YAMLParser{
		content: content,
	}
}

func (p *YAMLParser) Parse() (map[string]interface{}, error) {
	var data map[string]interface{}
	err := yaml.Unmarshal([]byte(p.content), &data)
	if err != nil {
		return nil, err
	}
	return data, nil
}
