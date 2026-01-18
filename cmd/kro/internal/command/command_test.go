package command_test

import (
	"bytes"
	"testing"

	"github.com/kubernetes-sigs/kro/cmd/kro/internal/command"
	"github.com/kubernetes-sigs/kro/cmd/kro/internal/view"
	"github.com/stretchr/testify/assert"
)

func TestNewCLI_WithHumanView(t *testing.T) {
	cli := command.NewCLI(view.ViewHuman, &bytes.Buffer{}, view.LogLevelSilent)
	assert.NotNil(t, cli.Viewer)
	assert.NotNil(t, cli.Stream)
	assert.IsType(t, &view.HumanView{}, cli.Viewer)
	assert.Equal(t, &bytes.Buffer{}, cli.Writer)
}

func TestNewCLI_WithJSONView(t *testing.T) {
	cli := command.NewCLI(view.ViewJSON, &bytes.Buffer{}, view.LogLevelSilent)
	assert.NotNil(t, cli.Viewer)
	assert.NotNil(t, cli.Stream)
	assert.IsType(t, &view.JSONView{}, cli.Viewer)
	assert.Equal(t, &bytes.Buffer{}, cli.Writer)
}

func TestExactArgs_ExactMatch(t *testing.T) {
	fn := command.ExactArgs(2)
	err := fn(nil, []string{"a", "b"})
	assert.NoError(t, err)
}

func TestExactArgs_TooFew(t *testing.T) {
	fn := command.ExactArgs(2)
	err := fn(nil, []string{"a"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "expected 2 arguments, got 1")
}

func TestExactArgs_TooMany(t *testing.T) {
	fn := command.ExactArgs(2)
	err := fn(nil, []string{"a", "b", "c"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "expected 2 arguments, got 3")
}

func TestMaxArgs_WithinLimit(t *testing.T) {
	fn := command.MaxArgs(2)
	err := fn(nil, []string{"a"})
	assert.NoError(t, err)
}

func TestMaxArgs_AtLimit(t *testing.T) {
	fn := command.MaxArgs(2)
	err := fn(nil, []string{"a", "b"})
	assert.NoError(t, err)
}

func TestMaxArgs_ExceedsLimit(t *testing.T) {
	fn := command.MaxArgs(2)
	err := fn(nil, []string{"a", "b", "c"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "expected at most 2 arguments, got 3")
}
