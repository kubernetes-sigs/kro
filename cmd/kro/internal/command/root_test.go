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

	assert.Equal(t, "kroctl", cmd.Use)
	assert.NotEmpty(t, cmd.Short)
	assert.NotEmpty(t, cmd.Long)
	assert.NotEmpty(t, cmd.Version)
	assert.True(t, cmd.SilenceUsage)
	assert.True(t, cmd.SilenceErrors)
	assert.True(t, cmd.CompletionOptions.DisableDefaultCmd)
}

func TestNewRootCommand_HasJSONFlag(t *testing.T) {
	cmd := command.NewRootCommand()

	flag := cmd.PersistentFlags().Lookup("json")
	assert.NotNil(t, flag)
	assert.Equal(t, "false", flag.DefValue)
	assert.Equal(t, "Output in JSON format", flag.Usage)
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
	assert.Contains(t, buf.String(), "kroctl")
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
