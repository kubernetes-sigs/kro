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

package resolver

import (
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	openapiresolver "k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

var testGVK = schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Foo"}

func dummySchema(typ string) *spec.Schema {
	return &spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{typ}},
	}
}

type staticResolver struct{ schema *spec.Schema }

func (r *staticResolver) ResolveSchema(_ schema.GroupVersionKind) (*spec.Schema, error) {
	return r.schema, nil
}

type notFoundResolver struct{}

func (r *notFoundResolver) ResolveSchema(_ schema.GroupVersionKind) (*spec.Schema, error) {
	return nil, openapiresolver.ErrSchemaNotFound
}

type errorResolver struct{ err error }

func (r *errorResolver) ResolveSchema(_ schema.GroupVersionKind) (*spec.Schema, error) {
	return nil, r.err
}

func TestChainedResolver(t *testing.T) {
	first := dummySchema("first")
	second := dummySchema("second")
	realErr := errors.New("connection refused")

	tests := []struct {
		name      string
		resolvers []openapiresolver.SchemaResolver
		want      *spec.Schema
		wantErr   error
	}{
		{
			name:      "first resolver wins",
			resolvers: []openapiresolver.SchemaResolver{&staticResolver{schema: first}, &staticResolver{schema: second}},
			want:      first,
		},
		{
			name:      "falls through on not found",
			resolvers: []openapiresolver.SchemaResolver{&notFoundResolver{}, &staticResolver{schema: second}},
			want:      second,
		},
		{
			name:      "all not found",
			resolvers: []openapiresolver.SchemaResolver{&notFoundResolver{}, &notFoundResolver{}},
			wantErr:   openapiresolver.ErrSchemaNotFound,
		},
		{
			name:      "stops on real error",
			resolvers: []openapiresolver.SchemaResolver{&errorResolver{err: realErr}, &staticResolver{schema: second}},
			wantErr:   realErr,
		},
		{
			name:      "empty chain",
			resolvers: nil,
			wantErr:   openapiresolver.ErrSchemaNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewChainedResolver(tt.resolvers...)
			got, err := r.ResolveSchema(testGVK)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err: got %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatal("unexpected schema pointer")
			}
		})
	}
}
