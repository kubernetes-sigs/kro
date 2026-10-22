package simpleschema

import (
	"encoding/json"
	"testing"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// A string default containing quotes (JSON-in-string) must survive as valid
// JSON in the CRD schema, not crash CRD creation downstream.
func TestDefaultMarkerStringWithQuotes(t *testing.T) {
	spec, err := ToOpenAPISpec(map[string]any{
		"spec": map[string]any{"services": `string | default="["keycloak"]"`},
	}, nil)
	if err != nil {
		t.Fatalf("ToOpenAPISpec: %v", err)
	}
	var s extv1.JSONSchemaProps = *spec
	f := s.Properties["spec"].Properties["services"]
	if f.Default == nil {
		t.Fatal("no default")
	}
	if !json.Valid(f.Default.Raw) {
		t.Fatalf("Default.Raw is not valid JSON: %s", f.Default.Raw)
	}
}
