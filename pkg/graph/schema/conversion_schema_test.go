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

package schema

import (
	"testing"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestConvertJSONSchemaPropsToSpecSchema_ExternalDocs(t *testing.T) {
	props := &extv1.JSONSchemaProps{
		Type: "string",
		ExternalDocs: &extv1.ExternalDocumentation{
			URL:         "https://example.com/docs",
			Description: "See documentation",
		},
	}

	result, err := ConvertJSONSchemaPropsToSpecSchema(props)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.ExternalDocs == nil {
		t.Fatal("expected ExternalDocs to be set")
	}
	if result.ExternalDocs.URL != "https://example.com/docs" {
		t.Errorf("got URL %q, want %q", result.ExternalDocs.URL, "https://example.com/docs")
	}
}
