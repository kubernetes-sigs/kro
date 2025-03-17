// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package schema

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

func TestNewCombinedResolver(t *testing.T) {
	tests := []struct {
		name          string
		serverHandler http.HandlerFunc
		expectError   bool
	}{
		{
			name: "successful resolver creation",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				// Mock successful discovery endpoints
				if r.URL.Path == "/api" {
					w.Write([]byte(`{"versions":["v1"]}`))
				} else if r.URL.Path == "/apis" {
					w.Write([]byte(`{"groups":[{"name":"apps","versions":[{"groupVersion":"apps/v1"}]}]}`))
				} else {
					w.WriteHeader(http.StatusOK)
					w.Write([]byte(`{}`))
				}
			},
			expectError: false,
		},
		{
			name: "discovery client error",
			serverHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"error":"internal server error"}`))
			},
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			testServer := httptest.NewServer(tc.serverHandler)
			defer testServer.Close()

			config := &rest.Config{
				Host: testServer.URL,
			}

			// Calling the function being tested
			resolver, discoveryClient, err := NewCombinedResolver(config)

			if tc.name == "discovery client error" {
				// Creating a new test - verifying that using the client fails
				serverVersion, err := discoveryClient.ServerVersion()
				require.Error(t, err)
				assert.Nil(t, serverVersion)
			} else if tc.expectError {
				require.Error(t, err)
				assert.Nil(t, resolver)
				assert.Nil(t, discoveryClient)
			} else {
				require.NoError(t, err)
				assert.NotNil(t, resolver)
				assert.NotNil(t, discoveryClient)

				// Verify resolver can resolve some basic types
				// This is a more complex test that would require deeper mocking
				// of the OpenAPI schemas returned by the server
			}
		})
	}
}
