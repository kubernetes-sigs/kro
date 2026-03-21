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
	"fmt"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	graphhash "github.com/kubernetes-sigs/kro/pkg/graph/hash"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// newFakeClientBuilder returns a fake client builder with the field indexer
// for spec.snapshot.name pre-registered, matching the indexer registered in
// SetupWithManager.
func newFakeClientBuilder() *fake.ClientBuilder {
	scheme := runtime.NewScheme()
	_ = internalv1alpha1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(
			&internalv1alpha1.GraphRevision{},
			"spec.snapshot.name",
			func(obj client.Object) []string {
				gr, ok := obj.(*internalv1alpha1.GraphRevision)
				if !ok {
					return nil
				}
				return []string{gr.Spec.Snapshot.Name}
			},
		)
}

func TestListGraphRevisions_UsesRGDNameLabel(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, internalv1alpha1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	revisionWithOldUID := &internalv1alpha1.GraphRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo-r1",
			Labels: map[string]string{
				metadata.ResourceGraphDefinitionNameLabel: "demo",
				metadata.ResourceGraphDefinitionIDLabel:   "old-uid",
			},
		},
		Spec: internalv1alpha1.GraphRevisionSpec{
			Revision: 1,
			Snapshot: internalv1alpha1.ResourceGraphDefinitionSnapshot{
				Name: "demo",
			},
		},
	}
	revisionWithNewUID := &internalv1alpha1.GraphRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo-r2",
			Labels: map[string]string{
				metadata.ResourceGraphDefinitionNameLabel: "demo",
				metadata.ResourceGraphDefinitionIDLabel:   "new-uid",
			},
		},
		Spec: internalv1alpha1.GraphRevisionSpec{
			Revision: 2,
			Snapshot: internalv1alpha1.ResourceGraphDefinitionSnapshot{
				Name: "demo",
			},
		},
	}
	otherRGDRevision := &internalv1alpha1.GraphRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: "other-r1",
			Labels: map[string]string{
				metadata.ResourceGraphDefinitionNameLabel: "other",
				metadata.ResourceGraphDefinitionIDLabel:   "old-uid",
			},
		},
		Spec: internalv1alpha1.GraphRevisionSpec{
			Revision: 1,
			Snapshot: internalv1alpha1.ResourceGraphDefinitionSnapshot{
				Name: "other",
			},
		},
	}

	cl := newFakeClientBuilder().
		WithScheme(scheme).
		WithObjects(revisionWithOldUID, revisionWithNewUID, otherRGDRevision).
		Build()

	reconciler := &ResourceGraphDefinitionReconciler{Client: cl}
	recreatedRGD := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo",
			UID:  types.UID("new-uid"),
		},
	}

	got, hasTerminating, err := reconciler.listGraphRevisions(context.Background(), recreatedRGD)
	require.NoError(t, err)
	assert.False(t, hasTerminating)
	require.Len(t, got, 2)

	byRevision := map[int64]internalv1alpha1.GraphRevision{}
	for _, revision := range got {
		byRevision[revision.Spec.Revision] = revision
	}

	assert.Equal(t, int64(1), byRevision[1].Spec.Revision)
	assert.Equal(t, int64(2), byRevision[2].Spec.Revision)
}

func TestListGraphRevisions_SkipsTerminatingRevisions(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, internalv1alpha1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
	}
	liveRevision := newListedGraphRevision(rgd, 2, "hash-2")
	terminatingRevision := newListedGraphRevision(rgd, 1, "hash-1")
	now := metav1.Now()
	terminatingRevision.DeletionTimestamp = &now
	terminatingRevision.Finalizers = []string{"test.kro.run/finalizer"}

	cl := newFakeClientBuilder().
		WithScheme(scheme).
		WithObjects(liveRevision, terminatingRevision).
		Build()

	reconciler := &ResourceGraphDefinitionReconciler{Client: cl}

	got, hasTerminating, err := reconciler.listGraphRevisions(context.Background(), rgd)
	require.NoError(t, err)
	assert.True(t, hasTerminating)
	require.Len(t, got, 1)
	assert.Equal(t, int64(2), got[0].Spec.Revision)
}

func TestCreateGraphRevision_SetsOwnerReferences(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, internalv1alpha1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	cl := newFakeClientBuilder().WithScheme(scheme).Build()
	registry := revisions.NewRegistry()
	reconciler := &ResourceGraphDefinitionReconciler{
		Client:            cl,
		metadataLabeler:   metadata.NewKROMetaLabeler(),
		revisionsRegistry: registry,
	}

	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo",
			UID:  types.UID("new-uid"),
		},
	}

	created, err := reconciler.createGraphRevision(context.Background(), rgd, 3, "hash-3")
	require.NoError(t, err)
	require.Len(t, created.OwnerReferences, 1)
	assert.Equal(t, metadata.KRORGOwnerReferenceKind, created.OwnerReferences[0].Kind)
	assert.Equal(t, metadata.KRORGOwnerReferenceAPIVersion, created.OwnerReferences[0].APIVersion)
	assert.Equal(t, rgd.Name, created.OwnerReferences[0].Name)
	assert.Equal(t, rgd.UID, created.OwnerReferences[0].UID)

	stored := &internalv1alpha1.GraphRevision{}
	err = cl.Get(context.Background(), client.ObjectKey{Name: created.Name}, stored)
	require.NoError(t, err)
	require.Len(t, stored.OwnerReferences, 1)
	assert.Equal(t, metadata.KRORGOwnerReferenceKind, stored.OwnerReferences[0].Kind)
	assert.Equal(t, metadata.KRORGOwnerReferenceAPIVersion, stored.OwnerReferences[0].APIVersion)
	assert.Equal(t, rgd.Name, stored.OwnerReferences[0].Name)
	assert.Equal(t, rgd.UID, stored.OwnerReferences[0].UID)
	assert.Equal(t, "demo", stored.Labels[metadata.ResourceGraphDefinitionNameLabel])
	assert.Empty(t, stored.Labels[metadata.ResourceGraphDefinitionIDLabel])
	assert.Equal(t, "hash-3", stored.Labels[metadata.GraphRevisionHashLabel])

	entry, ok := registry.Get("demo", 3)
	require.True(t, ok)
	assert.Equal(t, revisions.RevisionStatePending, entry.State)
}

