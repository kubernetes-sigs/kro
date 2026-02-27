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

package features

import (
	"k8s.io/component-base/featuregate"
)

const (
	// ParallelDagWalking enables the controller to walk the ResourceGraphDefinition
	// DAG in parallel, potentially improving reconciliation throughput for large graphs.
	ParallelDagWalking featuregate.Feature = "ParallelDagWalking"
)

// defaultKroFeatureGates consists of all known KRO-specific feature keys.
// To add a new feature, define a Feature constant above and add it here with
// its default state and maturity stage (Alpha, Beta, or GA).
var defaultKroFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{
	ParallelDagWalking: {Default: false, PreRelease: featuregate.Alpha},
}

// FeatureGate is the shared global MutableFeatureGate for KRO.
// It is populated at init time and can be configured via the --feature-gates
// command-line flag in the controller binary.
var FeatureGate featuregate.MutableFeatureGate = featuregate.NewFeatureGate()

func init() {
	if err := FeatureGate.Add(defaultKroFeatureGates); err != nil {
		panic(err)
	}
}
