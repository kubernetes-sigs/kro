// Copyright 2026 The Kubernetes Authors.
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

package core_test

import (
	"os"
	"testing"

	"github.com/kubernetes-sigs/kro/test/integration/graphengine/environment"
)

// TestMain owns the envtest lifecycle for the whole core suite. Booting
// envtest is the slowest step (~3s), so we run once and let every test
// in this package share the control plane. Each test isolates itself via
// per-test namespaces created with env.CreateNamespace.
func TestMain(m *testing.M) {
	code := m.Run()
	environment.StopShared()
	os.Exit(code)
}
