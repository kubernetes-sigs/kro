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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

func TestConditionsMarker(t *testing.T) {
	tests := []struct {
		name      string
		condition string
		reason    string
		rootReady bool
		apply     func(*ConditionsMarker)
		check     func(*testing.T, *v1alpha1.ResourceGraphDefinition)
	}{
		{
			name:      "marks ready when all terminal conditions are true",
			rootReady: true,
			apply: func(m *ConditionsMarker) {
				m.ResourceGraphValid()
				m.KindReady("Network")
				m.ControllerRunning()
			},
			check: func(t *testing.T, rgd *v1alpha1.ResourceGraphDefinition) {
				assert.True(t, conditionFor(t, rgd, ResourceGraphAccepted).IsTrue())
				assert.True(t, conditionFor(t, rgd, KindReady).IsTrue())
				assert.True(t, conditionFor(t, rgd, ControllerReady).IsTrue())
			},
		},
		{
			name:      "graph invalid",
			condition: ResourceGraphAccepted,
			reason:    "InvalidResourceGraph",
			apply: func(m *ConditionsMarker) {
				m.ResourceGraphInvalid("bad graph")
			},
		},
		{
			name:      "kind unready",
			condition: KindReady,
			reason:    "Failed",
			apply: func(m *ConditionsMarker) {
				m.KindUnready("crd failed")
			},
		},
		{
			name:      "controller failed",
			condition: ControllerReady,
			reason:    "FailedToStart",
			apply: func(m *ConditionsMarker) {
				m.ControllerFailedToStart("register failed")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := newTestRGD(tt.name)
			marker := NewConditionsMarkerFor(rgd)
			tt.apply(marker)

			assert.Equal(t, tt.rootReady, rgdConditionTypes.For(rgd).IsRootReady())
			if tt.check != nil {
				tt.check(t, rgd)
				return
			}

			cond := conditionFor(t, rgd, tt.condition)
			assert.True(t, cond.IsFalse())
			require.NotNil(t, cond.Reason)
			assert.Equal(t, tt.reason, *cond.Reason)
		})
	}
}

func TestSetManaged(t *testing.T) {
	tests := []struct {
		name             string
		withFinalizer    bool
		wantPatchCalls   int
		wantHasFinalizer bool
	}{
		{
			name:             "adds the finalizer when missing",
			wantPatchCalls:   1,
			wantHasFinalizer: true,
		},
		{
			name:             "does nothing when the finalizer already exists",
			withFinalizer:    true,
			wantHasFinalizer: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := newTestRGD("set-managed")
			if tt.withFinalizer {
				metadata.SetResourceGraphDefinitionFinalizer(rgd)
			}

			patchCalls := 0
			c := newTestClient(t, interceptor.Funcs{
				Patch: func(ctx context.Context, base client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patchCalls++
					return base.Patch(ctx, obj, patch, opts...)
				},
			}, rgd.DeepCopy())

			reconciler := &ResourceGraphDefinitionReconciler{Client: c}
			require.NoError(t, reconciler.setManaged(context.Background(), rgd))
			assert.Equal(t, tt.wantPatchCalls, patchCalls)
			assert.Equal(t, tt.wantHasFinalizer, metadata.HasResourceGraphDefinitionFinalizer(getStoredRGD(t, c, rgd.Name)))
		})
	}
}

func TestSetUnmanaged(t *testing.T) {
	tests := []struct {
		name             string
		withFinalizer    bool
		wantPatchCalls   int
		wantHasFinalizer bool
	}{
		{
			name:           "removes the finalizer when present",
			withFinalizer:  true,
			wantPatchCalls: 1,
		},
		{
			name:             "does nothing when the finalizer is already gone",
			wantHasFinalizer: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := newTestRGD("set-unmanaged")
			if tt.withFinalizer {
				metadata.SetResourceGraphDefinitionFinalizer(rgd)
			}

			patchCalls := 0
			c := newTestClient(t, interceptor.Funcs{
				Patch: func(ctx context.Context, base client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					patchCalls++
					return base.Patch(ctx, obj, patch, opts...)
				},
			}, rgd.DeepCopy())

			reconciler := &ResourceGraphDefinitionReconciler{Client: c}
			require.NoError(t, reconciler.setUnmanaged(context.Background(), rgd))
			assert.Equal(t, tt.wantPatchCalls, patchCalls)
			assert.Equal(t, tt.wantHasFinalizer, metadata.HasResourceGraphDefinitionFinalizer(getStoredRGD(t, c, rgd.Name)))
		})
	}
}

