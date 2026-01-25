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

import (
	"github.com/fatih/color"
)

// ValidateView renders validation results in human-readable format.
type ValidateView interface {
	Render(result ValidateResult)
}

// ValidateResult holds validation results for one or more files.
type ValidateResult struct {
	FileCount int
	Errors    []ValidateFileError
}

// ValidateFileError represents a validation error in a specific file.
type ValidateFileError struct {
	File    string
	Message string
}

// HasErrors returns true if any validation errors were found.
func (r ValidateResult) HasErrors() bool {
	return len(r.Errors) > 0
}

type validateHumanView struct {
	stream *Stream
}

// NewValidateHumanView creates a view that renders validation results in human-readable format.
func NewValidateHumanView(s *Stream) ValidateView {
	return &validateHumanView{stream: s}
}

func (v *validateHumanView) Render(result ValidateResult) {
	if result.HasErrors() {
		for _, e := range result.Errors {
			v.stream.Println(color.RGB(229, 50, 50).Sprintf("Error!"), e.File+":", e.Message)
		}
		return
	}

	v.stream.Println(color.RGB(50, 108, 229).Sprintf("Valid!"), "no errors found.")
}
