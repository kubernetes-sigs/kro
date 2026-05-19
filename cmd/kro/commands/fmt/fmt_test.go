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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatFile(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		checkOnly   bool
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

			changed, err := formatFile(filePath, tt.checkOnly)

			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			assert.Equal(t, tt.wantChanged, changed)

			if !tt.checkOnly && !tt.wantError {
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
	changed, err := formatFile(filePath, false)
	require.NoError(t, err)
	assert.True(t, changed)

	// Verify no temp files left behind
	files, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	assert.Len(t, files, 1, "should only have the original file, no temp files")
	assert.Equal(t, "test.yaml", files[0].Name())
}

// TestSetBlockStyle removed - yamlfmt handles this internally

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
	_, err = formatFile(filePath, false)
	require.NoError(t, err)

	// Check permissions are preserved
	info, err := os.Stat(filePath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

func TestFormatYAMLIdempotent(t *testing.T) {
	// Formatting twice should produce the same result
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.yaml")

	input := `apiVersion: v1
kind: Pod
metadata:
    name: test
    labels:
        app: demo
`
	err := os.WriteFile(filePath, []byte(input), 0644)
	require.NoError(t, err)

	// First format
	_, err = formatFile(filePath, false)
	require.NoError(t, err)
	firstFormat, err := os.ReadFile(filePath)
	require.NoError(t, err)

	// Second format
	_, err = formatFile(filePath, false)
	require.NoError(t, err)
	secondFormat, err := os.ReadFile(filePath)
	require.NoError(t, err)

	// Should be identical
	assert.Equal(t, string(firstFormat), string(secondFormat))
}

func TestKnownLimitation_CommentIndentation(t *testing.T) {
	// Known limitation: yamlfmt preserves comment indentation from source,
	// even when comments are indented to match the preceding line instead of
	// their logical position as array item separators.
	//
	// See: https://github.com/google/yamlfmt/issues/255
	//      https://github.com/google/yamlfmt/issues/290
	//
	// Example from examples/aws/data-processor/eda-eks-data-processor.yaml:
	// The comment "# Pod role setup..." is indented to match "name: ${...}"
	// instead of being aligned with "- id: podRole" below it.

	input := `parent:
  key:
    - a: 1
      b: 2
      # Comment indented to match "b: 2" (6 spaces)
    - a: 2
      b: 3
`

	// yamlfmt preserves the incorrect indentation
	expected := input

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.yaml")
	err := os.WriteFile(filePath, []byte(input), 0644)
	require.NoError(t, err)

	_, err = formatFile(filePath, false)
	require.NoError(t, err)

	actual, err := os.ReadFile(filePath)
	require.NoError(t, err)

	// This test documents that yamlfmt preserves source indentation
	// even when it's logically incorrect for array separators
	assert.Equal(t, expected, string(actual),
		"yamlfmt preserves comment indentation from source (known limitation)")
}
