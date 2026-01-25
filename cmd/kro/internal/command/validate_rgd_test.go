// Copyright 2025 The Kube Resource Orchestrator Authors
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

package command_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubernetes-sigs/kro/cmd/kro/internal/command"
	"github.com/kubernetes-sigs/kro/cmd/kro/internal/view"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateRGD_JSONFlag_AfterFileFlag(t *testing.T) {
	// Create a temporary invalid RGD file
	tmpDir := t.TempDir()
	invalidFile := filepath.Join(tmpDir, "invalid.yaml")
	err := os.WriteFile(invalidFile, []byte(`
apiVersion: kro.run/v1alpha1
kind: WrongKind
metadata:
  name: test
`), 0644)
	require.NoError(t, err)

	// Create CLI and command
	buf := new(bytes.Buffer)
	cli := command.NewCLI(view.ViewHuman, buf, view.LogLevelSilent)
	root := command.NewRootCommand()
	command.AddCommands(root, cli)

	// Set up PersistentPreRun to configure viewer (simulating Execute())
	root.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		outputFlag, _ := cmd.Flags().GetString("output")
		viewType, _ := view.ParseOutputFormat(outputFlag)
		s := view.NewStream(buf)
		cli.Viewer = view.NewViewer(viewType, s, view.LogLevelSilent)
		cli.Stream = s
	}

	// Execute with -o json AFTER -f flag
	// Validate command should ignore -o flag and always output human-readable format
	root.SetArgs([]string{"validate", "rgd", "-f", invalidFile, "-o", "json"})
	root.SetOut(buf)
	root.SetErr(buf)

	err = root.Execute()
	assert.Error(t, err) // Should error on invalid file

	// Verify output is human-readable (not JSON)
	output := buf.String()
	assert.Contains(t, output, "Error!")
	assert.Contains(t, output, "expected kind ResourceGraphDefinition")

	// Should NOT be valid JSON
	var result map[string]interface{}
	err = json.Unmarshal([]byte(output), &result)
	assert.Error(t, err, "Output should NOT be valid JSON, got: %s", output)
}

func TestValidateRGD_JSONFlag_BeforeFileFlag(t *testing.T) {
	// Create a temporary invalid RGD file
	tmpDir := t.TempDir()
	invalidFile := filepath.Join(tmpDir, "invalid.yaml")
	err := os.WriteFile(invalidFile, []byte(`
apiVersion: kro.run/v1alpha1
kind: WrongKind
metadata:
  name: test
`), 0644)
	require.NoError(t, err)

	// Create CLI and command
	buf := new(bytes.Buffer)
	cli := command.NewCLI(view.ViewHuman, buf, view.LogLevelSilent)
	root := command.NewRootCommand()
	command.AddCommands(root, cli)

	// Set up PersistentPreRun to configure viewer
	root.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		outputFlag, _ := cmd.Flags().GetString("output")
		viewType, _ := view.ParseOutputFormat(outputFlag)
		s := view.NewStream(buf)
		cli.Viewer = view.NewViewer(viewType, s, view.LogLevelSilent)
		cli.Stream = s
	}

	// Execute with -o json BEFORE -f flag
	// Validate command should ignore -o flag and always output human-readable format
	root.SetArgs([]string{"validate", "rgd", "-o", "json", "-f", invalidFile})
	root.SetOut(buf)
	root.SetErr(buf)

	err = root.Execute()
	assert.Error(t, err)

	// Verify output is human-readable (not JSON)
	output := buf.String()
	assert.Contains(t, output, "Error!")
	assert.Contains(t, output, "expected kind ResourceGraphDefinition")

	// Should NOT be valid JSON
	var result map[string]interface{}
	err = json.Unmarshal([]byte(output), &result)
	assert.Error(t, err, "Output should NOT be valid JSON, got: %s", output)
}

func TestValidateRGD_JSONFlag_BeforeSubcommand(t *testing.T) {
	// Create a temporary invalid RGD file
	tmpDir := t.TempDir()
	invalidFile := filepath.Join(tmpDir, "invalid.yaml")
	err := os.WriteFile(invalidFile, []byte(`
apiVersion: kro.run/v1alpha1
kind: WrongKind
metadata:
  name: test
`), 0644)
	require.NoError(t, err)

	// Create CLI and command
	buf := new(bytes.Buffer)
	cli := command.NewCLI(view.ViewHuman, buf, view.LogLevelSilent)
	root := command.NewRootCommand()
	command.AddCommands(root, cli)

	// Set up PersistentPreRun to configure viewer
	root.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		outputFlag, _ := cmd.Flags().GetString("output")
		viewType, _ := view.ParseOutputFormat(outputFlag)
		s := view.NewStream(buf)
		cli.Viewer = view.NewViewer(viewType, s, view.LogLevelSilent)
		cli.Stream = s
	}

	// Execute with -o json at the very beginning
	// Validate command should ignore -o flag and always output human-readable format
	root.SetArgs([]string{"-o", "json", "validate", "rgd", "-f", invalidFile})
	root.SetOut(buf)
	root.SetErr(buf)

	err = root.Execute()
	assert.Error(t, err)

	// Verify output is human-readable (not JSON)
	output := buf.String()
	assert.Contains(t, output, "Error!")
	assert.Contains(t, output, "expected kind ResourceGraphDefinition")

	// Should NOT be valid JSON
	var result map[string]interface{}
	err = json.Unmarshal([]byte(output), &result)
	assert.Error(t, err, "Output should NOT be valid JSON, got: %s", output)
}

