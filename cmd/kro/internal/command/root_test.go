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
	"testing"

	"github.com/kubernetes-sigs/kro/cmd/kro/internal/command"
	"github.com/kubernetes-sigs/kro/cmd/kro/internal/view"
	"github.com/stretchr/testify/assert"
)

func TestNewRootCommand(t *testing.T) {
	cmd := command.NewRootCommand()

	assert.Equal(t, "kro", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	assert.NotEmpty(t, cmd.Long)
	assert.NotEmpty(t, cmd.Version)
	assert.True(t, cmd.SilenceUsage)
	assert.True(t, cmd.SilenceErrors)
	assert.True(t, cmd.CompletionOptions.DisableDefaultCmd)
}

func TestNewRootCommand_HasOutputFlag(t *testing.T) {
	cmd := command.NewRootCommand()

	flag := cmd.PersistentFlags().Lookup("output")
	assert.NotNil(t, flag)
	assert.Equal(t, "", flag.DefValue)
	assert.Equal(t, "Output format. One of: (json | yaml)", flag.Usage)

	// Check that the short flag is also available
	shortFlag := cmd.PersistentFlags().ShorthandLookup("o")
	assert.NotNil(t, shortFlag)
	assert.Equal(t, flag, shortFlag)
}

func TestNewRootCommand_VersionFlag(t *testing.T) {
	cmd := command.NewRootCommand()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--version"})

	err := cmd.Execute()
	assert.NoError(t, err)
	assert.Contains(t, buf.String(), cmd.Version)
}

func TestNewRootCommand_NoArgs_ShowsHelp(t *testing.T) {
	cmd := command.NewRootCommand()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	assert.NoError(t, err)
	assert.Contains(t, buf.String(), "kro")
}

func TestAddCommands(t *testing.T) {
	cli := command.NewCLI(view.ViewHuman, &bytes.Buffer{}, view.LogLevelSilent)
	root := command.NewRootCommand()
	command.AddCommands(root, cli)

	expectedCommands := []string{"apply", "delete", "version", "explain", "api-resources", "config", "login"}
	for _, name := range expectedCommands {
		cmd, _, err := root.Find([]string{name})
		assert.NoError(t, err, "command %s should exist", name)
		assert.Equal(t, name, cmd.Name())
	}
}

func TestAddCommands_Count(t *testing.T) {
	cli := command.NewCLI(view.ViewHuman, &bytes.Buffer{}, view.LogLevelSilent)
	root := command.NewRootCommand()
	command.AddCommands(root, cli)

	assert.True(t, root.HasSubCommands())
	assert.Len(t, root.Commands(), 7)
}
