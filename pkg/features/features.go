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
	// InstanceConditionEvents enables Kubernetes Events on instance condition
	// transitions. When enabled, the controller emits a K8s Event (visible
	// via `kubectl describe`) every time a status condition changes state.
	// This is opt-in to avoid noisy event streams in large clusters.
	InstanceConditionEvents featuregate.Feature = "InstanceConditionEvents"

	// InstanceConditionMetrics enables a Prometheus duration gauge per
	// instance condition and structured log lines on every status change.
	InstanceConditionMetrics featuregate.Feature = "InstanceConditionMetrics"

	// CELOmitFunction enables the omit() CEL function for conditional field
	// omission in resource templates. When enabled, CEL expressions can return
	// omit() to remove the containing field from the rendered object instead
	// of writing a value. When disabled, any use of omit() in an RGD is
	// rejected at build time.
	CELOmitFunction featuregate.Feature = "CELOmitFunction"
)

// defaultKroFeatureGates consists of all known KRO-specific feature keys.
// To add a new feature, define a Feature constant above and add it here with
// its default state and maturity stage (Alpha, Beta, or GA).
var defaultKroFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{
	InstanceConditionEvents:  {Default: false, PreRelease: featuregate.Alpha},
	InstanceConditionMetrics: {Default: false, PreRelease: featuregate.Alpha},
	CELOmitFunction:          {Default: false, PreRelease: featuregate.Alpha},
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