func TestCreateGraphRevision_AlreadyExistsReturnsError(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, internalv1alpha1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	existing := &internalv1alpha1.GraphRevision{
		ObjectMeta: metav1.ObjectMeta{Name: graphRevisionName("demo", 3)},
		Spec: internalv1alpha1.GraphRevisionSpec{
			Revision: 3,
			Snapshot: internalv1alpha1.ResourceGraphDefinitionSnapshot{
				Name: "demo",
			},
		},
	}

	cl := newFakeClientBuilder().
		WithScheme(scheme).
		WithObjects(existing).
		Build()

	registry := revisions.NewRegistry()
	reconciler := &ResourceGraphDefinitionReconciler{
		Client:            cl,
		metadataLabeler:   metadata.NewKROMetaLabeler(),
		revisionsRegistry: registry,
	}

	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo",
			UID:  types.UID("new-uid"),
		},
	}

	_, err := reconciler.createGraphRevision(context.Background(), rgd, 3, "hash-3")
	require.Error(t, err)

	_, ok := registry.Get("demo", 3)
	assert.False(t, ok)
}

func TestGetLatestGraphRevisionView(t *testing.T) {
	t.Parallel()

	reconciler := &ResourceGraphDefinitionReconciler{
		revisionsRegistry: revisions.NewRegistry(),
	}

	graphRevisions := []internalv1alpha1.GraphRevision{
		{
			Spec: internalv1alpha1.GraphRevisionSpec{
				Revision: 1,
				Snapshot: internalv1alpha1.ResourceGraphDefinitionSnapshot{
					Name: "demo",
				},
			},
		},
		{
			Spec: internalv1alpha1.GraphRevisionSpec{
				Revision: 3,
				Snapshot: internalv1alpha1.ResourceGraphDefinitionSnapshot{
					Name: "demo",
				},
			},
		},
	}

	t.Run("returns latest revision and runtime entry when cached", func(t *testing.T) {
		// Warm the cache for all revisions present in graphRevisions.
		for _, gr := range graphRevisions {
			reconciler.revisionsRegistry.Put(revisions.Entry{
				RGDName:  "demo",
				Revision: gr.Spec.Revision,
				SpecHash: fmt.Sprintf("hash-%d", gr.Spec.Revision),
				State:    revisions.RevisionStatePending,
			})
		}

		view, warmed := reconciler.getLatestGraphRevisionView("demo", graphRevisions)
		assert.True(t, warmed)
		require.NotNil(t, view.Revision)
		assert.Equal(t, int64(3), view.RevisionNumber)
		assert.Equal(t, int64(3), view.Revision.Spec.Revision)
		require.NotNil(t, view.RuntimeEntry)
		assert.Equal(t, revisions.RevisionStatePending, view.RuntimeEntry.State)
	})

	t.Run("warmed when only latest is in registry", func(t *testing.T) {
		// Only revision 3 (the latest) is in the registry. Revision 1 is
		// missing. Warmup should still return true because only the latest
		// matters for serving.
		reg := revisions.NewRegistry()
		reg.Put(revisions.Entry{
			RGDName:  "demo",
			Revision: 3,
			SpecHash: "hash-3",
			State:    revisions.RevisionStateActive,
		})
		reconciler := &ResourceGraphDefinitionReconciler{
			revisionsRegistry: reg,
		}

		view, warmed := reconciler.getLatestGraphRevisionView("demo", graphRevisions)
		assert.True(t, warmed, "should be warmed when latest revision is in registry")
		assert.Equal(t, int64(3), view.RevisionNumber)
		require.NotNil(t, view.RuntimeEntry)
		assert.Equal(t, revisions.RevisionStateActive, view.RuntimeEntry.State)
	})

	t.Run("not warmed when latest is missing but older exists", func(t *testing.T) {
		// Revision 1 is in the registry but revision 3 (the latest) is not.
		reg := revisions.NewRegistry()
		reg.Put(revisions.Entry{
			RGDName:  "demo",
			Revision: 1,
			SpecHash: "hash-1",
			State:    revisions.RevisionStateActive,
		})
		reconciler := &ResourceGraphDefinitionReconciler{
			revisionsRegistry: reg,
		}

		_, warmed := reconciler.getLatestGraphRevisionView("demo", graphRevisions)
		assert.False(t, warmed, "should not be warmed when latest revision is missing")
	})

	t.Run("returns not warmed when cache is cold", func(t *testing.T) {
		reconciler := &ResourceGraphDefinitionReconciler{
			revisionsRegistry: revisions.NewRegistry(),
		}

		_, warmed := reconciler.getLatestGraphRevisionView("demo", graphRevisions)
		assert.False(t, warmed)
	})
}

func TestGraphRevisionName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		rgdName     string
		revision    int64
		wantExact   string // if set, assert exact match
		wantMax     int    // max length (0 = no check)
		notContains []string
	}{
		{
			name:      "simple name",
			rgdName:   "demo",
			revision:  1,
			wantExact: "demo-r1-695b6c57ca75",
		},
		{
			name:      "larger revision number",
			rgdName:   "demo",
			revision:  1234,
			wantExact: "demo-r1234-695b6c57ca75",
		},
		{
			name:     "long name truncates to 253",
			rgdName:  strings.Repeat("a", 260),
			revision: 1,
			wantMax:  253,
		},
		{
			name:     "different long names produce different hashes",
			rgdName:  strings.Repeat("a", 250) + "bbbbbbbbbb",
			revision: 1,
			wantMax:  253,
		},
		{
			name:        "truncation at dot boundary trims trailing dot",
			rgdName:     strings.Repeat("a", 240) + "." + strings.Repeat("b", 20),
			revision:    1,
			wantMax:     253,
			notContains: []string{".-", "--"},
		},
		{
			name:        "truncation at hyphen boundary trims trailing hyphen",
			rgdName:     strings.Repeat("a", 240) + "-" + strings.Repeat("b", 20),
			revision:    1,
			wantMax:     253,
			notContains: []string{"--r"},
		},
		{
			name:        "truncation trims multiple trailing dots and hyphens",
			rgdName:     strings.Repeat("a", 238) + ".-." + strings.Repeat("b", 20),
			revision:    1,
			wantMax:     253,
			notContains: []string{".-", "--"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := graphRevisionName(tt.rgdName, tt.revision)
			if tt.wantExact != "" {
				assert.Equal(t, tt.wantExact, result)
			}
			if tt.wantMax > 0 {
				assert.LessOrEqual(t, len(result), tt.wantMax)
			}
			for _, s := range tt.notContains {
				assert.NotContains(t, result, s)
			}
		})
	}

	// Verify different long names produce different results (hash collision guard).
	nameA := graphRevisionName(strings.Repeat("a", 260), 1)
	nameB := graphRevisionName(strings.Repeat("a", 250)+"bbbbbbbbbb", 1)
	assert.NotEqual(t, nameA, nameB)
}

