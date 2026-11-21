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

package hash

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

func versionedSpec() v1alpha1.ResourceGraphDefinitionSpec {
	return v1alpha1.ResourceGraphDefinitionSpec{
		Schema: &v1alpha1.Schema{
			Kind:       "App",
			APIVersion: "v1beta1",
			Group:      "kro.run",
			Spec:       raw(`{"name":"string"}`),
		},
		Resources: []*v1alpha1.Resource{
			{ID: "config", Template: raw(`{"apiVersion":"v1","kind":"ConfigMap"}`)},
		},
	}
}

func TestSpecIgnoresRollout(t *testing.T) {
	base := versionedSpec()
	baseHash, err := Spec(base)
	require.NoError(t, err)

	for _, strategy := range []v1alpha1.RolloutStrategy{
		v1alpha1.RolloutStrategyLatest,
		v1alpha1.RolloutStrategyManual,
		"",
	} {
		withRollout := versionedSpec()
		withRollout.Rollout = &v1alpha1.Rollout{Strategy: strategy}

		got, err := Spec(withRollout)
		require.NoError(t, err)
		assert.Equal(t, baseHash, got, "rollout strategy %q must not affect the spec hash", strategy)
	}
}

func TestSpecTracksPreviousVersions(t *testing.T) {
	baseHash, err := Spec(versionedSpec())
	require.NoError(t, err)

	withSpoke := versionedSpec()
	withSpoke.Schema.PreviousVersions = []v1alpha1.PreviousSchemaVersion{{
		APIVersion: "v1alpha1",
		Spec:       raw(`{"name":"string"}`),
	}}
	spokeHash, err := Spec(withSpoke)
	require.NoError(t, err)
	assert.NotEqual(t, baseHash, spokeHash)

	// The spoke payload is normalized, so key order alone is not a change...
	reordered := versionedSpec()
	reordered.Schema.PreviousVersions = []v1alpha1.PreviousSchemaVersion{{
		APIVersion: "v1alpha1",
		Spec:       raw(`{"name":"string","size":"integer"}`),
	}}
	reorderedHash, err := Spec(reordered)
	require.NoError(t, err)

	sameContent := versionedSpec()
	sameContent.Schema.PreviousVersions = []v1alpha1.PreviousSchemaVersion{{
		APIVersion: "v1alpha1",
		Spec:       raw(`{"size":"integer","name":"string"}`),
	}}
	sameContentHash, err := Spec(sameContent)
	require.NoError(t, err)
	assert.Equal(t, reorderedHash, sameContentHash)

	// ... but an actual edit to a spoke is.
	assert.NotEqual(t, spokeHash, reorderedHash)

	deprecated := versionedSpec()
	deprecated.Schema.PreviousVersions = []v1alpha1.PreviousSchemaVersion{{
		APIVersion: "v1alpha1",
		Spec:       raw(`{"name":"string"}`),
		Deprecated: true,
	}}
	deprecatedHash, err := Spec(deprecated)
	require.NoError(t, err)
	assert.NotEqual(t, spokeHash, deprecatedHash)
}