func TestValidateRGD_NoJSONFlag_HumanOutput(t *testing.T) {
	// Create a temporary invalid RGD file
	tmpDir := t.TempDir()
	invalidFile := filepath.Join(tmpDir, "invalid.yaml")
	err := os.WriteFile(invalidFile, []byte(`
apiVersion: kro.run/v1alpha1
kind: WrongKind
metadata:
  name: test
`), 0644)
	require.NoError(t, err)

	// Create CLI and command
	buf := new(bytes.Buffer)
	cli := command.NewCLI(view.ViewHuman, buf, view.LogLevelSilent)
	root := command.NewRootCommand()
	command.AddCommands(root, cli)

	// Set up PersistentPreRun to configure viewer
	root.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		outputFlag, _ := cmd.Flags().GetString("output")
		viewType, _ := view.ParseOutputFormat(outputFlag)
		s := view.NewStream(buf)
		cli.Viewer = view.NewViewer(viewType, s, view.LogLevelSilent)
		cli.Stream = s
	}

	// Execute without -o flag (default human output)
	root.SetArgs([]string{"validate", "rgd", "-f", invalidFile})
	root.SetOut(buf)
	root.SetErr(buf)

	err = root.Execute()
	assert.Error(t, err)

	// Verify output is NOT JSON (human-readable)
	output := buf.String()
	assert.Contains(t, output, "Error!")
	assert.Contains(t, output, "expected kind ResourceGraphDefinition")

	// Should NOT be valid JSON
	var result map[string]interface{}
	err = json.Unmarshal([]byte(output), &result)
	assert.Error(t, err, "Output should NOT be valid JSON, got: %s", output)
}

func TestValidateRGD_ValidFile_JSONOutput(t *testing.T) {
	// Create a temporary valid RGD file
	tmpDir := t.TempDir()
	validFile := filepath.Join(tmpDir, "valid.yaml")
	err := os.WriteFile(validFile, []byte(`
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: test-rgd
spec:
  schema:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: test
`), 0644)
	require.NoError(t, err)

	// Create CLI and command
	buf := new(bytes.Buffer)
	cli := command.NewCLI(view.ViewHuman, buf, view.LogLevelSilent)
	root := command.NewRootCommand()
	command.AddCommands(root, cli)

	// Set up PersistentPreRun to configure viewer
	root.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		outputFlag, _ := cmd.Flags().GetString("output")
		viewType, _ := view.ParseOutputFormat(outputFlag)
		s := view.NewStream(buf)
		cli.Viewer = view.NewViewer(viewType, s, view.LogLevelSilent)
		cli.Stream = s
	}

	// Execute with -o json flag
	// Validate command should ignore -o flag and always output human-readable format
	root.SetArgs([]string{"validate", "rgd", "-f", validFile, "-o", "json"})
	root.SetOut(buf)
	root.SetErr(buf)

	err = root.Execute()

	// Check output format if there's any output
	output := buf.String()
	if strings.TrimSpace(output) != "" {
		// Should NOT be valid JSON
		var result map[string]interface{}
		jsonErr := json.Unmarshal([]byte(output), &result)
		assert.Error(t, jsonErr, "Output should NOT be valid JSON in human mode, got: %s", output)
	}
}

func TestValidateRGD_ValidFile_HumanOutput(t *testing.T) {
	// Create a temporary valid RGD file
	tmpDir := t.TempDir()
	validFile := filepath.Join(tmpDir, "valid.yaml")
	err := os.WriteFile(validFile, []byte(`
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: test-rgd
spec:
  schema:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: test
`), 0644)
	require.NoError(t, err)

	// Create CLI and command
	buf := new(bytes.Buffer)
	cli := command.NewCLI(view.ViewHuman, buf, view.LogLevelSilent)
	root := command.NewRootCommand()
	command.AddCommands(root, cli)

	// Set up PersistentPreRun to configure viewer
	root.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		outputFlag, _ := cmd.Flags().GetString("output")
		viewType, _ := view.ParseOutputFormat(outputFlag)
		s := view.NewStream(buf)
		cli.Viewer = view.NewViewer(viewType, s, view.LogLevelSilent)
		cli.Stream = s
	}

	// Execute without -o flag (default human output)
	root.SetArgs([]string{"validate", "rgd", "-f", validFile})
	root.SetOut(buf)
	root.SetErr(buf)

	err = root.Execute()

	// Check output format if there's any output
	output := buf.String()
	if strings.TrimSpace(output) != "" {
		// Should NOT be valid JSON
		var result map[string]interface{}
		jsonErr := json.Unmarshal([]byte(output), &result)
		assert.Error(t, jsonErr, "Output should NOT be valid JSON in human mode, got: %s", output)
	}
}
