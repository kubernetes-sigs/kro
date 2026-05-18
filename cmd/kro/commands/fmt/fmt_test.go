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

package fmt

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	// Silence formatted output in tests
	os.Setenv("KROFMT_QUIET", "1")
}

func TestFormatYAML(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name: "preserves comments",
			input: `# This is a header comment
apiVersion: v1
kind: Pod
metadata:
  name: test  # inline comment
`,
			expected: `# This is a header comment
apiVersion: v1
kind: Pod
metadata:
  name: test # inline comment
`,
		},
		{
			name: "enforces 2-space indentation",
			input: `apiVersion: v1
kind: Pod
metadata:
    name: test
    labels:
        app: demo
`,
			expected: `apiVersion: v1
kind: Pod
metadata:
  name: test
  labels:
    app: demo
`,
		},
		{
			name: "converts flow style to block style",
			input: `metadata: {name: test, namespace: default}
`,
			expected: `metadata:
  name: test
  namespace: default
`,
		},
		{
			name: "preserves key order",
			input: `zulu: last
alpha: first
bravo: second
`,
			expected: `zulu: last
alpha: first
bravo: second
`,
		},
		{
			name: "formats nested arrays",
			input: `spec:
  resources:
  - id: deployment
    template:
      spec:
        containers:
        - name: web
          ports:
          - containerPort: 80
`,
			expected: `spec:
  resources:
    - id: deployment
      template:
        spec:
          containers:
            - name: web
              ports:
                - containerPort: 80
`,
		},
		{
			name: "handles multi-document YAML",
			input: `apiVersion: v1
kind: ConfigMap
metadata:
  name: config1
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: config2
`,
			expected: `apiVersion: v1
kind: ConfigMap
metadata:
  name: config1
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: config2
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := &bytes.Buffer{}
			err := formatYAML(strings.NewReader(tt.input), out)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, out.String())
		})
	}
}

func TestFormatYAMLInvalid(t *testing.T) {
	// Test truly invalid YAML that yaml.v3 will reject
	input := `apiVersion: v1
kind: Pod
metadata:
  name: test
  labels: {unclosed
`

	out := &bytes.Buffer{}
	err := formatYAML(strings.NewReader(input), out)
	assert.Error(t, err, "should fail on invalid YAML")
}

func TestFormatFile(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		checkOnly   bool
		inPlace     bool
		wantChanged bool
		wantError   bool
	}{
		{
			name: "format unformatted file in-place",
			input: `apiVersion: v1
kind: Pod
metadata:
    name: test
`,
			checkOnly:   false,
			inPlace:     true,
			wantChanged: true,
			wantError:   false,
		},
		{
			name: "check mode detects formatting needed",
			input: `apiVersion: v1
kind: Pod
metadata:
    name: test
`,
			checkOnly:   true,
			inPlace:     false,
			wantChanged: true,
			wantError:   false,
		},
		{
			name: "check mode detects no changes needed",
			input: `apiVersion: v1
kind: Pod
metadata:
  name: test
`,
			checkOnly:   true,
			inPlace:     false,
			wantChanged: false,
			wantError:   false,
		},
		{
			name: "handles invalid YAML",
			input: `apiVersion: v1
kind: Pod
metadata: {
`,
			checkOnly:   false,
			inPlace:     false,
			wantChanged: false,
			wantError:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			filePath := filepath.Join(tmpDir, "test.yaml")

			err := os.WriteFile(filePath, []byte(tt.input), 0644)
			require.NoError(t, err)

			changed, err := formatFile(filePath, tt.checkOnly, tt.inPlace)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			assert.Equal(t, tt.wantChanged, changed)

			if tt.inPlace && !tt.wantError {
				content, err := os.ReadFile(filePath)
				require.NoError(t, err)

				if tt.wantChanged {
					// Verify file was actually modified
					assert.NotEqual(t, tt.input, string(content))
					// Verify it's properly formatted (2-space indent)
					assert.Contains(t, string(content), "  name: test")
				} else {
					// Verify file unchanged
					assert.Equal(t, tt.input, string(content))
				}
			}
		})
	}
}

func TestFormatFileAtomic(t *testing.T) {
	// Test that in-place writes are atomic (temp file + rename)
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.yaml")

	input := `apiVersion: v1
kind: Pod
metadata:
    name: test
`
	err := os.WriteFile(filePath, []byte(input), 0644)
	require.NoError(t, err)

	// Format in-place
	changed, err := formatFile(filePath, false, true)
	require.NoError(t, err)
	assert.True(t, changed)

	// Verify no temp files left behind
	files, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	assert.Len(t, files, 1, "should only have the original file, no temp files")
	assert.Equal(t, "test.yaml", files[0].Name())
}

func TestSetBlockStyle(t *testing.T) {
	// Test that flow style is converted to block style
	input := `metadata: {name: test, labels: {app: demo, env: prod}}
spec: {replicas: 3}
`
	expected := `metadata:
  name: test
  labels:
    app: demo
    env: prod
spec:
  replicas: 3
`

	out := &bytes.Buffer{}
	err := formatYAML(strings.NewReader(input), out)
	require.NoError(t, err)
	assert.Equal(t, expected, out.String())
}

func TestFormatFilePreservesPermissions(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.yaml")

	input := `apiVersion: v1
kind: Pod
metadata:
    name: test
`
	// Write with specific permissions
	err := os.WriteFile(filePath, []byte(input), 0600)
	require.NoError(t, err)

	// Format in-place
	_, err = formatFile(filePath, false, true)
	require.NoError(t, err)

	// Check permissions are preserved
	info, err := os.Stat(filePath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

func TestFormatYAMLIdempotent(t *testing.T) {
	// Formatting twice should produce the same result
	input := `apiVersion: v1
kind: Pod
metadata:
    name: test
    labels:
        app: demo
`

	// First format
	out1 := &bytes.Buffer{}
	err := formatYAML(strings.NewReader(input), out1)
	require.NoError(t, err)

	// Second format
	out2 := &bytes.Buffer{}
	err = formatYAML(strings.NewReader(out1.String()), out2)
	require.NoError(t, err)

	// Should be identical
	assert.Equal(t, out1.String(), out2.String())
}
