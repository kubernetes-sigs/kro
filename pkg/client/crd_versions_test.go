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

package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	"k8s.io/apimachinery/pkg/util/sets"
	k8stesting "k8s.io/client-go/testing"
)

// crdVersion builds one CRD version whose spec schema carries the given
// properties, mirroring what the graph builder synthesizes.
func crdVersion(name string, served, storage bool, specProps map[string]v1.JSONSchemaProps) v1.CustomResourceDefinitionVersion {
	return v1.CustomResourceDefinitionVersion{
		Name:    name,
		Served:  served,
		Storage: storage,
		Schema: &v1.CustomResourceValidation{
			OpenAPIV3Schema: &v1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]v1.JSONSchemaProps{
					"spec": {
						Type:       "object",
						Properties: specProps,
					},
				},
			},
		},
		Subresources: &v1.CustomResourceSubresources{
			Status: &v1.CustomResourceSubresourceStatus{},
		},
	}
}

func stringProps(names ...string) map[string]v1.JSONSchemaProps {
	props := make(map[string]v1.JSONSchemaProps, len(names))
	for _, name := range names {
		props[name] = v1.JSONSchemaProps{Type: "string"}
	}
	return props
}

// versionedCRD returns the shared test CRD with its version list replaced.
func versionedCRD(versions ...v1.CustomResourceDefinitionVersion) *v1.CustomResourceDefinition {
	crd := testCRD()
	crd.Spec.Versions = versions
	return crd
}

