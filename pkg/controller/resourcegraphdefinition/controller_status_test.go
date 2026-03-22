// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except
// in compliance with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software distributed under the License
// is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing permissions and limitations under
// the License.

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

func assertConditionExact(
	t testing.TB,
	rgd *v1alpha1.ResourceGraphDefinition,
	conditionType string,
	wantStatus metav1.ConditionStatus,
	wantReason string,
	wantMessage string,
) {
	t.Helper()

	cond := conditionFor(t, rgd, conditionType)
	require.Equal(t, v1alpha1.ConditionType(conditionType), cond.Type)
	assert.Equal(t, wantStatus, cond.Status)
	require.NotNil(t, cond.Reason)
	assert.Equal(t, wantReason, *cond.Reason)
	require.NotNil(t, cond.Message)
	assert.Equal(t, wantMessage, *cond.Message)
	assert.Equal(t, rgd.Generation, cond.ObservedGeneration)
	require.NotNil(t, cond.LastTransitionTime)
}

func markOtherConditionsReadyForGraphRevisions(m *ConditionsMarker) {
	m.ResourceGraphValid()
	m.KindReady("Network")
	m.ControllerRunning()
}

func markOtherConditionsReadyForGraphAccepted(m *ConditionsMarker) {
	m.GraphRevisionsResolved(7)
	m.KindReady("Network")
	m.ControllerRunning()
}

func markOtherConditionsReadyForKind(m *ConditionsMarker) {
	m.GraphRevisionsResolved(7)
	m.ResourceGraphValid()
	m.ControllerRunning()
}

func markOtherConditionsReadyForController(m *ConditionsMarker) {
	m.GraphRevisionsResolved(7)
	m.ResourceGraphValid()
	m.KindReady("Network")
}

