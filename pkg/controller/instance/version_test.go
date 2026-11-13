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

package instance

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
)

// controllerTestParentGVRBeta is the same instance resource served under a
// later version, as a multi-version CRD does.
var controllerTestParentGVRBeta = schema.GroupVersionResource{
	Group: "kro.run", Version: "v1beta1", Resource: "webapps",
}

// TestGVRForUsesTheInstancesOwnVersion pins that reads and writes go through
// the version the instance object itself carries, not the version the
// controller was registered with: with several served versions, writing
// through the wrong one drops fields that version does not declare.
func TestGVRForUsesTheInstancesOwnVersion(t *testing.T) {
	raw := newControllerTestDynamicClient(t)
	c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, nil, nil)

	registered := newInstanceObject("demo", "default")
	assert.Equal(t, controllerTestParentGVR, c.gvrFor(registered),
		"an instance at the registered version keeps that version")

	served := newInstanceObject("demo", "default")
	served.SetAPIVersion("kro.run/v1beta1")
	assert.Equal(t, controllerTestParentGVRBeta, c.gvrFor(served),
		"an instance read under another served version is written back under it")

	bare := &unstructured.Unstructured{Object: map[string]any{}}
	assert.Equal(t, controllerTestParentGVR, c.gvrFor(bare),
		"an object without a version falls back to the registered GVR")
}

// TestGetInstanceAtVersionReadsThroughTheRequestedVersion covers the re-read
// the engine does when a graph revision was compiled against a different
// version than the one the instance arrived on.
func TestGetInstanceAtVersionReadsThroughTheRequestedVersion(t *testing.T) {
	stored := newInstanceObject("demo", "default")
	stored.SetAPIVersion("kro.run/v1beta1")
	require.NoError(t, unstructured.SetNestedField(stored.Object, "beta-only", "spec", "extra"))

	scheme := apimachineryruntime.NewScheme()
	raw := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{
			controllerTestParentGVR:     "WebAppList",
			controllerTestParentGVRBeta: "WebAppList",
			controllerTestDeployGVR:     "DeploymentList",
			controllerTestCMGVR:         "ConfigMapList",
		},
		stored,
	)
	c, _ := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, nil, nil)

	fetched, err := c.getInstanceAtVersion(context.Background(), newInstanceObject("demo", "default"), "v1beta1")
	require.NoError(t, err)
	value, found, err := unstructured.NestedString(fetched.Object, "spec", "extra")
	require.NoError(t, err)
	require.True(t, found, "the field only the later version declares must be visible")
	assert.Equal(t, "beta-only", value)

	var requested []schema.GroupVersionResource
	for _, action := range raw.Actions() {
		if get, ok := action.(k8stesting.GetAction); ok {
			requested = append(requested, get.GetResource())
		}
	}
	assert.Contains(t, requested, controllerTestParentGVRBeta)

	_, err = c.getInstanceAtVersion(context.Background(), newInstanceObject("missing", "default"), "v1beta1")
	assert.True(t, apierrors.IsNotFound(err),
		"a version that does not serve the instance surfaces NotFound so the caller can hold the revision")
}
