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

// Package view provides output formatting and logging for the kro CLI.
//
// The package uses a layered architecture: CLI → Viewer → Stream → io.Writer.
// Viewers handle format-specific rendering (human/json/yaml), while Stream
// provides basic output operations. Logs are always human-readable regardless
// of output format.
package view
