//go:build !graphcompat

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

// This file is the declared cost of the zero-upstream-modification invariant.
//
// The 36 sibling _test.go files are symlinks to test/integration/suites/core/.
// They reference a package-level variable `env *environment.Environment` and
// a helper `toRawExtension` — both defined by upstream's own setup_test.go.
// Our harness setup_test.go (guarded by `//go:build graphcompat`) declares
// those symbols; without the tag, the package would fail to compile and
// `go vet ./...` would break for the whole module.
//
// Rather than inject build tags into the symlinked upstream files (forbidden
// by the no-upstream-modification rule), we declare the stubs here with the
// inverse tag (`!graphcompat`). The real suite replaces them when the tag is
// present. The stubs are never executed.
package core_test

import (
	"encoding/json"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kubernetes-sigs/kro/test/integration/environment"
)

var env *environment.Environment

func toRawExtension(v interface{}) runtime.RawExtension {
	rawJSON, _ := json.Marshal(v)
	return runtime.RawExtension{Raw: rawJSON}
}
