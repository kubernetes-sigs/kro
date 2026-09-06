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

package resourcegraphdefinition

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

func rgdWithResourceIDs(ids ...string) *v1alpha1.ResourceGraphDefinition {
	resources := make([]*v1alpha1.Resource, 0, len(ids))
	for _, id := range ids {
		resources = append(resources, &v1alpha1.Resource{ID: id})
	}
	return &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "rgd", UID: "uid"},
		Spec:       v1alpha1.ResourceGraphDefinitionSpec{Resources: resources},
	}
}

func TestWarnOnEncodedResourceIDs(t *testing.T) {
	t.Parallel()

	longID := "my" + strings.Repeat("Long", 20) + "NodeID"
	require.True(t, metadata.NodeIDTokenIsHashed(longID))

	recorder := record.NewFakeRecorder(10)
	r := &ResourceGraphDefinitionReconciler{recorder: recorder}
	r.warnOnEncodedResourceIDs(rgdWithResourceIDs("frontend", longID))

	var got []string
	for len(recorder.Events) > 0 {
		got = append(got, <-recorder.Events)
	}

	require.Len(t, got, 1, "only the over-long id should warn")
	assert.Contains(t, got[0], "Warning NodeIDEncoded")
	assert.Contains(t, got[0], longID)
	assert.Contains(t, got[0], metadata.NodeIDToken(longID))
	assert.Contains(t, got[0], metadata.NodePathAnnotation)
}

func TestWarnOnEncodedResourceIDs_NoRecorder(t *testing.T) {
	t.Parallel()

	r := &ResourceGraphDefinitionReconciler{}
	assert.NotPanics(t, func() {
		r.warnOnEncodedResourceIDs(rgdWithResourceIDs(strings.Repeat("a", 100)))
	})
}