func TestUpdateStatus(t *testing.T) {
	tests := []struct {
		name             string
		topologicalOrder []string
		resources        []v1alpha1.ResourceInformation
		build            func(*testing.T) (*ResourceGraphDefinitionReconciler, client.WithWatch, *v1alpha1.ResourceGraphDefinition, *int)
		check            func(*testing.T, error, client.WithWatch, *v1alpha1.ResourceGraphDefinition, *int)
	}{
		{
			name:             "persists desired status",
			topologicalOrder: []string{"vpc", "subnetA"},
			resources: []v1alpha1.ResourceInformation{
				{
					ID: "subnetA",
					Dependencies: []v1alpha1.Dependency{
						{ID: "vpc"},
					},
				},
			},
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, client.WithWatch, *v1alpha1.ResourceGraphDefinition, *int) {
				rgd := newTestRGD("status-persist")
				marker := NewConditionsMarkerFor(rgd)
				marker.ResourceGraphValid()
				marker.KindReady("Network")
				marker.ControllerRunning()

				current := rgd.DeepCopy()
				current.Status = rgd.Status
				current.Status.TopologicalOrder = nil
				current.Status.Resources = nil

				patchCalls := 0
				c := newTestClient(t, interceptor.Funcs{
					SubResourcePatch: func(ctx context.Context, base client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
						patchCalls++
						return base.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
					},
				}, current)

				return &ResourceGraphDefinitionReconciler{Client: c}, c, rgd, &patchCalls
			},
			check: func(t *testing.T, err error, c client.WithWatch, rgd *v1alpha1.ResourceGraphDefinition, patchCalls *int) {
				require.NoError(t, err)
				assert.Equal(t, 1, *patchCalls)
				stored := getStoredRGD(t, c, rgd.Name)
				assert.Equal(t, v1alpha1.ResourceGraphDefinitionStateActive, stored.Status.State)
				assert.Equal(t, []string{"vpc", "subnetA"}, stored.Status.TopologicalOrder)
				assert.Equal(t, []v1alpha1.ResourceInformation{
					{
						ID: "subnetA",
						Dependencies: []v1alpha1.Dependency{
							{ID: "vpc"},
						},
					},
				}, stored.Status.Resources)
			},
		},
		{
			name:             "does nothing when status already matches",
			topologicalOrder: []string{"vpc", "subnetA"},
			resources: []v1alpha1.ResourceInformation{
				{
					ID: "subnetA",
					Dependencies: []v1alpha1.Dependency{
						{ID: "vpc"},
					},
				},
			},
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, client.WithWatch, *v1alpha1.ResourceGraphDefinition, *int) {
				rgd := newTestRGD("status-noop")
				marker := NewConditionsMarkerFor(rgd)
				marker.ResourceGraphValid()
				marker.KindReady("Network")
				marker.ControllerRunning()

				current := rgd.DeepCopy()
				current.Status = rgd.Status
				current.Status.State = v1alpha1.ResourceGraphDefinitionStateActive
				current.Status.TopologicalOrder = []string{"vpc", "subnetA"}
				current.Status.Resources = []v1alpha1.ResourceInformation{
					{
						ID: "subnetA",
						Dependencies: []v1alpha1.Dependency{
							{ID: "vpc"},
						},
					},
				}

				patchCalls := 0
				c := newTestClient(t, interceptor.Funcs{
					Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
						current.DeepCopyInto(obj.(*v1alpha1.ResourceGraphDefinition))
						return nil
					},
					SubResourcePatch: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
						patchCalls++
						return nil
					},
				})

				return &ResourceGraphDefinitionReconciler{Client: c}, c, rgd, &patchCalls
			},
			check: func(t *testing.T, err error, _ client.WithWatch, _ *v1alpha1.ResourceGraphDefinition, patchCalls *int) {
				require.NoError(t, err)
				assert.Equal(t, 0, *patchCalls)
			},
		},
		{
			name:             "marks the status inactive when root is not ready",
			topologicalOrder: []string{"vpc"},
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, client.WithWatch, *v1alpha1.ResourceGraphDefinition, *int) {
				rgd := newTestRGD("status-inactive")
				c := newTestClient(t, interceptor.Funcs{}, rgd.DeepCopy())
				return &ResourceGraphDefinitionReconciler{Client: c}, c, rgd, nil
			},
			check: func(t *testing.T, err error, c client.WithWatch, rgd *v1alpha1.ResourceGraphDefinition, _ *int) {
				require.NoError(t, err)
				assert.Equal(t, v1alpha1.ResourceGraphDefinitionStateInactive, getStoredRGD(t, c, rgd.Name).Status.State)
			},
		},
		{
			name: "returns a wrapped get error",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, client.WithWatch, *v1alpha1.ResourceGraphDefinition, *int) {
				rgd := newTestRGD("status-get-error")
				c := newTestClient(t, interceptor.Funcs{
					Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
						return errors.New("get boom")
					},
				})
				return &ResourceGraphDefinitionReconciler{Client: c}, c, rgd, nil
			},
			check: func(t *testing.T, err error, _ client.WithWatch, _ *v1alpha1.ResourceGraphDefinition, _ *int) {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "failed to get current resource graph definition")
				assert.Contains(t, err.Error(), "get boom")
			},
		},
		{
			name:             "returns a status patch error",
			topologicalOrder: []string{"vpc"},
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, client.WithWatch, *v1alpha1.ResourceGraphDefinition, *int) {
				rgd := newTestRGD("status-patch-error")
				marker := NewConditionsMarkerFor(rgd)
				marker.ResourceGraphValid()

				c := newTestClient(t, interceptor.Funcs{
					SubResourcePatch: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
						return errors.New("status boom")
					},
				}, rgd.DeepCopy())
				return &ResourceGraphDefinitionReconciler{Client: c}, c, rgd, nil
			},
			check: func(t *testing.T, err error, _ client.WithWatch, _ *v1alpha1.ResourceGraphDefinition, _ *int) {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "status boom")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reconciler, c, rgd, patchCalls := tt.build(t)
			err := reconciler.updateStatus(context.Background(), rgd, tt.topologicalOrder, tt.resources)

			tt.check(t, err, c, rgd, patchCalls)
		})
	}
}