func TestConditionsMarkerContracts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		prepare         func(*ConditionsMarker)
		apply           func(*ConditionsMarker)
		condition       string
		wantStatus      metav1.ConditionStatus
		wantReason      string
		wantMessage     string
		wantRootStatus  metav1.ConditionStatus
		wantRootReason  string
		wantRootMessage string
		wantRootReady   bool
	}{
		{
			name:           "graph revisions resolved",
			prepare:        markOtherConditionsReadyForGraphRevisions,
			apply:          func(m *ConditionsMarker) { m.GraphRevisionsResolved(7) },
			condition:      GraphRevisionsResolved,
			wantStatus:     metav1.ConditionTrue,
			wantReason:     "Resolved",
			wantMessage:    "revision 7 compiled and active",
			wantRootStatus: metav1.ConditionTrue,
			wantRootReason: Ready,
			wantRootReady:  true,
		},
		{
			name:            "graph revisions settling",
			prepare:         markOtherConditionsReadyForGraphRevisions,
			apply:           func(m *ConditionsMarker) { m.GraphRevisionsSettling() },
			condition:       GraphRevisionsResolved,
			wantStatus:      metav1.ConditionUnknown,
			wantReason:      waitingForGraphRevisionSettlementReason,
			wantMessage:     "waiting for terminating graph revisions to settle",
			wantRootStatus:  metav1.ConditionUnknown,
			wantRootReason:  waitingForGraphRevisionSettlementReason,
			wantRootMessage: "waiting for terminating graph revisions to settle",
		},
		{
			name:            "graph revisions warming up",
			prepare:         markOtherConditionsReadyForGraphRevisions,
			apply:           func(m *ConditionsMarker) { m.GraphRevisionsWarmingUp() },
			condition:       GraphRevisionsResolved,
			wantStatus:      metav1.ConditionUnknown,
			wantReason:      waitingForGraphRevisionWarmupReason,
			wantMessage:     "waiting for the latest graph revision to warm the in-memory registry",
			wantRootStatus:  metav1.ConditionUnknown,
			wantRootReason:  waitingForGraphRevisionWarmupReason,
			wantRootMessage: "waiting for the latest graph revision to warm the in-memory registry",
		},
		{
			name:            "graph revisions issued and compiling",
			prepare:         markOtherConditionsReadyForGraphRevisions,
			apply:           func(m *ConditionsMarker) { m.GraphRevisionsCompiling(7) },
			condition:       GraphRevisionsResolved,
			wantStatus:      metav1.ConditionUnknown,
			wantReason:      waitingForGraphRevisionCompilationReason,
			wantMessage:     "graph revision 7 issued and awaiting compilation",
			wantRootStatus:  metav1.ConditionUnknown,
			wantRootReason:  waitingForGraphRevisionCompilationReason,
			wantRootMessage: "graph revision 7 issued and awaiting compilation",
		},
		{
			name:            "graph revisions awaiting compilation",
			prepare:         markOtherConditionsReadyForGraphRevisions,
			apply:           func(m *ConditionsMarker) { m.GraphRevisionsAwaitingCompilation(7) },
			condition:       GraphRevisionsResolved,
			wantStatus:      metav1.ConditionUnknown,
			wantReason:      waitingForGraphRevisionCompilationReason,
			wantMessage:     "waiting for graph revision 7 to compile",
			wantRootStatus:  metav1.ConditionUnknown,
			wantRootReason:  waitingForGraphRevisionCompilationReason,
			wantRootMessage: "waiting for graph revision 7 to compile",
		},
		{
			name:            "graph revisions awaiting settlement",
			prepare:         markOtherConditionsReadyForGraphRevisions,
			apply:           func(m *ConditionsMarker) { m.GraphRevisionsAwaitingSettlement(7) },
			condition:       GraphRevisionsResolved,
			wantStatus:      metav1.ConditionUnknown,
			wantReason:      waitingForGraphRevisionSettlementReason,
			wantMessage:     "waiting for graph revision 7 to settle",
			wantRootStatus:  metav1.ConditionUnknown,
			wantRootReason:  waitingForGraphRevisionSettlementReason,
			wantRootMessage: "waiting for graph revision 7 to settle",
		},
		{
			name:            "graph revisions failed",
			prepare:         markOtherConditionsReadyForGraphRevisions,
			apply:           func(m *ConditionsMarker) { m.GraphRevisionsUnresolved("latest graph revision 7 failed compilation") },
			condition:       GraphRevisionsResolved,
			wantStatus:      metav1.ConditionFalse,
			wantReason:      "Failed",
			wantMessage:     "latest graph revision 7 failed compilation",
			wantRootStatus:  metav1.ConditionFalse,
			wantRootReason:  "Failed",
			wantRootMessage: "latest graph revision 7 failed compilation",
		},
		{
			name:           "graph accepted true",
			prepare:        markOtherConditionsReadyForGraphAccepted,
			apply:          func(m *ConditionsMarker) { m.ResourceGraphValid() },
			condition:      GraphAccepted,
			wantStatus:     metav1.ConditionTrue,
			wantReason:     "Valid",
			wantMessage:    "resource graph and schema are valid",
			wantRootStatus: metav1.ConditionTrue,
			wantRootReason: Ready,
			wantRootReady:  true,
		},
		{
			name:            "graph accepted false",
			prepare:         markOtherConditionsReadyForGraphAccepted,
			apply:           func(m *ConditionsMarker) { m.ResourceGraphInvalid("bad graph") },
			condition:       GraphAccepted,
			wantStatus:      metav1.ConditionFalse,
			wantReason:      "InvalidResourceGraph",
			wantMessage:     "bad graph",
			wantRootStatus:  metav1.ConditionFalse,
			wantRootReason:  "InvalidResourceGraph",
			wantRootMessage: "bad graph",
		},
		{
			name:           "kind ready",
			prepare:        markOtherConditionsReadyForKind,
			apply:          func(m *ConditionsMarker) { m.KindReady("Network") },
			condition:      KindReady,
			wantStatus:     metav1.ConditionTrue,
			wantReason:     "Ready",
			wantMessage:    "kind Network has been accepted and ready",
			wantRootStatus: metav1.ConditionTrue,
			wantRootReason: Ready,
			wantRootReady:  true,
		},
		{
			name:            "kind unready",
			prepare:         markOtherConditionsReadyForKind,
			apply:           func(m *ConditionsMarker) { m.KindUnready("crd failed") },
			condition:       KindReady,
			wantStatus:      metav1.ConditionFalse,
			wantReason:      "Failed",
			wantMessage:     "crd failed",
			wantRootStatus:  metav1.ConditionFalse,
			wantRootReason:  "Failed",
			wantRootMessage: "crd failed",
		},
		{
			name:           "controller running",
			prepare:        markOtherConditionsReadyForController,
			apply:          func(m *ConditionsMarker) { m.ControllerRunning() },
			condition:      ControllerReady,
			wantStatus:     metav1.ConditionTrue,
			wantReason:     "Running",
			wantMessage:    "controller is running",
			wantRootStatus: metav1.ConditionTrue,
			wantRootReason: Ready,
			wantRootReady:  true,
		},
		{
			name:            "controller failed to start",
			prepare:         markOtherConditionsReadyForController,
			apply:           func(m *ConditionsMarker) { m.ControllerFailedToStart("controller boom") },
			condition:       ControllerReady,
			wantStatus:      metav1.ConditionFalse,
			wantReason:      "FailedToStart",
			wantMessage:     "controller boom",
			wantRootStatus:  metav1.ConditionFalse,
			wantRootReason:  "FailedToStart",
			wantRootMessage: "controller boom",
		},
		{
			name:            "controller labeler setup failed",
			prepare:         markOtherConditionsReadyForController,
			apply:           func(m *ConditionsMarker) { m.FailedLabelerSetup("duplicate labels") },
			condition:       ControllerReady,
			wantStatus:      metav1.ConditionFalse,
			wantReason:      "FailedLabelerSetup",
			wantMessage:     "duplicate labels",
			wantRootStatus:  metav1.ConditionFalse,
			wantRootReason:  "FailedLabelerSetup",
			wantRootMessage: "duplicate labels",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rgd := newTestRGD(tt.name)
			marker := NewConditionsMarkerFor(rgd)
			if tt.prepare != nil {
				tt.prepare(marker)
			}
			tt.apply(marker)

			assertConditionExact(t, rgd, tt.condition, tt.wantStatus, tt.wantReason, tt.wantMessage)
			assertConditionExact(t, rgd, Ready, tt.wantRootStatus, tt.wantRootReason, tt.wantRootMessage)
			assert.Equal(t, tt.wantRootReady, rgdConditionTypes.For(rgd).IsRootReady())
		})
	}
}

