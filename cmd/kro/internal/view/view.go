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

package view

var _ Viewer = (*HumanView)(nil)
var _ Viewer = (*JSONView)(nil)
var _ Viewer = (*YAMLView)(nil)

// Viewer provides format-specific output rendering and logging.
type Viewer interface {
	Logger() Logger
}

// NewViewer creates a Viewer for the specified output format.
func NewViewer(vt ViewType, s *Stream, level LogLevel) Viewer {
	// Ensure we never have a nil logger scenario by using appropriate loggers
	switch vt {
	case ViewHuman:
		return NewHumanView(s, level)
	case ViewJSON:
		return NewJSONView(s, level)
	case ViewYAML:
		return NewYAMLView(s, level)
	default:
		panic("unknown view type")
	}
}

// HumanView provides colored, human-readable output for terminals.
type HumanView struct {
	*Stream
	logger Logger
}

// NewHumanView creates a HumanView with colored output and logging.
func NewHumanView(s *Stream, level LogLevel) *HumanView {
	var logger Logger
	if level == LogLevelSilent {
		logger = NewNopLogger()
	} else {
		logger = NewHumanLogger(s.Writer, level)
	}
	return &HumanView{
		Stream: s,
		logger: logger,
	}
}

func (h *HumanView) Logger() Logger {
	return h.logger
}

// JSONView provides JSON-formatted output for machine parsing.
type JSONView struct {
	*Stream
	logger Logger
}

// NewJSONView creates a JSONView with JSON output and human-readable logs.
func NewJSONView(s *Stream, level LogLevel) *JSONView {
	var logger Logger
	if level == LogLevelSilent {
		logger = NewNopLogger()
	} else {
		logger = NewHumanLogger(s.Writer, level)
	}
	return &JSONView{
		Stream: s,
		logger: logger,
	}
}

func (j *JSONView) Logger() Logger {
	return j.logger
}

// YAMLView provides YAML-formatted output.
type YAMLView struct {
	*Stream
	logger Logger
}

// NewYAMLView creates a YAMLView with YAML output and human-readable logs.
func NewYAMLView(s *Stream, level LogLevel) *YAMLView {
	var logger Logger
	if level == LogLevelSilent {
		logger = NewNopLogger()
	} else {
		logger = NewHumanLogger(s.Writer, level)
	}
	return &YAMLView{
		Stream: s,
		logger: logger,
	}
}

func (y *YAMLView) Logger() Logger {
	return y.logger
}
