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
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

func TestExtractCRDName(t *testing.T) {
	tests := []struct {
		name  string
		group string
		kind  string
		want  string
	}{
		{
			name:  "pluralizes compound kinds",
			group: "example.io",
			kind:  "NetworkPolicy",
			want:  "networkpolicies.example.io",
		},
		{
			name:  "pluralizes simple kinds",
			group: "example.io",
			kind:  "Network",
			want:  "networks.example.io",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractCRDName(tt.group, tt.kind))
		})
	}
}

func TestShutdownResourceGraphDefinitionMicroController(t *testing.T) {
	tests := []struct {
		name     string
		register bool
	}{
		{
			name:     "deregisters registered controllers",
			register: true,
		},
		{
			name: "ignores missing registrations",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := newTestRGD("shutdown")
			gvr := metadata.GetResourceGraphDefinitionInstanceGVR(rgd.Spec.Schema.Group, rgd.Spec.Schema.APIVersion, rgd.Spec.Schema.Kind)
			dc := newRunningDynamicController(t)
			if tt.register {
				require.NoError(t, dc.Register(context.Background(), gvr, func(context.Context, ctrl.Request) error { return nil }))
			}

			reconciler := &ResourceGraphDefinitionReconciler{dynamicController: dc}
			require.NoError(t, reconciler.shutdownResourceGraphDefinitionMicroController(context.Background(), &gvr))
		})
	}
}

func TestCleanupResourceGraphDefinition(t *testing.T) {
	tests := []struct {
		name             string
		allowCRDDeletion bool
		deleteErr        error
		wantDeleted      []string
		wantErr          string
	}{
		{
			name:             "cleans up the controller and crd",
			allowCRDDeletion: true,
			wantDeleted:      []string{"networks.example.io"},
		},
		{
			name: "skips crd deletion when disabled",
		},
		{
			name:             "returns crd cleanup errors",
			allowCRDDeletion: true,
			deleteErr:        errors.New("delete boom"),
			wantDeleted:      []string{"networks.example.io"},
			wantErr:          "failed to cleanup CRD networks.example.io: error deleting CRD: delete boom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := newTestRGD("cleanup")
			gvr := metadata.GetResourceGraphDefinitionInstanceGVR(rgd.Spec.Schema.Group, rgd.Spec.Schema.APIVersion, rgd.Spec.Schema.Kind)
			dc := newRunningDynamicController(t)
			require.NoError(t, dc.Register(context.Background(), gvr, func(context.Context, ctrl.Request) error { return nil }))

			manager := &stubCRDManager{deleteErr: tt.deleteErr}
			reconciler := &ResourceGraphDefinitionReconciler{
				cfg:               Config{AllowCRDDeletion: tt.allowCRDDeletion},
				dynamicController: dc,
				crdManager:        manager,
				revisionsRegistry: revisions.NewRegistry(),
			}

			err := reconciler.cleanupResourceGraphDefinition(context.Background(), rgd)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.EqualError(t, err, tt.wantErr)
			}

			assert.Equal(t, tt.wantDeleted, manager.deleted)
		})
	}
}

func TestCleanupPreservesRegistryEntries(t *testing.T) {
	rgd := newTestRGD("evict")
	gvr := metadata.GetResourceGraphDefinitionInstanceGVR(rgd.Spec.Schema.Group, rgd.Spec.Schema.APIVersion, rgd.Spec.Schema.Kind)
	dc := newRunningDynamicController(t)
	require.NoError(t, dc.Register(context.Background(), gvr, func(context.Context, ctrl.Request) error { return nil }))

	registry := revisions.NewRegistry()
	registry.Put(revisions.Entry{RGDName: rgd.Name, Revision: 1, SpecHash: "aaa", State: revisions.RevisionStateActive})
	registry.Put(revisions.Entry{RGDName: rgd.Name, Revision: 2, SpecHash: "bbb", State: revisions.RevisionStateActive})
	registry.Put(revisions.Entry{RGDName: "other-rgd", Revision: 1, SpecHash: "ccc", State: revisions.RevisionStateActive})

	reconciler := &ResourceGraphDefinitionReconciler{
		dynamicController: dc,
		crdManager:        &stubCRDManager{},
		revisionsRegistry: registry,
	}

	require.NoError(t, reconciler.cleanupResourceGraphDefinition(context.Background(), rgd))

	// RGD cleanup no longer evicts registry entries. The GraphRevision
	// controller handles eviction as each revision is deleted by GC.
	_, found := registry.Get(rgd.Name, 1)
	assert.True(t, found, "revision 1 should be preserved")
	_, found = registry.Get(rgd.Name, 2)
	assert.True(t, found, "revision 2 should be preserved")

	_, found = registry.Get("other-rgd", 1)
	assert.True(t, found, "unrelated RGD entries must be preserved")
}

func TestCleanupResourceGraphDefinitionCRD(t *testing.T) {
	tests := []struct {
		name             string
		allowCRDDeletion bool
		deleteErr        error
		wantDeleted      []string
		wantErr          string
	}{
		{
			name:        "skips deletion when crd deletion is disabled",
			deleteErr:   errors.New("should not be called"),
			wantDeleted: nil,
		},
		{
			name:             "returns delete errors",
			allowCRDDeletion: true,
			deleteErr:        errors.New("delete boom"),
			wantDeleted:      []string{"networks.example.io"},
			wantErr:          "error deleting CRD",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &stubCRDManager{deleteErr: tt.deleteErr}
			reconciler := &ResourceGraphDefinitionReconciler{
				cfg:        Config{AllowCRDDeletion: tt.allowCRDDeletion},
				crdManager: manager,
			}

			err := reconciler.cleanupResourceGraphDefinitionCRD(context.Background(), "networks.example.io")
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}

			assert.Equal(t, tt.wantDeleted, manager.deleted)
		})
	}
}