func TestUpdateStatusStateSemantics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		apply           func(*ConditionsMarker, *v1alpha1.ResourceGraphDefinition)
		wantState       v1alpha1.ResourceGraphDefinitionState
		wantReadyStatus metav1.ConditionStatus
		wantReadyReason string
		wantReadyMsg    string
		wantLeafType    string
		wantLeafStatus  metav1.ConditionStatus
		wantLeafReason  string
		wantLeafMsg     string
	}{
		{
			name: "active while latest graph revision is still compiling",
			apply: func(m *ConditionsMarker, rgd *v1alpha1.ResourceGraphDefinition) {
				m.ResourceGraphValid()
				m.KindReady("Network")
				m.ControllerRunning()
				m.GraphRevisionsCompiling(9)
				rgd.Status.LastIssuedRevision = 9
			},
			wantState:       v1alpha1.ResourceGraphDefinitionStateActive,
			wantReadyStatus: metav1.ConditionUnknown,
			wantReadyReason: waitingForGraphRevisionCompilationReason,
			wantReadyMsg:    "graph revision 9 issued and awaiting compilation",
			wantLeafType:    GraphRevisionsResolved,
			wantLeafStatus:  metav1.ConditionUnknown,
			wantLeafReason:  waitingForGraphRevisionCompilationReason,
			wantLeafMsg:     "graph revision 9 issued and awaiting compilation",
		},
		{
			name: "inactive when the current graph is invalid",
			apply: func(m *ConditionsMarker, _ *v1alpha1.ResourceGraphDefinition) {
				m.GraphRevisionsResolved(7)
				m.KindReady("Network")
				m.ControllerRunning()
				m.ResourceGraphInvalid("bad graph")
			},
			wantState:       v1alpha1.ResourceGraphDefinitionStateInactive,
			wantReadyStatus: metav1.ConditionFalse,
			wantReadyReason: "InvalidResourceGraph",
			wantReadyMsg:    "bad graph",
			wantLeafType:    GraphAccepted,
			wantLeafStatus:  metav1.ConditionFalse,
			wantLeafReason:  "InvalidResourceGraph",
			wantLeafMsg:     "bad graph",
		},
		{
			name: "inactive when the CRD is not ready",
			apply: func(m *ConditionsMarker, _ *v1alpha1.ResourceGraphDefinition) {
				m.GraphRevisionsResolved(7)
				m.ResourceGraphValid()
				m.ControllerRunning()
				m.KindUnready("crd failed")
			},
			wantState:       v1alpha1.ResourceGraphDefinitionStateInactive,
			wantReadyStatus: metav1.ConditionFalse,
			wantReadyReason: "Failed",
			wantReadyMsg:    "crd failed",
			wantLeafType:    KindReady,
			wantLeafStatus:  metav1.ConditionFalse,
			wantLeafReason:  "Failed",
			wantLeafMsg:     "crd failed",
		},
		{
			name: "inactive when the controller is not ready",
			apply: func(m *ConditionsMarker, _ *v1alpha1.ResourceGraphDefinition) {
				m.GraphRevisionsResolved(7)
				m.ResourceGraphValid()
				m.KindReady("Network")
				m.ControllerFailedToStart("controller boom")
			},
			wantState:       v1alpha1.ResourceGraphDefinitionStateInactive,
			wantReadyStatus: metav1.ConditionFalse,
			wantReadyReason: "FailedToStart",
			wantReadyMsg:    "controller boom",
			wantLeafType:    ControllerReady,
			wantLeafStatus:  metav1.ConditionFalse,
			wantLeafReason:  "FailedToStart",
			wantLeafMsg:     "controller boom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rgd := newTestRGD(tt.name)
			marker := NewConditionsMarkerFor(rgd)
			tt.apply(marker, rgd)

			current := rgd.DeepCopy()
			current.Status = v1alpha1.ResourceGraphDefinitionStatus{}
			c := newTestClient(t, interceptor.Funcs{}, current)
			reconciler := &ResourceGraphDefinitionReconciler{Client: c}

			err := reconciler.updateStatus(context.Background(), rgd, []string{"vpc", "subnetA"}, expectedResourcesInfo())
			require.NoError(t, err)

			stored := getStoredRGD(t, c, rgd.Name)
			assert.Equal(t, tt.wantState, stored.Status.State)
			assert.Equal(t, []string{"vpc", "subnetA"}, stored.Status.TopologicalOrder)
			assert.Equal(t, expectedResourcesInfo(), stored.Status.Resources)
			assert.Equal(t, rgd.Status.LastIssuedRevision, stored.Status.LastIssuedRevision)
			assertConditionExact(t, stored, tt.wantLeafType, tt.wantLeafStatus, tt.wantLeafReason, tt.wantLeafMsg)
			assertConditionExact(t, stored, Ready, tt.wantReadyStatus, tt.wantReadyReason, tt.wantReadyMsg)
		})
	}
}

