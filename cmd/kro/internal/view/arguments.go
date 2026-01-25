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

// ViewType specifies the output format (human, json, yaml).
type ViewType rune

const (
	ViewNone  ViewType = 0
	ViewHuman ViewType = 'H'
	ViewJSON  ViewType = 'J'
	ViewYAML  ViewType = 'Y'
)

// String returns the string representation of ViewType.
func (vt ViewType) String() string {
	switch vt {
	case ViewNone:
		return "none"
	case ViewHuman:
		return "human"
	case ViewJSON:
		return "json"
	case ViewYAML:
		return "yaml"
	default:
		return "unknown"
	}
}

func ParseOutputFormat(format string) (ViewType, error) {
	switch format {
	case "", "human":
		return ViewHuman, nil
	case "json":
		return ViewJSON, nil
	case "yaml", "yml":
		return ViewYAML, nil
	default:
		return ViewNone, nil
	}
}