func TestBuildResourceInfo(t *testing.T) {
	t.Parallel()

	assert.Equal(t, v1alpha1.ResourceInformation{
		ID: "subnet",
		Dependencies: []v1alpha1.Dependency{
			{ID: "vpc"},
			{ID: "internetGateway"},
		},
	}, buildResourceInfo("subnet", []string{"vpc", "internetGateway"}))
}

func TestErrorWrappers(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")

	tests := []struct {
		name     string
		build    func(error) error
		assertAs func(*testing.T, error)
	}{
		{
			name:  "graph errors unwrap correctly",
			build: newGraphError,
			assertAs: func(t *testing.T, err error) {
				var graphErr *graphError
				require.ErrorAs(t, err, &graphErr)
			},
		},
		{
			name:  "crd errors unwrap correctly",
			build: newCRDError,
			assertAs: func(t *testing.T, err error) {
				var crdErr *crdError
				require.ErrorAs(t, err, &crdErr)
			},
		},
		{
			name:  "microcontroller errors unwrap correctly",
			build: newMicroControllerError,
			assertAs: func(t *testing.T, err error) {
				var microErr *microControllerError
				require.ErrorAs(t, err, &microErr)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.build(boom)
			assert.ErrorIs(t, err, boom)
			assert.Equal(t, "boom", err.Error())
			tt.assertAs(t, err)
		})
	}
}

func TestReconcileResourceGraphDefinitionGraphStableOrder(t *testing.T) {
	t.Parallel()

	reconciler := &ResourceGraphDefinitionReconciler{rgBuilder: newTestBuilder()}

	for i := 0; i < 25; i++ {
		processed, resourcesInfo, err := reconciler.buildResourceGraphDefinition(context.Background(), newTestRGD("graph-stable"))
		require.NoError(t, err)
		assert.Equal(t, []string{"vpc", "subnetA", "subnetB"}, processed.TopologicalOrder)
		assert.Equal(t, expectedResourcesInfo(), resourcesInfo)
	}
}

func TestReconcileResourceGraphDefinitionGraphWrapsBuilderErrors(t *testing.T) {
	t.Parallel()

	reconciler := &ResourceGraphDefinitionReconciler{rgBuilder: newFailingBuilder(errors.New("naming convention violation"))}

	_, _, err := reconciler.buildResourceGraphDefinition(context.Background(), newTestRGD("graph-error"))
	require.Error(t, err)

	var graphErr *graphError
	require.ErrorAs(t, err, &graphErr)
}

