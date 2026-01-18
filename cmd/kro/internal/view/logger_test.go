package view_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/kubernetes-sigs/kro/cmd/kro/internal/view"
	"github.com/stretchr/testify/assert"
)

func setupHumanLogger(level view.LogLevel) (*bytes.Buffer, view.Logger) {
	buf := &bytes.Buffer{}
	stream := view.NewStream(buf)
	humanView := view.NewHumanView(stream, level)
	return buf, humanView.Logger()
}

func setupJsonLogger(level view.LogLevel) (*bytes.Buffer, view.Logger) {
	buf := &bytes.Buffer{}
	stream := view.NewStream(buf)
	jsonView := view.NewJSONView(stream, level)
	return buf, jsonView.Logger()
}

func TestHumanLogger_Debug(t *testing.T) {
	buf, logger := setupHumanLogger(view.LogLevelDebug)
	logger.Debug("test debug message")

	output := buf.String()
	assert.Contains(t, output, "DEBUG")
	assert.Contains(t, output, "test debug message")
}

func TestHumanLogger_Info(t *testing.T) {
	buf, logger := setupHumanLogger(view.LogLevelInfo)
	logger.Info("test info message")

	output := buf.String()
	assert.Contains(t, output, "INFO")
	assert.Contains(t, output, "test info message")
}

func TestHumanLogger_DebugLevelLogsAll(t *testing.T) {
	buf, logger := setupHumanLogger(view.LogLevelDebug)

	logger.Debug("debug message")
	logger.Info("info message")

	output := buf.String()
	assert.Contains(t, output, "debug message")
	assert.Contains(t, output, "info message")
}

func TestHumanLogger_InfoLevelFiltersDebug(t *testing.T) {
	buf, logger := setupHumanLogger(view.LogLevelInfo)

	logger.Debug("debug message")
	logger.Info("info message")

	output := buf.String()
	assert.NotContains(t, output, "debug message")
	assert.Contains(t, output, "info message")
}

func TestHumanLogger_SilentLevelFiltersAll(t *testing.T) {
	buf, logger := setupHumanLogger(view.LogLevelSilent)

	logger.Debug("debug message")
	logger.Info("info message")

	output := buf.String()
	assert.NotContains(t, output, "debug message")
	assert.NotContains(t, output, "info message")
}

func TestJSONLogger_Debug(t *testing.T) {
	buf, logger := setupJsonLogger(view.LogLevelDebug)
	logger.Debug("test debug message")

	var entry map[string]any
	decoder := json.NewDecoder(buf)
	err := decoder.Decode(&entry)
	assert.NoError(t, err, "JSON output should be valid")

	assert.Equal(t, "DEBUG", entry["level"])
	assert.Equal(t, "test debug message", entry["msg"])
	assert.NotEmpty(t, entry["time"])
}

func TestJSONLogger_Info(t *testing.T) {
	buf, logger := setupJsonLogger(view.LogLevelInfo)
	logger.Info("test info message")

	var entry map[string]any
	decoder := json.NewDecoder(buf)
	err := decoder.Decode(&entry)
	assert.NoError(t, err, "JSON output should be valid")

	assert.Equal(t, "INFO", entry["level"])
	assert.Equal(t, "test info message", entry["msg"])
	assert.NotEmpty(t, entry["time"])
}

func TestJSONLogger_LogLevelFiltering(t *testing.T) {
	buf, logger := setupJsonLogger(view.LogLevelInfo)

	logger.Debug("debug message")
	logger.Info("info message")

	output := buf.String()

	assert.NotContains(t, output, "debug message")
	assert.Contains(t, output, "info message")
}
