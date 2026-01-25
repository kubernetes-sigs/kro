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

package view_test

import (
	"bytes"
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

func TestJSONView_UsesHumanLogger(t *testing.T) {
	// JSONView now uses HumanLogger for logs (like kubectl)
	// Logs are always human-readable regardless of output format
	buf, logger := setupJsonLogger(view.LogLevelDebug)
	logger.Debug("test debug message")

	output := buf.String()
	assert.Contains(t, output, "DEBUG")
	assert.Contains(t, output, "test debug message")
}

func TestJSONView_LogLevelFiltering(t *testing.T) {
	// JSONView now uses HumanLogger, so logs are text-based
	buf, logger := setupJsonLogger(view.LogLevelInfo)

	logger.Debug("debug message")
	logger.Info("info message")

	output := buf.String()

	assert.NotContains(t, output, "debug message")
	assert.Contains(t, output, "info message")
}
