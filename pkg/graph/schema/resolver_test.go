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
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestCombinedResolver(t *testing.T) {
	fakeClient := fakeclientset.NewSimpleClientset()

	// Creating REST client using fake client
	clientConfig := &rest.Config{
		Host: "https://fake.example.com",
		ContentConfig: rest.ContentConfig{
			ContentType: "application/json",
		},
		WrapTransport: func(rt http.RoundTripper) http.RoundTripper {
			return &fakeTransport{
				fakeClient: fakeClient,
			}
		},
	}

	// Testing resolver with valid config
	t.Run("with valid config", func(t *testing.T) {
		resolver, discoveryClient, err := NewCombinedResolver(clientConfig)
		require.NoError(t, err)
		require.NotNil(t, resolver)
		require.NotNil(t, discoveryClient)

		// Checking that we can get some basic info from discoveryClient
		version, err := discoveryClient.ServerVersion()
		require.NoError(t, err)
		assert.NotNil(t, version)
		assert.Equal(t, "1", version.Major)
		assert.Equal(t, "25", version.Minor)
	})

	// Testing with invalid config
	t.Run("with invalid config", func(t *testing.T) {
		// Malformed config that will fail during client creation
		badConfig := &rest.Config{
			Host: "://invalid-host",
		}
		resolver, discoveryClient, err := NewCombinedResolver(badConfig)
		require.Error(t, err)
		assert.Nil(t, resolver)
		assert.Nil(t, discoveryClient)
	})
}

// fakeTransport simulates HTTP transport using fake Kubernetes client
type fakeTransport struct {
	fakeClient *fakeclientset.Clientset
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Simplified responses based on request path
	switch {
	case req.URL.Path == "/version":
		return &http.Response{
			StatusCode: 200,
			Body: io.NopCloser(strings.NewReader(`{
                "major": "1", 
                "minor": "25",
                "gitVersion": "v1.25.0"
            }`)),
			Header: make(http.Header),
		}, nil

	case req.URL.Path == "/api":
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"versions":["v1"]}`)),
			Header:     make(http.Header),
		}, nil

	case req.URL.Path == "/apis":
		return &http.Response{
			StatusCode: 200,
			Body: io.NopCloser(strings.NewReader(`{
                "groups": [
                    {
                        "name": "apps",
                        "versions": [{"groupVersion": "apps/v1"}]
                    }
                ]
            }`)),
			Header: make(http.Header),
		}, nil

	case strings.Contains(req.URL.Path, "/openapi/v3"):
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Header:     make(http.Header),
		}, nil

	default:
		return &http.Response{
			StatusCode: 404,
			Body:       io.NopCloser(strings.NewReader(`{"message": "not found"}`)),
			Header:     make(http.Header),
		}, nil
	}
}