func TestReconcileResourceGraphDefinition(t *testing.T) {
	buildReconciler := func(
		t *testing.T,
		rgd *v1alpha1.ResourceGraphDefinition,
	) (*ResourceGraphDefinitionReconciler, *stubCRDManager) {
		t.Helper()

		scheme := runtime.NewScheme()
		require.NoError(t, internalv1alpha1.AddToScheme(scheme))
		require.NoError(t, v1alpha1.AddToScheme(scheme))

		liveRevision, registryEntry := newActiveGraphRevisionFixture(t, rgd, 1)
		cl := newFakeClientBuilder().WithScheme(scheme).WithObjects(liveRevision).Build()
		manager := &stubCRDManager{}
		registry := revisions.NewRegistry()
		registry.Put(registryEntry)

		return &ResourceGraphDefinitionReconciler{
			Client:            cl,
			metadataLabeler:   metadata.NewKROMetaLabeler(),
			rgBuilder:         newTestBuilder(),
			dynamicController: newRunningDynamicController(t),
			crdManager:        manager,
			clientSet:         newKROFakeSet(),
			instanceLogger:    logr.Discard(),
			newEventRecorder:  newFakeEventRecorderFactory(),
			revisionsRegistry: registry,
			maxGraphRevisions: 20,
		}, manager
	}

	tests := []struct {
		name  string
		build func(*testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition, *stubCRDManager)
		check func(*testing.T, ctrl.Result, []string, []v1alpha1.ResourceInformation, error, *v1alpha1.ResourceGraphDefinition, *stubCRDManager)
	}{
		{
			name: "successfully reconciles graph crd and microcontroller",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition, *stubCRDManager) {
				rgd := newTestRGD("rgd-success")
				rgd.Annotations = map[string]string{
					v1alpha1.AllowBreakingChangesAnnotation: "true",
				}

				scheme := runtime.NewScheme()
				require.NoError(t, internalv1alpha1.AddToScheme(scheme))
				require.NoError(t, v1alpha1.AddToScheme(scheme))

				liveRevision, registryEntry := newActiveGraphRevisionFixture(t, rgd, 1)
				cl := newFakeClientBuilder().WithScheme(scheme).WithObjects(liveRevision).Build()
				registry := revisions.NewRegistry()
				registry.Put(registryEntry)

				manager := &stubCRDManager{}
				return &ResourceGraphDefinitionReconciler{
					Client:            cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newTestBuilder(),
					dynamicController: newRunningDynamicController(t),
					crdManager:        manager,
					clientSet:         newKROFakeSet(),
					instanceLogger:    logr.Discard(),
					newEventRecorder:  newFakeEventRecorderFactory(),
					revisionsRegistry: registry,
					maxGraphRevisions: 20,
				}, rgd, manager
			},
			check: func(t *testing.T, result ctrl.Result, topologicalOrder []string, resourcesInfo []v1alpha1.ResourceInformation, err error, rgd *v1alpha1.ResourceGraphDefinition, manager *stubCRDManager) {
				require.NoError(t, err)
				assert.Equal(t, ctrl.Result{}, result)
				assert.Equal(t, []string{"vpc", "subnetA", "subnetB"}, topologicalOrder)
				assert.Equal(t, expectedResourcesInfo(), resourcesInfo)
				assert.True(t, manager.lastAllowBreaking)
				assert.Equal(t, "true", manager.lastEnsure.Labels[metadata.OwnedLabel])
				assert.Equal(t, rgd.Name, manager.lastEnsure.Labels[metadata.ResourceGraphDefinitionNameLabel])
				assert.Equal(t, string(rgd.UID), manager.lastEnsure.Labels[metadata.ResourceGraphDefinitionIDLabel])
				assert.True(t, conditionFor(t, rgd, ResourceGraphAccepted).IsTrue())
				assert.True(t, conditionFor(t, rgd, KindReady).IsTrue())
				assert.True(t, conditionFor(t, rgd, ControllerReady).IsTrue())
				assert.True(t, rgdConditionTypes.For(rgd).IsRootReady())
			},
		},
		{
			name: "returns graph errors and marks the graph invalid",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition, *stubCRDManager) {
				rgd := newTestRGD("rgd-graph-error")

				scheme := runtime.NewScheme()
				require.NoError(t, internalv1alpha1.AddToScheme(scheme))
				require.NoError(t, v1alpha1.AddToScheme(scheme))

				cl := newFakeClientBuilder().WithScheme(scheme).Build()
				return &ResourceGraphDefinitionReconciler{
					Client:            cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newFailingBuilder(errors.New("naming convention violation")),
					revisionsRegistry: revisions.NewRegistry(),
					maxGraphRevisions: 20,
				}, rgd, nil
			},
			check: func(t *testing.T, result ctrl.Result, topologicalOrder []string, resourcesInfo []v1alpha1.ResourceInformation, err error, rgd *v1alpha1.ResourceGraphDefinition, _ *stubCRDManager) {
				require.Error(t, err)
				assert.Equal(t, ctrl.Result{}, result)
				assert.Nil(t, topologicalOrder)
				assert.Nil(t, resourcesInfo)

				var graphErr *graphError
				require.ErrorAs(t, err, &graphErr)
				assert.True(t, conditionFor(t, rgd, ResourceGraphAccepted).IsFalse())
			},
		},
		{
			name: "returns labeler setup errors",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition, *stubCRDManager) {
				rgd := newTestRGD("rgd-labeler-error")

				scheme := runtime.NewScheme()
				require.NoError(t, internalv1alpha1.AddToScheme(scheme))
				require.NoError(t, v1alpha1.AddToScheme(scheme))

				cl := newFakeClientBuilder().WithScheme(scheme).Build()
				return &ResourceGraphDefinitionReconciler{
					Client: cl,
					metadataLabeler: metadata.GenericLabeler{
						metadata.ResourceGraphDefinitionNameLabel: "conflict",
					},
					rgBuilder:         newTestBuilder(),
					revisionsRegistry: revisions.NewRegistry(),
					maxGraphRevisions: 20,
				}, rgd, nil
			},
			check: func(t *testing.T, result ctrl.Result, topologicalOrder []string, resourcesInfo []v1alpha1.ResourceInformation, err error, rgd *v1alpha1.ResourceGraphDefinition, _ *stubCRDManager) {
				require.Error(t, err)
				assert.Equal(t, ctrl.Result{}, result)
				assert.Nil(t, topologicalOrder)
				assert.Nil(t, resourcesInfo)
				assert.Contains(t, err.Error(), "failed to setup graph revision labels")
				assert.True(t, conditionFor(t, rgd, ResourceGraphAccepted).IsTrue())
			},
		},
		{
			name: "returns crd errors and preserves graph output",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition, *stubCRDManager) {
				rgd := newTestRGD("rgd-crd-error")
				reconciler, manager := buildReconciler(t, rgd)
				manager.ensureErr = errors.New("crd boom")
				return reconciler, rgd, manager
			},
			check: func(t *testing.T, result ctrl.Result, topologicalOrder []string, resourcesInfo []v1alpha1.ResourceInformation, err error, rgd *v1alpha1.ResourceGraphDefinition, _ *stubCRDManager) {
				require.Error(t, err)
				assert.Equal(t, ctrl.Result{}, result)
				assert.Equal(t, []string{"vpc", "subnetA", "subnetB"}, topologicalOrder)
				assert.Equal(t, expectedResourcesInfo(), resourcesInfo)

				var crdErr *crdError
				require.ErrorAs(t, err, &crdErr)
				assert.True(t, conditionFor(t, rgd, KindReady).IsFalse())
			},
		},
		{
			name: "continues when the crd fetch fails after ensure",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition, *stubCRDManager) {
				rgd := newTestRGD("rgd-crd-get-error")
				scheme := runtime.NewScheme()
				require.NoError(t, internalv1alpha1.AddToScheme(scheme))
				require.NoError(t, v1alpha1.AddToScheme(scheme))

				liveRevision, registryEntry := newActiveGraphRevisionFixture(t, rgd, 1)
				cl := newFakeClientBuilder().WithScheme(scheme).WithObjects(liveRevision).Build()
				registry := revisions.NewRegistry()
				registry.Put(registryEntry)

				manager := &stubCRDManager{getErr: errors.New("crd get boom")}
				return &ResourceGraphDefinitionReconciler{
					Client:            cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newTestBuilder(),
					dynamicController: newRunningDynamicController(t),
					crdManager:        manager,
					clientSet:         newKROFakeSet(),
					instanceLogger:    logr.Discard(),
					newEventRecorder:  newFakeEventRecorderFactory(),
					revisionsRegistry: registry,
					maxGraphRevisions: 20,
				}, rgd, manager
			},
			check: func(t *testing.T, result ctrl.Result, topologicalOrder []string, resourcesInfo []v1alpha1.ResourceInformation, err error, rgd *v1alpha1.ResourceGraphDefinition, _ *stubCRDManager) {
				require.NoError(t, err)
				assert.Equal(t, ctrl.Result{}, result)
				assert.Equal(t, []string{"vpc", "subnetA", "subnetB"}, topologicalOrder)
				assert.Equal(t, expectedResourcesInfo(), resourcesInfo)
				assert.True(t, conditionFor(t, rgd, KindReady).IsFalse())
				assert.True(t, conditionFor(t, rgd, ControllerReady).IsTrue())
				assert.False(t, rgdConditionTypes.For(rgd).IsRootReady())
			},
		},
		{
			name: "returns microcontroller registration errors",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition, *stubCRDManager) {
				rgd := newTestRGD("rgd-micro-error")
				liveRevision, entry := newActiveGraphRevisionFixture(t, rgd, 1)

				scheme := runtime.NewScheme()
				require.NoError(t, internalv1alpha1.AddToScheme(scheme))
				require.NoError(t, v1alpha1.AddToScheme(scheme))

				cl := newFakeClientBuilder().WithScheme(scheme).WithObjects(liveRevision).Build()
				registry := revisions.NewRegistry()
				registry.Put(entry)
				return &ResourceGraphDefinitionReconciler{
					Client:            cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newTestBuilder(),
					dynamicController: newDynamicController(t),
					crdManager:        &stubCRDManager{},
					clientSet:         newKROFakeSet(),
					instanceLogger:    logr.Discard(),
					newEventRecorder:  newFakeEventRecorderFactory(),
					revisionsRegistry: registry,
					maxGraphRevisions: 20,
				}, rgd, nil
			},
			check: func(t *testing.T, result ctrl.Result, topologicalOrder []string, resourcesInfo []v1alpha1.ResourceInformation, err error, _ *v1alpha1.ResourceGraphDefinition, _ *stubCRDManager) {
				require.Error(t, err)
				assert.Equal(t, ctrl.Result{}, result)
				assert.Equal(t, []string{"vpc", "subnetA", "subnetB"}, topologicalOrder)
				assert.Equal(t, expectedResourcesInfo(), resourcesInfo)

				var microErr *microControllerError
				require.ErrorAs(t, err, &microErr)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reconciler, rgd, manager := tt.build(t)
			result, topologicalOrder, resourcesInfo, err := reconciler.reconcileResourceGraphDefinition(context.Background(), rgd)
			tt.check(t, result, topologicalOrder, resourcesInfo, err, rgd, manager)
		})
	}
}