func TestUpdateStatusNoopWhenStatusMatches(t *testing.T) {
	t.Parallel()

	rgd := newTestRGD("status-noop")
	marker := NewConditionsMarkerFor(rgd)
	marker.GraphRevisionsResolved(7)
	marker.ResourceGraphValid()
	marker.KindReady("Network")
	marker.ControllerRunning()
	rgd.Status.LastIssuedRevision = 7
	rgd.Status.State = v1alpha1.ResourceGraphDefinitionStateActive
	rgd.Status.TopologicalOrder = []string{"vpc", "subnetA"}
	rgd.Status.Resources = expectedResourcesInfo()

	current := rgd.DeepCopy()
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

	reconciler := &ResourceGraphDefinitionReconciler{Client: c}
	err := reconciler.updateStatus(context.Background(), rgd, []string{"vpc", "subnetA"}, expectedResourcesInfo())
	require.NoError(t, err)
	assert.Equal(t, 0, patchCalls)
}

func TestUpdateStatusTracksStateTransition(t *testing.T) {
	t.Parallel()

	rgd := newTestRGD("status-transition")
	rgd.Status.State = v1alpha1.ResourceGraphDefinitionStateInactive
	marker := NewConditionsMarkerFor(rgd)
	marker.GraphRevisionsResolved(7)
	marker.ResourceGraphValid()
	marker.KindReady("Network")
	marker.ControllerRunning()

	c := newTestClient(t, interceptor.Funcs{}, rgd.DeepCopy())
	reconciler := &ResourceGraphDefinitionReconciler{Client: c}

	err := reconciler.updateStatus(context.Background(), rgd, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.ResourceGraphDefinitionStateActive, getStoredRGD(t, c, rgd.Name).Status.State)
}

func TestUpdateStatusErrors(t *testing.T) {
	t.Parallel()

	t.Run("wraps get error", func(t *testing.T) {
		t.Parallel()

		rgd := newTestRGD("status-get-error")
		reconciler := &ResourceGraphDefinitionReconciler{Client: newTestClient(t, interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return errors.New("get boom")
			},
		})}

		err := reconciler.updateStatus(context.Background(), rgd, nil, nil)
		require.Error(t, err)
		assert.EqualError(t, err, "failed to get current resource graph definition: get boom")
	})

	t.Run("returns status patch error", func(t *testing.T) {
		t.Parallel()

		rgd := newTestRGD("status-patch-error")
		marker := NewConditionsMarkerFor(rgd)
		marker.ResourceGraphValid()

		reconciler := &ResourceGraphDefinitionReconciler{Client: newTestClient(t, interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				return errors.New("status boom")
			},
		}, rgd.DeepCopy())}

		err := reconciler.updateStatus(context.Background(), rgd, nil, nil)
		require.Error(t, err)
		assert.EqualError(t, err, "status boom")
	})
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
