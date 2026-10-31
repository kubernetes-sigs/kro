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

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kubernetes-sigs/kro/pkg/features"
)

func TestFeatureGatesFlag(t *testing.T) {
	root := NewRootCommand()
	t.Cleanup(func() {
		require.NoError(t, features.FeatureGate.Set("CELOmitFunction=false"))
	})

	flag := root.PersistentFlags().Lookup("feature-gates")
	require.NotNil(t, flag, "root command should expose --feature-gates")
	assert.Contains(t, flag.Usage, string(features.CELOmitFunction))

	require.False(t, features.FeatureGate.Enabled(features.CELOmitFunction))

	require.NoError(t, root.ParseFlags([]string{"--feature-gates=CELOmitFunction=true"}))
	assert.True(t, features.FeatureGate.Enabled(features.CELOmitFunction))
}