func TestReconcileResourceGraphDefinitionGCFailureDoesNotBlockReady(t *testing.T) {
	t.Parallel()

	rgd := newTestRGD("rgd-gc-failure")
	liveRevision, entry := newActiveGraphRevisionFixture(t, rgd, 1)
	listCalls := 0
	cl := newTestClient(t, interceptor.Funcs{
		List: func(ctx context.Context, base client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			listCalls++
			if listCalls == 2 {
				return errors.New("gc boom")
			}
			return base.List(ctx, list, opts...)
		},
	}, liveRevision)
	registry := revisions.NewRegistry()
	registry.Put(entry)

	reconciler := &ResourceGraphDefinitionReconciler{
		Client:            cl,
		metadataLabeler:   metadata.NewKROMetaLabeler(),
		rgBuilder:         newTestBuilder(),
		dynamicController: newRunningDynamicController(t),
		crdManager:        &stubCRDManager{},
		clientSet:         newKROFakeSet(),
		instanceLogger:    logr.Discard(),
		newEventRecorder:  newFakeEventRecorderFactory(),
		revisionsRegistry: registry,
		maxGraphRevisions: 20,
	}

	// GC failure is best-effort: it increments a metric but does not produce
	// an error or block the reconcile from succeeding.
	result, topologicalOrder, resourcesInfo, err := reconciler.reconcileResourceGraphDefinition(context.Background(), rgd)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
	assert.Equal(t, []string{"vpc", "subnetA", "subnetB"}, topologicalOrder)
	assert.Equal(t, expectedResourcesInfo(), resourcesInfo)
	assert.True(t, conditionFor(t, rgd, ResourceGraphAccepted).IsTrue())
	assert.True(t, conditionFor(t, rgd, KindReady).IsTrue())
	assert.True(t, conditionFor(t, rgd, ControllerReady).IsTrue())
	assert.True(t, rgdConditionTypes.For(rgd).IsRootReady())
}

