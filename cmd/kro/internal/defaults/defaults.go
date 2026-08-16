// Copyright 2025 The Kubernetes Authors.
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

// Package defaults exposes shared default values consumed by the kro CLI
// commands. Kept as a leaf package so any cmd/kro subpackage can import it
// without cycling through cmd/kro/commands.
package defaults

import "github.com/kubernetes-sigs/kro/pkg/graph"

// RGDConfig mirrors the controller's default RGD collection limits
// (see cmd/controller/main.go rgd-max-collection-* flag defaults). It is
// shared by every kro CLI command that constructs a graph without user
// overrides.
var RGDConfig = graph.RGDConfig{
	MaxCollectionSize:          1000,
	MaxCollectionDimensionSize: 10,
}
