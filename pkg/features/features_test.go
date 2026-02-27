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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDefaultFeatureGatesAreDisabled verifies that all Alpha/Beta features
// are disabled by default, as per the Kubernetes feature gate convention.
func TestDefaultFeatureGatesAreDisabled(t *testing.T) {
	assert.False(t, FeatureGate.Enabled(ParallelDagWalking),
		"ParallelDagWalking should be disabled by default (Alpha)")
}

// TestEnableFeatureViaSet verifies that a feature can be enabled by calling
// FeatureGate.Set() with a "key=value" string, mimicking the --feature-gates flag.
func TestEnableFeatureViaSet(t *testing.T) {
	// Use a deep copy so we don't mutate the global FeatureGate between tests.
	gate := FeatureGate.DeepCopy()
	require.NoError(t, gate.Set("ParallelDagWalking=true"))
	assert.True(t, gate.Enabled(ParallelDagWalking))
}

// TestDisableFeatureViaSet verifies that a feature can be
// explicitly disabled via Set() after being enabled.
func TestDisableFeatureViaSet(t *testing.T) {
	gate := FeatureGate.DeepCopy()
	require.NoError(t, gate.Set("ParallelDagWalking=true"))
	assert.True(t, gate.Enabled(ParallelDagWalking))

	require.NoError(t, gate.Set("ParallelDagWalking=false"))
	assert.False(t, gate.Enabled(ParallelDagWalking))
}

// TestSetUnknownFeatureReturnsError verifies that specifying an unknown
// feature gate name returns an error rather than silently succeeding.
func TestSetUnknownFeatureReturnsError(t *testing.T) {
	gate := FeatureGate.DeepCopy()
	err := gate.Set("NonExistentFeature=true")
	require.Error(t, err, "setting an unknown feature should return an error")
}

// TestKnownFeaturesContainsAllRegistered verifies that KnownFeatures() lists
// all features that were registered in defaultKroFeatureGates.
func TestKnownFeaturesContainsAllRegistered(t *testing.T) {
	known := FeatureGate.KnownFeatures()
	knownStr := strings.Join(known, " ")

	assert.Contains(t, knownStr, string(ParallelDagWalking))
}