func TestReconcileResourceGraphDefinitionRevisionPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		build func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition)
		check func(
			t *testing.T,
			result ctrl.Result,
			topologicalOrder []string,
			resourcesInfo []v1alpha1.ResourceInformation,
			err error,
			reconciler *ResourceGraphDefinitionReconciler,
			rgd *v1alpha1.ResourceGraphDefinition,
		)
	}{
		{
			name: "requeues until the revision cache is warm",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition) {
				t.Helper()

				rgd := newTestRGD("rgd-cache-warm")
				rgd.Status.TopologicalOrder = []string{"cached-order"}
				rgd.Status.Resources = []v1alpha1.ResourceInformation{{ID: "cached-resource"}}

				currentSpecHash, err := graphhash.Spec(rgd.Spec)
				require.NoError(t, err)

				cl := newTestClient(
					t,
					interceptor.Funcs{},
					newListedGraphRevision(rgd, 1, currentSpecHash),
					newListedGraphRevision(rgd, 2, "old-hash"),
				)
				registry := revisions.NewRegistry()
				registry.Put(revisions.Entry{
					RGDName:  rgd.Name,
					Revision: 1,
					SpecHash: currentSpecHash,
					State:    revisions.RevisionStateActive,
				})

				return &ResourceGraphDefinitionReconciler{
					Client:            cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newFailingBuilder(errors.New("builder should not be called before warmup")),
					revisionsRegistry: registry,
					maxGraphRevisions: 20,
				}, rgd
			},
			check: func(
				t *testing.T,
				result ctrl.Result,
				topologicalOrder []string,
				resourcesInfo []v1alpha1.ResourceInformation,
				err error,
				_ *ResourceGraphDefinitionReconciler,
				rgd *v1alpha1.ResourceGraphDefinition,
			) {
				t.Helper()

				require.NoError(t, err)
				assert.Equal(t, defaultRequeueDelay, result.RequeueAfter)
				assert.Equal(t, rgd.Status.TopologicalOrder, topologicalOrder)
				assert.Equal(t, rgd.Status.Resources, resourcesInfo)
			},
		},
		{
			name: "uses cached active revision when the spec hash matches",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition) {
				t.Helper()

				rgd := newTestRGD("rgd-cached-active")
				currentSpecHash, err := graphhash.Spec(rgd.Spec)
				require.NoError(t, err)

				cl := newTestClient(t, interceptor.Funcs{}, newListedGraphRevision(rgd, 3, currentSpecHash))
				registry := revisions.NewRegistry()
				registry.Put(revisions.Entry{
					RGDName:       rgd.Name,
					Revision:      3,
					SpecHash:      currentSpecHash,
					State:         revisions.RevisionStateActive,
					CompiledGraph: newProcessedGraph(),
				})

				return &ResourceGraphDefinitionReconciler{
					Client:            cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newFailingBuilder(errors.New("builder should not be called when cache is hot")),
					dynamicController: newRunningDynamicController(t),
					crdManager:        &stubCRDManager{},
					clientSet:         newKROFakeSet(),
					instanceLogger:    logr.Discard(),
					newEventRecorder:  newFakeEventRecorderFactory(),
					revisionsRegistry: registry,
					maxGraphRevisions: 20,
				}, rgd
			},
			check: func(
				t *testing.T,
				result ctrl.Result,
				topologicalOrder []string,
				resourcesInfo []v1alpha1.ResourceInformation,
				err error,
				reconciler *ResourceGraphDefinitionReconciler,
				rgd *v1alpha1.ResourceGraphDefinition,
			) {
				t.Helper()

				require.NoError(t, err)
				assert.Equal(t, ctrl.Result{}, result)
				assert.Equal(t, []string{"vpc", "subnetA", "subnetB"}, topologicalOrder)
				assert.Equal(t, expectedResourcesInfo(), resourcesInfo)
				assert.Equal(t, int64(3), rgd.Status.LastIssuedRevision)
				assert.True(t, conditionFor(t, rgd, ResourceGraphAccepted).IsTrue())

				revisionList := &internalv1alpha1.GraphRevisionList{}
				require.NoError(t, reconciler.Client.List(context.Background(), revisionList))
				require.Len(t, revisionList.Items, 1)
			},
		},
		{
			name: "requeues while the latest matching revision is pending",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition) {
				t.Helper()

				rgd := newTestRGD("rgd-pending-latest")
				rgd.Status.TopologicalOrder = []string{"existing-order"}
				rgd.Status.Resources = []v1alpha1.ResourceInformation{{ID: "existing-resource"}}

				currentSpecHash, err := graphhash.Spec(rgd.Spec)
				require.NoError(t, err)

				cl := newTestClient(t, interceptor.Funcs{}, newListedGraphRevision(rgd, 4, currentSpecHash))
				registry := revisions.NewRegistry()
				registry.Put(revisions.Entry{
					RGDName:  rgd.Name,
					Revision: 4,
					SpecHash: currentSpecHash,
					State:    revisions.RevisionStatePending,
				})

				return &ResourceGraphDefinitionReconciler{
					Client:            cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newFailingBuilder(errors.New("builder should not be called while latest revision is pending")),
					revisionsRegistry: registry,
					maxGraphRevisions: 20,
				}, rgd
			},
			check: func(
				t *testing.T,
				result ctrl.Result,
				topologicalOrder []string,
				resourcesInfo []v1alpha1.ResourceInformation,
				err error,
				_ *ResourceGraphDefinitionReconciler,
				rgd *v1alpha1.ResourceGraphDefinition,
			) {
				t.Helper()

				require.NoError(t, err)
				assert.Equal(t, defaultRequeueDelay, result.RequeueAfter)
				assert.Equal(t, rgd.Status.TopologicalOrder, topologicalOrder)
				assert.Equal(t, rgd.Status.Resources, resourcesInfo)
				assert.Equal(t, int64(4), rgd.Status.LastIssuedRevision)
			},
		},
		{
			name: "marks the resource graph invalid when the latest matching revision failed",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition) {
				t.Helper()

				rgd := newTestRGD("rgd-failed-latest")
				rgd.Status.TopologicalOrder = []string{"existing-order"}
				rgd.Status.Resources = []v1alpha1.ResourceInformation{{ID: "existing-resource"}}

				currentSpecHash, err := graphhash.Spec(rgd.Spec)
				require.NoError(t, err)

				cl := newTestClient(t, interceptor.Funcs{}, newListedGraphRevision(rgd, 5, currentSpecHash))
				registry := revisions.NewRegistry()
				registry.Put(revisions.Entry{
					RGDName:  rgd.Name,
					Revision: 5,
					SpecHash: currentSpecHash,
					State:    revisions.RevisionStateFailed,
				})

				return &ResourceGraphDefinitionReconciler{
					Client:            cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newFailingBuilder(errors.New("builder should not be called when latest revision failed")),
					revisionsRegistry: registry,
					maxGraphRevisions: 20,
				}, rgd
			},
			check: func(
				t *testing.T,
				result ctrl.Result,
				topologicalOrder []string,
				resourcesInfo []v1alpha1.ResourceInformation,
				err error,
				_ *ResourceGraphDefinitionReconciler,
				rgd *v1alpha1.ResourceGraphDefinition,
			) {
				t.Helper()

				require.Error(t, err)
				assert.ErrorContains(t, err, "latest graph revision 5 failed")
				assert.Equal(t, ctrl.Result{}, result)
				assert.Equal(t, rgd.Status.TopologicalOrder, topologicalOrder)
				assert.Equal(t, rgd.Status.Resources, resourcesInfo)
				assert.Equal(t, int64(5), rgd.Status.LastIssuedRevision)
				assert.True(t, conditionFor(t, rgd, ResourceGraphAccepted).IsFalse())
			},
		},
		{
			name: "issues the next revision from the highest observed watermark",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition) {
				t.Helper()

				rgd := newTestRGD("rgd-watermark")
				rgd.Status.LastIssuedRevision = 7

				cl := newTestClient(t, interceptor.Funcs{}, newListedGraphRevision(rgd, 9, "old-hash"))
				registry := revisions.NewRegistry()
				registry.Put(revisions.Entry{
					RGDName:  rgd.Name,
					Revision: 9,
					SpecHash: "old-hash",
					State:    revisions.RevisionStateActive,
				})

				return &ResourceGraphDefinitionReconciler{
					Client:            cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newTestBuilder(),
					dynamicController: newRunningDynamicController(t),
					crdManager:        &stubCRDManager{},
					clientSet:         newKROFakeSet(),
					instanceLogger:    logr.Discard(),
					newEventRecorder:  newFakeEventRecorderFactory(),
					revisionsRegistry: registry,
					maxGraphRevisions: 20,
				}, rgd
			},
			check: func(
				t *testing.T,
				result ctrl.Result,
				topologicalOrder []string,
				resourcesInfo []v1alpha1.ResourceInformation,
				err error,
				reconciler *ResourceGraphDefinitionReconciler,
				rgd *v1alpha1.ResourceGraphDefinition,
			) {
				t.Helper()

				require.NoError(t, err)
				assert.Equal(t, defaultRequeueDelay, result.RequeueAfter)
				assert.Nil(t, topologicalOrder)
				assert.Nil(t, resourcesInfo)
				assert.Equal(t, int64(10), rgd.Status.LastIssuedRevision)

				revisionList := &internalv1alpha1.GraphRevisionList{}
				require.NoError(t, reconciler.Client.List(context.Background(), revisionList))
				require.Len(t, revisionList.Items, 2)
				assert.Contains(t, []int64{revisionList.Items[0].Spec.Revision, revisionList.Items[1].Spec.Revision}, int64(10))

				currentSpecHash, hashErr := graphhash.Spec(rgd.Spec)
				require.NoError(t, hashErr)

				entry, ok := reconciler.revisionsRegistry.Get(rgd.Name, 10)
				require.True(t, ok)
				assert.Equal(t, currentSpecHash, entry.SpecHash)
				assert.Equal(t, revisions.RevisionStatePending, entry.State)
				assert.True(t, conditionFor(t, rgd, RevisionLineageResolved).IsUnknown())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reconciler, rgd := tt.build(t)
			result, topologicalOrder, resourcesInfo, err := reconciler.reconcileResourceGraphDefinition(context.Background(), rgd)
			tt.check(t, result, topologicalOrder, resourcesInfo, err, reconciler, rgd)
		})
	}
}