func TestEnsureVersionRules(t *testing.T) {
	tests := []struct {
		name     string
		existing func() *v1.CustomResourceDefinition
		desired  func() *v1.CustomResourceDefinition
		opts     EnsureOptions
		wantErr  string
		wantIs   error
		verify   func(t *testing.T, got *v1.CustomResourceDefinition)
	}{
		{
			name: "storage version may not move backwards",
			existing: func() *v1.CustomResourceDefinition {
				return versionedCRD(
					crdVersion("v1beta1", true, true, stringProps("name")),
					crdVersion("v1alpha1", true, false, stringProps("name")),
				)
			},
			desired: func() *v1.CustomResourceDefinition {
				return versionedCRD(
					crdVersion("v1alpha1", true, true, stringProps("name")),
					crdVersion("v1beta1", true, false, stringProps("name")),
				)
			},
			opts:    EnsureOptions{AllowBreakingChanges: true, MultiVersion: true},
			wantErr: "schema.apiVersion may only move forward in Kubernetes version priority (current v1beta1, desired v1alpha1)",
		},
		{
			name: "hub move is rejected without the multi-version gate",
			existing: func() *v1.CustomResourceDefinition {
				return versionedCRD(crdVersion("v1alpha1", true, true, stringProps("name")))
			},
			desired: func() *v1.CustomResourceDefinition {
				return versionedCRD(
					crdVersion("v1beta1", true, true, stringProps("name")),
					crdVersion("v1alpha1", true, false, stringProps("name")),
				)
			},
			wantErr: "schema.apiVersion is immutable (current v1alpha1, desired v1beta1)",
		},
		{
			name: "hub moves forward with the old version kept served",
			existing: func() *v1.CustomResourceDefinition {
				return versionedCRD(crdVersion("v1alpha1", true, true, stringProps("name")))
			},
			desired: func() *v1.CustomResourceDefinition {
				return versionedCRD(
					crdVersion("v1beta1", true, true, stringProps("name")),
					crdVersion("v1alpha1", true, false, stringProps("name")),
				)
			},
			opts: EnsureOptions{MultiVersion: true},
			verify: func(t *testing.T, got *v1.CustomResourceDefinition) {
				require.Len(t, got.Spec.Versions, 2)
				assert.Equal(t, "v1beta1", got.Spec.Versions[0].Name)
				assert.True(t, got.Spec.Versions[0].Storage)
				assert.Equal(t, "v1alpha1", got.Spec.Versions[1].Name)
				assert.False(t, got.Spec.Versions[1].Storage)
				assert.True(t, got.Spec.Versions[1].Served)
			},
		},
		{
			name: "removing a stored version is blocked",
			existing: func() *v1.CustomResourceDefinition {
				crd := versionedCRD(
					crdVersion("v1beta1", true, true, stringProps("name")),
					crdVersion("v1alpha1", true, false, stringProps("name")),
				)
				crd.Status.StoredVersions = []string{"v1alpha1", "v1beta1"}
				return crd
			},
			desired: func() *v1.CustomResourceDefinition {
				return versionedCRD(crdVersion("v1beta1", true, true, stringProps("name")))
			},
			opts:    EnsureOptions{MultiVersion: true},
			wantIs:  ErrVersionRemovalBlocked,
			wantErr: "still listed in status.storedVersions",
		},
		{
			name: "removing a stored version is not overridable",
			existing: func() *v1.CustomResourceDefinition {
				crd := versionedCRD(
					crdVersion("v1beta1", true, true, stringProps("name")),
					crdVersion("v1alpha1", true, false, stringProps("name")),
				)
				crd.Status.StoredVersions = []string{"v1alpha1", "v1beta1"}
				return crd
			},
			desired: func() *v1.CustomResourceDefinition {
				return versionedCRD(crdVersion("v1beta1", true, true, stringProps("name")))
			},
			opts:   EnsureOptions{MultiVersion: true, AllowBreakingChanges: true},
			wantIs: ErrVersionRemovalBlocked,
		},
		{
			name: "unserving a version in use by a pinned revision is blocked",
			existing: func() *v1.CustomResourceDefinition {
				crd := versionedCRD(
					crdVersion("v1beta1", true, true, stringProps("name")),
					crdVersion("v1alpha1", true, false, stringProps("name")),
				)
				crd.Status.StoredVersions = []string{"v1beta1"}
				return crd
			},
			desired: func() *v1.CustomResourceDefinition {
				return versionedCRD(
					crdVersion("v1beta1", true, true, stringProps("name")),
					crdVersion("v1alpha1", false, false, stringProps("name")),
				)
			},
			opts: EnsureOptions{
				MultiVersion:         true,
				AllowBreakingChanges: true,
				VersionsInUse:        func() (sets.Set[string], error) { return sets.New("v1alpha1"), nil },
			},
			wantIs:  ErrVersionRemovalBlocked,
			wantErr: "still used by a pinned graph revision",
		},
		{
			name: "retiring an unused, unstored version is allowed",
			existing: func() *v1.CustomResourceDefinition {
				crd := versionedCRD(
					crdVersion("v1beta1", true, true, stringProps("name")),
					crdVersion("v1alpha1", true, false, stringProps("name")),
				)
				crd.Status.StoredVersions = []string{"v1beta1"}
				return crd
			},
			desired: func() *v1.CustomResourceDefinition {
				return versionedCRD(crdVersion("v1beta1", true, true, stringProps("name")))
			},
			opts: EnsureOptions{
				MultiVersion:  true,
				VersionsInUse: func() (sets.Set[string], error) { return sets.New[string](), nil },
			},
			verify: func(t *testing.T, got *v1.CustomResourceDefinition) {
				require.Len(t, got.Spec.Versions, 1)
				assert.Equal(t, "v1beta1", got.Spec.Versions[0].Name)
			},
		},
		{
			name: "spoke dropping a served property is rejected",
			existing: func() *v1.CustomResourceDefinition {
				return versionedCRD(
					crdVersion("v1beta1", true, true, stringProps("name", "size")),
					crdVersion("v1alpha1", true, false, stringProps("name", "size")),
				)
			},
			desired: func() *v1.CustomResourceDefinition {
				return versionedCRD(
					crdVersion("v1beta1", true, true, stringProps("name", "size")),
					crdVersion("v1alpha1", true, false, stringProps("name")),
				)
			},
			opts:    EnsureOptions{MultiVersion: true},
			wantIs:  ErrSpokeSchemaMismatch,
			wantErr: "differs incompatibly from the served schema",
		},
		{
			name: "spoke dropping a served property is overridable",
			existing: func() *v1.CustomResourceDefinition {
				return versionedCRD(
					crdVersion("v1beta1", true, true, stringProps("name", "size")),
					crdVersion("v1alpha1", true, false, stringProps("name", "size")),
				)
			},
			desired: func() *v1.CustomResourceDefinition {
				return versionedCRD(
					crdVersion("v1beta1", true, true, stringProps("name", "size")),
					crdVersion("v1alpha1", true, false, stringProps("name")),
				)
			},
			opts: EnsureOptions{MultiVersion: true, AllowBreakingChanges: true},
			verify: func(t *testing.T, got *v1.CustomResourceDefinition) {
				require.Len(t, got.Spec.Versions, 2)
				assert.NotContains(t, got.Spec.Versions[1].Schema.OpenAPIV3Schema.Properties["spec"].Properties, "size")
			},
		},
		{
			name: "spoke that cannot be converted to the hub is rejected",
			existing: func() *v1.CustomResourceDefinition {
				return versionedCRD(crdVersion("v1alpha1", true, false, stringProps("name", "legacy")),
					crdVersion("v1beta1", true, true, stringProps("name")))
			},
			desired: func() *v1.CustomResourceDefinition {
				return versionedCRD(
					crdVersion("v1beta1", true, true, stringProps("name")),
					crdVersion("v1alpha1", true, false, stringProps("name", "legacy")),
				)
			},
			opts:    EnsureOptions{MultiVersion: true},
			wantErr: "version v1alpha1 cannot be converted to v1beta1 without a conversion webhook",
		},
		{
			name: "default change is rejected only when asked",
			existing: func() *v1.CustomResourceDefinition {
				return versionedCRD(crdVersion("v1alpha1", true, true, stringProps("name")))
			},
			desired: func() *v1.CustomResourceDefinition {
				return versionedCRD(crdVersion("v1alpha1", true, true, map[string]v1.JSONSchemaProps{
					"name": {Type: "string", Default: &v1.JSON{Raw: []byte(`"widget"`)}},
				}))
			},
			opts:    EnsureOptions{RejectDefaultChanges: true},
			wantErr: "default changes are not allowed with rollout strategy Manual",
		},
		{
			name: "default change is accepted by default",
			existing: func() *v1.CustomResourceDefinition {
				return versionedCRD(crdVersion("v1alpha1", true, true, stringProps("name")))
			},
			desired: func() *v1.CustomResourceDefinition {
				return versionedCRD(crdVersion("v1alpha1", true, true, map[string]v1.JSONSchemaProps{
					"name": {Type: "string", Default: &v1.JSON{Raw: []byte(`"widget"`)}},
				}))
			},
			verify: func(t *testing.T, got *v1.CustomResourceDefinition) {
				name := got.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["name"]
				require.NotNil(t, name.Default)
				assert.JSONEq(t, `"widget"`, string(name.Default.Raw))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existing := tt.existing()
			wrapper := newTestCRDWrapper(existing)

			err := wrapper.Ensure(context.Background(), *tt.desired(), tt.opts)

			if tt.wantErr != "" || tt.wantIs != nil {
				require.Error(t, err)
				if tt.wantErr != "" {
					assert.Contains(t, err.Error(), tt.wantErr)
				}
				if tt.wantIs != nil {
					assert.ErrorIs(t, err, tt.wantIs)
				}
				// A rejected update must not have touched the CRD.
				got, getErr := wrapper.Get(context.Background(), existing.Name)
				require.NoError(t, getErr)
				assert.Equal(t, existing.Spec.Versions, got.Spec.Versions)
				return
			}

			require.NoError(t, err)
			got, err := wrapper.Get(context.Background(), existing.Name)
			require.NoError(t, err)
			tt.verify(t, got)
		})
	}
}

