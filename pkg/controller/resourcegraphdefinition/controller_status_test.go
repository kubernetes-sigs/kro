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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
			name:      "revision lineage failed",
			condition: RevisionLineageResolved,
			reason:    "ResolutionFailed",
			apply: func(m *ConditionsMarker) {
				m.RevisionLineageFailed("lineage failed")
			},
		},
		{
			name:      "labeler failed",
			condition: ControllerReady,
			reason:    "FailedLabelerSetup",
			apply: func(m *ConditionsMarker) {
				m.FailedLabelerSetup("duplicate labels")
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
		{
			name:      "gc condition is informational only",
			condition: GraphRevisionGCHealthy,
			reason:    "GarbageCollectionFailed",
			rootReady: true,
			apply: func(m *ConditionsMarker) {
				m.ResourceGraphValid()
				m.KindReady("Network")
				m.ControllerRunning()
				m.GraphRevisionGCUnhealthy("gc boom")
			},
			check: func(t *testing.T, rgd *v1alpha1.ResourceGraphDefinition) {
				assert.True(t, conditionFor(t, rgd, ResourceGraphAccepted).IsTrue())
				assert.True(t, conditionFor(t, rgd, KindReady).IsTrue())
				assert.True(t, conditionFor(t, rgd, ControllerReady).IsTrue())
				assert.True(t, conditionFor(t, rgd, GraphRevisionGCHealthy).IsFalse())
			},
		},
		{
			name:      "revision lineage pending can coexist with ready serving",
			rootReady: true,
			apply: func(m *ConditionsMarker) {
				m.KindReady("Network")
				m.ControllerRunning()
				m.RevisionLineagePending("Waiting", "lineage settling")
			},
			check: func(t *testing.T, rgd *v1alpha1.ResourceGraphDefinition) {
				cond := conditionFor(t, rgd, RevisionLineageResolved)
				assert.True(t, cond.IsUnknown())
				require.NotNil(t, cond.Reason)
				assert.Equal(t, "Waiting", *cond.Reason)
			},
		},
		{
			name: "serving unknown keeps ready unresolved",
			apply: func(m *ConditionsMarker) {
				m.ServingUnknown("InvalidResourceGraph", "new generation is invalid while older serving state remains")
			},
			check: func(t *testing.T, rgd *v1alpha1.ResourceGraphDefinition) {
				assert.True(t, conditionFor(t, rgd, KindReady).IsUnknown())
				assert.True(t, conditionFor(t, rgd, ControllerReady).IsUnknown())
			},
		},
		{
			name:      "resource graph acceptance is informational for ready",
			rootReady: true,
			apply: func(m *ConditionsMarker) {
				m.KindReady("Network")
				m.ControllerRunning()
				m.ResourceGraphInvalid("bad graph")
			},
			check: func(t *testing.T, rgd *v1alpha1.ResourceGraphDefinition) {
				assert.True(t, conditionFor(t, rgd, ResourceGraphAccepted).IsFalse())
				assert.True(t, conditionFor(t, rgd, KindReady).IsTrue())
				assert.True(t, conditionFor(t, rgd, ControllerReady).IsTrue())
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

func TestInformationalConditionStatusMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		condition string
		want      metav1.ConditionStatus
		apply     func(*ConditionsMarker)
	}{
		{
			name:      "resource graph accepted true",
			condition: ResourceGraphAccepted,
			want:      metav1.ConditionTrue,
			apply: func(m *ConditionsMarker) {
				m.ResourceGraphValid()
			},
		},
		{
			name:      "resource graph accepted false",
			condition: ResourceGraphAccepted,
			want:      metav1.ConditionFalse,
			apply: func(m *ConditionsMarker) {
				m.ResourceGraphInvalid("bad graph")
			},
		},
		{
			name:      "resource graph accepted unknown",
			condition: ResourceGraphAccepted,
			want:      metav1.ConditionUnknown,
			apply: func(m *ConditionsMarker) {
				m.cs.SetUnknownWithReason(ResourceGraphAccepted, "Reconciling", "graph validation pending")
			},
		},
		{
			name:      "revision lineage resolved true",
			condition: RevisionLineageResolved,
			want:      metav1.ConditionTrue,
			apply: func(m *ConditionsMarker) {
				m.RevisionLineageResolved(7)
			},
		},
		{
			name:      "revision lineage resolved false",
			condition: RevisionLineageResolved,
			want:      metav1.ConditionFalse,
			apply: func(m *ConditionsMarker) {
				m.RevisionLineageFailed("lineage failed")
			},
		},
		{
			name:      "revision lineage resolved unknown",
			condition: RevisionLineageResolved,
			want:      metav1.ConditionUnknown,
			apply: func(m *ConditionsMarker) {
				m.RevisionLineagePending("Waiting", "lineage settling")
			},
		},
		{
			name:      "graph revision gc healthy true",
			condition: GraphRevisionGCHealthy,
			want:      metav1.ConditionTrue,
			apply: func(m *ConditionsMarker) {
				m.GraphRevisionGCHealthy()
			},
		},
		{
			name:      "graph revision gc healthy false",
			condition: GraphRevisionGCHealthy,
			want:      metav1.ConditionFalse,
			apply: func(m *ConditionsMarker) {
				m.GraphRevisionGCUnhealthy("gc failed")
			},
		},
		{
			name:      "graph revision gc healthy unknown",
			condition: GraphRevisionGCHealthy,
			want:      metav1.ConditionUnknown,
			apply: func(m *ConditionsMarker) {
				m.cs.SetUnknownWithReason(GraphRevisionGCHealthy, "Reconciling", "gc pending")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rgd := newTestRGD(tt.name)
			marker := NewConditionsMarkerFor(rgd)
			marker.KindReady("Network")
			marker.ControllerRunning()
			tt.apply(marker)

			cond := conditionFor(t, rgd, tt.condition)
			assert.Equal(t, tt.want, cond.Status)
			assert.True(t, rgdConditionTypes.For(rgd).IsRootReady())
			assert.Equal(t, metav1.ConditionTrue, conditionFor(t, rgd, Ready).Status)
		})
	}
}

func TestServingConditionStatusMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		condition     string
		want          metav1.ConditionStatus
		wantReady     metav1.ConditionStatus
		prepare       func(*ConditionsMarker)
		apply         func(*ConditionsMarker)
		wantRootReady bool
	}{
		{
			name:      "kind ready true",
			condition: KindReady,
			want:      metav1.ConditionTrue,
			wantReady: metav1.ConditionTrue,
			prepare: func(m *ConditionsMarker) {
				m.ControllerRunning()
			},
			apply: func(m *ConditionsMarker) {
				m.KindReady("Network")
			},
			wantRootReady: true,
		},
		{
			name:      "kind ready false",
			condition: KindReady,
			want:      metav1.ConditionFalse,
			wantReady: metav1.ConditionFalse,
			prepare: func(m *ConditionsMarker) {
				m.ControllerRunning()
			},
			apply: func(m *ConditionsMarker) {
				m.KindUnready("crd failed")
			},
			wantRootReady: false,
		},
		{
			name:      "kind ready unknown",
			condition: KindReady,
			want:      metav1.ConditionUnknown,
			wantReady: metav1.ConditionUnknown,
			prepare: func(m *ConditionsMarker) {
				m.ControllerRunning()
			},
			apply: func(m *ConditionsMarker) {
				m.cs.SetUnknownWithReason(KindReady, "Reconciling", "waiting for CRD")
			},
			wantRootReady: false,
		},
		{
			name:      "controller ready true",
			condition: ControllerReady,
			want:      metav1.ConditionTrue,
			wantReady: metav1.ConditionTrue,
			prepare: func(m *ConditionsMarker) {
				m.KindReady("Network")
			},
			apply: func(m *ConditionsMarker) {
				m.ControllerRunning()
			},
			wantRootReady: true,
		},
		{
			name:      "controller ready false",
			condition: ControllerReady,
			want:      metav1.ConditionFalse,
			wantReady: metav1.ConditionFalse,
			prepare: func(m *ConditionsMarker) {
				m.KindReady("Network")
			},
			apply: func(m *ConditionsMarker) {
				m.ControllerFailedToStart("controller boom")
			},
			wantRootReady: false,
		},
		{
			name:      "controller ready unknown",
			condition: ControllerReady,
			want:      metav1.ConditionUnknown,
			wantReady: metav1.ConditionUnknown,
			prepare: func(m *ConditionsMarker) {
				m.KindReady("Network")
			},
			apply: func(m *ConditionsMarker) {
				m.cs.SetUnknownWithReason(ControllerReady, "Reconciling", "waiting for controller")
			},
			wantRootReady: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rgd := newTestRGD(tt.name)
			marker := NewConditionsMarkerFor(rgd)
			tt.prepare(marker)
			tt.apply(marker)

			cond := conditionFor(t, rgd, tt.condition)
			assert.Equal(t, tt.want, cond.Status)
			assert.Equal(t, tt.wantReady, conditionFor(t, rgd, Ready).Status)
			assert.Equal(t, tt.wantRootReady, rgdConditionTypes.For(rgd).IsRootReady())
		})
	}
}

func TestServingHelperStatusMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		apply     func(*ConditionsMarker)
		wantKind  metav1.ConditionStatus
		wantCtrl  metav1.ConditionStatus
		wantReady metav1.ConditionStatus
	}{
		{
			name: "initialize serving conditions marks unknown",
			apply: func(m *ConditionsMarker) {
				m.InitializeServingConditions(1)
			},
			wantKind:  metav1.ConditionUnknown,
			wantCtrl:  metav1.ConditionUnknown,
			wantReady: metav1.ConditionUnknown,
		},
		{
			name: "serving pending marks both unknown",
			apply: func(m *ConditionsMarker) {
				m.ServingPending("Waiting", "pending")
			},
			wantKind:  metav1.ConditionUnknown,
			wantCtrl:  metav1.ConditionUnknown,
			wantReady: metav1.ConditionUnknown,
		},
		{
			name: "serving unknown marks both unknown",
			apply: func(m *ConditionsMarker) {
				m.ServingUnknown("Reconciling", "unknown")
			},
			wantKind:  metav1.ConditionUnknown,
			wantCtrl:  metav1.ConditionUnknown,
			wantReady: metav1.ConditionUnknown,
		},
		{
			name: "serving unavailable marks both false",
			apply: func(m *ConditionsMarker) {
				m.ServingUnavailable("Failed", "unavailable")
			},
			wantKind:  metav1.ConditionFalse,
			wantCtrl:  metav1.ConditionFalse,
			wantReady: metav1.ConditionFalse,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rgd := newTestRGD(tt.name)
			marker := NewConditionsMarkerFor(rgd)
			tt.apply(marker)

			assert.Equal(t, tt.wantKind, conditionFor(t, rgd, KindReady).Status)
			assert.Equal(t, tt.wantCtrl, conditionFor(t, rgd, ControllerReady).Status)
			assert.Equal(t, tt.wantReady, conditionFor(t, rgd, Ready).Status)
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