func TestReconcileResourceGraphDefinition_RecreateWithEmptyLiveListClearsStaleRegistry(t *testing.T) {
	t.Parallel()

	rgd := newTestRGD("rgd-recreate")

	cl := newTestClient(t, interceptor.Funcs{})
	registry := revisions.NewRegistry()
	registry.Put(revisions.Entry{
		RGDName:       rgd.Name,
		Revision:      2,
		SpecHash:      "old-hash",
		State:         revisions.RevisionStateActive,
		CompiledGraph: newProcessedGraph(),
	})

	reconciler := &ResourceGraphDefinitionReconciler{
		Client:            cl,
		metadataLabeler:   metadata.NewKROMetaLabeler(),
		rgBuilder:         newTestBuilder(),
		dynamicController: newRunningDynamicController(t),
		crdManager:        &stubCRDManager{},
		clientSet:         newKROFakeSet(),
		instanceLogger:    logr.Discard(),
		newEventRecorder:  newFakeEventRecorderFactory(),
		revisionsRegistry: registry,
		maxGraphRevisions: 20,
	}

	result, _, _, reconcileErr := reconciler.reconcileResourceGraphDefinition(context.Background(), rgd)
	require.NoError(t, reconcileErr)
	assert.Equal(t, defaultRequeueDelay, result.RequeueAfter)
	assert.Equal(t, int64(1), rgd.Status.LastIssuedRevision)

	revisionList := &internalv1alpha1.GraphRevisionList{}
	require.NoError(t, reconciler.Client.List(context.Background(), revisionList))
	require.Len(t, revisionList.Items, 1)
	assert.Equal(t, int64(1), revisionList.Items[0].Spec.Revision)

	latest, ok := reconciler.revisionsRegistry.Latest(rgd.Name)
	require.True(t, ok)
	assert.Equal(t, int64(1), latest.Revision)
}

func TestReconcileResourceGraphDefinition_TerminatingRevisionsBlockReconcile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		liveRevisions       []int64
		terminatingRevs     []int64
		staleRegistry       []int64 // revision numbers pre-populated in registry
		wantRequeue         bool
		wantNewRevision     bool
		wantRegistryCleared bool
	}{
		{
			name:            "all revisions terminating causes requeue",
			terminatingRevs: []int64{1, 2},
			staleRegistry:   []int64{1, 2},
			wantRequeue:     true,
		},
		{
			name:            "mix of live and terminating causes requeue",
			liveRevisions:   []int64{3},
			terminatingRevs: []int64{1, 2},
			staleRegistry:   []int64{1, 2, 3},
			wantRequeue:     true,
		},
		{
			name:            "only terminating with stale registry causes requeue and preserves registry",
			terminatingRevs: []int64{1},
			staleRegistry:   []int64{1, 2, 3},
			wantRequeue:     true,
		},
		{
			name:                "no live no terminating clears stale registry and issues fresh revision",
			staleRegistry:       []int64{1, 2},
			wantRequeue:         false,
			wantNewRevision:     true,
			wantRegistryCleared: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rgd := newTestRGD("rgd-terminating-test")

			// Build GR objects
			objects := make(
				[]client.Object,
				0,
				len(tt.liveRevisions)+len(tt.terminatingRevs),
			)
			for _, rev := range tt.liveRevisions {
				objects = append(objects, newListedGraphRevision(rgd, rev, fmt.Sprintf("hash-%d", rev)))
			}
			for _, rev := range tt.terminatingRevs {
				gr := newListedGraphRevision(rgd, rev, fmt.Sprintf("hash-%d", rev))
				now := metav1.Now()
				gr.DeletionTimestamp = &now
				gr.Finalizers = []string{"test.kro.run/finalizer"}
				objects = append(objects, gr)
			}

			cl := newFakeClientBuilder().WithObjects(objects...).Build()

			registry := revisions.NewRegistry()
			for _, rev := range tt.staleRegistry {
				registry.Put(revisions.Entry{
					RGDName:       rgd.Name,
					Revision:      rev,
					SpecHash:      fmt.Sprintf("hash-%d", rev),
					State:         revisions.RevisionStateActive,
					CompiledGraph: newProcessedGraph(),
				})
			}

			reconciler := &ResourceGraphDefinitionReconciler{
				Client:            cl,
				metadataLabeler:   metadata.NewKROMetaLabeler(),
				rgBuilder:         newTestBuilder(),
				dynamicController: newRunningDynamicController(t),
				crdManager:        &stubCRDManager{},
				clientSet:         newKROFakeSet(),
				instanceLogger:    logr.Discard(),
				newEventRecorder:  newFakeEventRecorderFactory(),
				revisionsRegistry: registry,
				maxGraphRevisions: 20,
			}

			result, _, _, reconcileErr := reconciler.reconcileResourceGraphDefinition(context.Background(), rgd)

			if tt.wantRequeue {
				require.NoError(t, reconcileErr)
				assert.Equal(t, defaultRequeueDelay, result.RequeueAfter,
					"should requeue when terminating revisions exist")

				// Registry should NOT be cleared during requeue — GR controller
				// handles eviction as each revision finalizes.
				for _, rev := range tt.staleRegistry {
					_, ok := registry.Get(rgd.Name, rev)
					assert.True(t, ok, "registry entry %d should survive during requeue", rev)
				}
				return
			}

			require.NoError(t, reconcileErr)

			if tt.wantRegistryCleared {
				// Stale entries above the new revision should be gone.
				// Revision 1 may exist as the newly issued revision.
				for _, rev := range tt.staleRegistry {
					if rev == 1 && tt.wantNewRevision {
						continue // revision 1 was re-created by the new issuance
					}
					_, ok := registry.Get(rgd.Name, rev)
					assert.False(t, ok, "stale registry entry %d should be cleared", rev)
				}
			}

			if tt.wantNewRevision {
				revisionList := &internalv1alpha1.GraphRevisionList{}
				require.NoError(t, cl.List(context.Background(), revisionList))
				require.Len(t, revisionList.Items, 1, "should issue exactly one new revision")
				assert.Equal(t, int64(1), revisionList.Items[0].Spec.Revision,
					"new revision should start from 1 after stale cleanup")

				// The new revision 1 should be in the registry as Pending
				// (the GR controller will compile and promote it to Active).
				entry, ok := registry.Get(rgd.Name, 1)
				require.True(t, ok, "new revision 1 should be in registry")
				assert.Equal(t, revisions.RevisionStatePending, entry.State)
			}
		})
	}
}