func TestEnsureBlockedRemovalWithoutVersionsInUseLookup(t *testing.T) {
	existing := versionedCRD(
		crdVersion("v1beta1", true, true, stringProps("name")),
		crdVersion("v1alpha1", true, false, stringProps("name")),
	)
	existing.Status.StoredVersions = []string{"v1beta1"}

	called := 0
	wrapper := newTestCRDWrapper(existing)
	desired := versionedCRD(crdVersion("v1beta1", true, true, stringProps("name")))

	err := wrapper.Ensure(context.Background(), *desired, EnsureOptions{
		MultiVersion: true,
		VersionsInUse: func() (sets.Set[string], error) {
			called++
			return sets.New("v1alpha1"), nil
		},
	})

	require.ErrorIs(t, err, ErrVersionRemovalBlocked)
	assert.Equal(t, 1, called, "VersionsInUse must be consulted lazily, at most once")
}

func TestSetStoredVersions(t *testing.T) {
	crd := versionedCRD(
		crdVersion("v1beta1", true, true, stringProps("name")),
		crdVersion("v1alpha1", true, false, stringProps("name")),
	)
	crd.Status.StoredVersions = []string{"v1alpha1", "v1beta1"}

	clientset := apiextensionsfake.NewSimpleClientset(crd)
	wrapper := newCRDWrapper(CRDWrapperConfig{
		Client:       clientset.ApiextensionsV1(),
		PollInterval: time.Millisecond,
		Timeout:      time.Second,
	})

	require.NoError(t, wrapper.SetStoredVersions(context.Background(), crd.Name, []string{"v1beta1"}))

	got, err := wrapper.Get(context.Background(), crd.Name)
	require.NoError(t, err)
	assert.Equal(t, []string{"v1beta1"}, got.Status.StoredVersions)
	assert.Equal(t, 1, countStatusUpdates(clientset.Actions()))

	// Already equal: no second write.
	require.NoError(t, wrapper.SetStoredVersions(context.Background(), crd.Name, []string{"v1beta1"}))
	assert.Equal(t, 1, countStatusUpdates(clientset.Actions()))
}

func countStatusUpdates(actions []k8stesting.Action) int {
	count := 0
	for _, action := range actions {
		if action.GetVerb() == "update" && action.GetSubresource() == "status" {
			count++
		}
	}
	return count
}
