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

package instance

import (
	"os"
	"testing"

	"github.com/kubernetes-sigs/kro/pkg/features"
)

func TestMain(m *testing.M) {
	// Fleet-gate tests in this package need the alpha gate on.
	if err := features.FeatureGate.Set("PropagateWhen=true"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
