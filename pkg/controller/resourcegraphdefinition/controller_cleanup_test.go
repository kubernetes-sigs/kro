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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgotesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

type reactorCRDManager struct {
	client       *apiextensionsfake.Clientset
	pollInterval time.Duration
	timeout      time.Duration
}

func (m *reactorCRDManager) Ensure(context.Context, extv1.CustomResourceDefinition, bool) error {
	return nil
}

func (m *reactorCRDManager) Get(ctx context.Context, name string) (*extv1.CustomResourceDefinition, error) {
	return m.client.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, name, metav1.GetOptions{})
}

func (m *reactorCRDManager) Delete(ctx context.Context, name string) error {
	err := m.client.ApiextensionsV1().CustomResourceDefinitions().Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if apierrors.IsNotFound(err) {
		return nil
	}

	return wait.PollUntilContextTimeout(ctx, m.pollInterval, m.timeout, true, func(ctx context.Context) (bool, error) {
		_, err := m.Get(ctx, name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		return false, nil
	})
}

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
				allowCRDDeletion:  tt.allowCRDDeletion,
				dynamicController: dc,
				crdManager:        manager,
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
				allowCRDDeletion: tt.allowCRDDeletion,
				crdManager:       manager,
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

func TestCleanupResourceGraphDefinitionWaitsForCRDToDisappear(t *testing.T) {
	rgd := newTestRGD("cleanup-waits")
	crdName := extractCRDName(rgd.Spec.Schema.Group, rgd.Spec.Schema.Kind)
	gvr := metadata.GetResourceGraphDefinitionInstanceGVR(
		rgd.Spec.Schema.Group,
		rgd.Spec.Schema.APIVersion,
		rgd.Spec.Schema.Kind,
	)

	clientset := apiextensionsfake.NewSimpleClientset(&extv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: crdName},
	})

	deleteRequested := false
	getCallsAfterDelete := 0
	clientset.PrependReactor("delete", "customresourcedefinitions", func(clientgotesting.Action) (bool, runtime.Object, error) {
		deleteRequested = true
		return true, nil, nil
	})
	clientset.PrependReactor("get", "customresourcedefinitions", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		if !deleteRequested {
			return false, nil, nil
		}

		getCallsAfterDelete++
		if getCallsAfterDelete < 3 {
			return true, &extv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: action.(clientgotesting.GetAction).GetName()},
			}, nil
		}

		return true, nil, apierrors.NewNotFound(schema.GroupResource{
			Group:    extv1.SchemeGroupVersion.Group,
			Resource: "customresourcedefinitions",
		}, action.(clientgotesting.GetAction).GetName())
	})

	dc := newRunningDynamicController(t)
	require.NoError(t, dc.Register(context.Background(), gvr, func(context.Context, ctrl.Request) error { return nil }))

	reconciler := &ResourceGraphDefinitionReconciler{
		allowCRDDeletion:  true,
		dynamicController: dc,
		crdManager: &reactorCRDManager{
			client:       clientset,
			pollInterval: 5 * time.Millisecond,
			timeout:      250 * time.Millisecond,
		},
	}

	err := reconciler.cleanupResourceGraphDefinition(context.Background(), rgd)
	require.NoError(t, err)
	assert.Equal(t, 3, getCallsAfterDelete)
}
