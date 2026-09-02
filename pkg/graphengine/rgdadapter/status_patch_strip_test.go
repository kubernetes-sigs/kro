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

package rgdadapter

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// The controller writes .status.conditions and .status.state under its own
// field manager (kro-instance-status). If the synthesized author-status patch
// node also carries those fields, two Force:true SSA writers fight over the
// same paths forever (the value flaps between the author's and kro's on every
// poll). authorStatusPatchNode must therefore strip BOTH `conditions` and
// `state` from the node's payload, leaving only genuine author fields. This is
// the field `kubectl wait` / CI gates match on, so a regression here is
// silently catastrophic.
func TestAuthorStatusPatchNode_StripsControllerOwnedFields(t *testing.T) {
	t.Parallel()

	rgdWithStatus := func(status string) *v1alpha1.ResourceGraphDefinition {
		return &v1alpha1.ResourceGraphDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
			Spec: v1alpha1.ResourceGraphDefinitionSpec{
				Schema: &v1alpha1.Schema{
					Kind:       "WebApp",
					APIVersion: "v1alpha1",
					Group:      "kro.run",
					Status:     apimachineryruntime.RawExtension{Raw: []byte(status)},
				},
			},
		}
	}

	t.Run("state and conditions are stripped, author fields survive", func(t *testing.T) {
		t.Parallel()

		rgd := rgdWithStatus(`{"foo":"${schema.spec.foo}","state":"${schema.spec.state}","conditions":[]}`)

		node, ok, err := authorStatusPatchNode(rgd)
		require.NoError(t, err)
		require.True(t, ok, "an RGD with a genuine author field must still yield a patch node")
		require.NotNil(t, node.Patch)

		var manifest map[string]any
		require.NoError(t, json.Unmarshal(node.Patch.Raw, &manifest))
		status, isMap := manifest["status"].(map[string]any)
		require.True(t, isMap, "the synthesized node must carry a status map")

		assert.Contains(t, status, "foo", "author fields must survive")
		assert.NotContains(t, status, "state",
			".status.state is controller-owned and must be stripped to avoid two SSA writers fighting")
		assert.NotContains(t, status, "conditions",
			".status.conditions is controller-owned and must be stripped")
	})

	t.Run("a status of only controller-owned fields yields no node", func(t *testing.T) {
		t.Parallel()

		rgd := rgdWithStatus(`{"state":"${schema.spec.state}","conditions":[]}`)

		node, ok, err := authorStatusPatchNode(rgd)
		require.NoError(t, err)
		assert.False(t, ok,
			"stripping state and conditions leaves an empty status, so no patch node is synthesized")
		assert.Nil(t, node.Patch)
	})
}