func TestReconcileResourceGraphDefinitionRecoversWhenLatestFailedBecomesActiveWithoutSpecChange(t *testing.T) {
	t.Parallel()

	rgd := newTestRGD("rgd-failed-recovery")
	rgd.Status.TopologicalOrder = []string{"existing-order"}
	rgd.Status.Resources = []v1alpha1.ResourceInformation{{ID: "existing-resource"}}

	currentSpecHash, err := graphhash.Spec(rgd.Spec)
	require.NoError(t, err)

	const revision int64 = 6
	cl := newTestClient(t, interceptor.Funcs{}, newListedGraphRevision(rgd, revision, currentSpecHash))
	registry := revisions.NewRegistry()
	registry.Put(revisions.Entry{
		RGDName:  rgd.Name,
		Revision: revision,
		SpecHash: currentSpecHash,
		State:    revisions.RevisionStateFailed,
	})

	reconciler := &ResourceGraphDefinitionReconciler{
		Client:            cl,
		metadataLabeler:   metadata.NewKROMetaLabeler(),
		rgBuilder:         newFailingBuilder(errors.New("builder should not be called when hash matches latest revision")),
		dynamicController: newRunningDynamicController(t),
		crdManager:        &stubCRDManager{},
		clientSet:         newKROFakeSet(),
		instanceLogger:    logr.Discard(),
		newEventRecorder:  newFakeEventRecorderFactory(),
		revisionsRegistry: registry,
		maxGraphRevisions: 20,
	}

	result, topologicalOrder, resourcesInfo, err := reconciler.reconcileResourceGraphDefinition(context.Background(), rgd)
	require.Error(t, err)
	assert.ErrorContains(t, err, "latest graph revision 6 failed")
	assert.Equal(t, ctrl.Result{}, result)
	assert.Equal(t, []string{"existing-order"}, topologicalOrder)
	assert.Equal(t, []v1alpha1.ResourceInformation{{ID: "existing-resource"}}, resourcesInfo)
	assert.True(t, conditionFor(t, rgd, ResourceGraphAccepted).IsFalse())
	assert.Equal(t, revision, rgd.Status.LastIssuedRevision)

	registry.Put(revisions.Entry{
		RGDName:       rgd.Name,
		Revision:      revision,
		SpecHash:      currentSpecHash,
		State:         revisions.RevisionStateActive,
		CompiledGraph: newProcessedGraph(),
	})

	result, topologicalOrder, resourcesInfo, err = reconciler.reconcileResourceGraphDefinition(context.Background(), rgd)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
	assert.Equal(t, []string{"vpc", "subnetA", "subnetB"}, topologicalOrder)
	assert.Equal(t, expectedResourcesInfo(), resourcesInfo)
	assert.True(t, conditionFor(t, rgd, ResourceGraphAccepted).IsTrue())
	assert.True(t, conditionFor(t, rgd, KindReady).IsTrue())
	assert.True(t, conditionFor(t, rgd, ControllerReady).IsTrue())
	assert.Equal(t, revision, rgd.Status.LastIssuedRevision)

	revisionList := &internalv1alpha1.GraphRevisionList{}
	require.NoError(t, reconciler.Client.List(context.Background(), revisionList))
	require.Len(t, revisionList.Items, 1)
	assert.Equal(t, revision, revisionList.Items[0].Spec.Revision)
}

func TestGarbageCollectGraphRevisionsPrunesObjectsAndRegistry(t *testing.T) {
	t.Parallel()

	rgd := newTestRGD("rgd-gc-prune")
	cl := newTestClient(t, interceptor.Funcs{},
		newListedGraphRevision(rgd, 1, "hash-1"),
		newListedGraphRevision(rgd, 2, "hash-2"),
		newListedGraphRevision(rgd, 3, "hash-3"),
		newListedGraphRevision(rgd, 4, "hash-4"),
	)
	registry := revisions.NewRegistry()
	for revision := int64(1); revision <= 4; revision++ {
		registry.Put(revisions.Entry{
			RGDName:  rgd.Name,
			Revision: revision,
			SpecHash: "hash",
			State:    revisions.RevisionStateActive,
		})
	}

	reconciler := &ResourceGraphDefinitionReconciler{
		Client:            cl,
		revisionsRegistry: registry,
		maxGraphRevisions: 2,
	}

	require.NoError(t, reconciler.garbageCollectGraphRevisions(context.Background(), rgd))

	revisionList := &internalv1alpha1.GraphRevisionList{}
	require.NoError(t, cl.List(context.Background(), revisionList))
	require.Len(t, revisionList.Items, 2)
	assert.ElementsMatch(t, []int64{3, 4}, []int64{revisionList.Items[0].Spec.Revision, revisionList.Items[1].Spec.Revision})

	_, ok := registry.Get(rgd.Name, 1)
	assert.False(t, ok)
	_, ok = registry.Get(rgd.Name, 2)
	assert.False(t, ok)
	_, ok = registry.Get(rgd.Name, 3)
	assert.True(t, ok)
	_, ok = registry.Get(rgd.Name, 4)
	assert.True(t, ok)
}

func newListedGraphRevision(rgd *v1alpha1.ResourceGraphDefinition, revision int64, specHash string) *internalv1alpha1.GraphRevision {
	graphRevision := &internalv1alpha1.GraphRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: graphRevisionName(rgd.Name, revision),
			Labels: map[string]string{
				metadata.ResourceGraphDefinitionNameLabel: rgd.Name,
				metadata.GraphRevisionHashLabel:           metadata.NewGraphRevisionHashLabeler(specHash)[metadata.GraphRevisionHashLabel],
			},
		},
		Spec: internalv1alpha1.GraphRevisionSpec{
			Revision: revision,
			Snapshot: internalv1alpha1.ResourceGraphDefinitionSnapshot{
				Name: rgd.Name,
				Spec: *rgd.Spec.DeepCopy(),
			},
		},
	}

	return graphRevision
}
