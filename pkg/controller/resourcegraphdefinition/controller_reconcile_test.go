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
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	graphhash "github.com/kubernetes-sigs/kro/pkg/graph/hash"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// newFakeClientBuilder returns a fake client builder with the field indexer
// for spec.snapshot.name pre-registered, matching the RGD selectable-field
// lookup used in tests.
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

func TestListGraphRevisions_UsesSnapshotNameSelector(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, internalv1alpha1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	revisionWithOldUID := &internalv1alpha1.GraphRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo-r00001",
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
			Name: "demo-r00002",
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

	reconciler := &ResourceGraphDefinitionReconciler{Client: cl, apiReader: cl}
	recreatedRGD := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo",
			UID:  types.UID("new-uid"),
		},
	}

	got, hasTerminating, orphans, err := reconciler.listGraphRevisions(context.Background(), recreatedRGD)
	require.NoError(t, err)
	assert.False(t, hasTerminating)
	require.Len(t, got, 2)
	// Both GRs lack an ownerRef pointing to the recreated RGD's UID.
	assert.Len(t, orphans, 2)

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
	terminatingRevision.DeletionTimestamp = new(metav1.Now())
	terminatingRevision.Finalizers = []string{"test.kro.run/finalizer"}

	cl := newFakeClientBuilder().
		WithScheme(scheme).
		WithObjects(liveRevision, terminatingRevision).
		Build()

	reconciler := &ResourceGraphDefinitionReconciler{Client: cl, apiReader: cl}

	got, hasTerminating, orphans, err := reconciler.listGraphRevisions(context.Background(), rgd)
	require.NoError(t, err)
	assert.True(t, hasTerminating)
	require.Len(t, got, 1)
	// The live GR has no ownerRef, so it's an orphan.
	assert.Len(t, orphans, 1)
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
		apiReader:         cl,
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
	require.NotNil(t, created.OwnerReferences[0].Controller)
	assert.True(t, *created.OwnerReferences[0].Controller)

	stored := &internalv1alpha1.GraphRevision{}
	err = cl.Get(context.Background(), client.ObjectKey{Name: created.Name}, stored)
	require.NoError(t, err)
	require.Len(t, stored.OwnerReferences, 1)
	assert.Equal(t, metadata.KRORGOwnerReferenceKind, stored.OwnerReferences[0].Kind)
	assert.Equal(t, metadata.KRORGOwnerReferenceAPIVersion, stored.OwnerReferences[0].APIVersion)
	assert.Equal(t, rgd.Name, stored.OwnerReferences[0].Name)
	assert.Equal(t, rgd.UID, stored.OwnerReferences[0].UID)
	require.NotNil(t, stored.OwnerReferences[0].Controller)
	assert.True(t, *stored.OwnerReferences[0].Controller)
	assert.Equal(t, "demo", stored.Labels[metadata.ResourceGraphDefinitionNameLabel])
	assert.Empty(t, stored.Labels[metadata.ResourceGraphDefinitionIDLabel])
	assert.Equal(t, "hash-3", stored.Labels[metadata.GraphRevisionHashLabel])

	_, ok := registry.Get("demo", 3)
	assert.False(t, ok, "RGD controller should not seed runtime revision state")
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
		apiReader:         cl,
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

func TestCreateGraphRevision_DoesNotOverwriteExistingRegistryEntry(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, internalv1alpha1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	cl := newFakeClientBuilder().WithScheme(scheme).Build()
	registry := revisions.NewRegistry()
	compiledGraph := &graph.Graph{TopologicalOrder: []string{"deploy"}}
	registry.Put(revisions.Entry{
		RGDName:       "demo",
		Revision:      3,
		SpecHash:      "hash-3",
		State:         revisions.RevisionStateActive,
		CompiledGraph: compiledGraph,
	})

	reconciler := &ResourceGraphDefinitionReconciler{
		Client:            cl,
		apiReader:         cl,
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
	assert.Equal(t, graphRevisionName("demo", 3), created.Name)

	entry, ok := registry.Get("demo", 3)
	require.True(t, ok)
	assert.Equal(t, revisions.RevisionStateActive, entry.State)
	assert.Same(t, compiledGraph, entry.CompiledGraph)
}

func TestCreateGraphRevision_HashLabelMergeError(t *testing.T) {
	t.Parallel()

	registry := revisions.NewRegistry()
	reconciler := &ResourceGraphDefinitionReconciler{
		metadataLabeler:   metadata.GenericLabeler{metadata.GraphRevisionHashLabel: "existing-hash"},
		revisionsRegistry: registry,
	}

	rgd := newTestRGD("demo")

	_, err := reconciler.createGraphRevision(context.Background(), rgd, 3, "hash-3")
	require.Error(t, err)
	assert.EqualError(t, err, "failed to setup graph revision hash label: duplicate labels: found key 'kro.run/graph-revision-hash' in both maps")

	_, ok := registry.Get(rgd.Name, 3)
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

func TestEnsureServingState_LabelerSetupFailure(t *testing.T) {
	t.Parallel()

	rgd := newTestRGD("rgd-serving-labeler-fail")
	reconciler := &ResourceGraphDefinitionReconciler{
		metadataLabeler: metadata.GenericLabeler{
			metadata.ResourceGraphDefinitionNameLabel: "conflict",
		},
	}
	mark := NewConditionsMarkerFor(rgd)

	topologicalOrder, resourcesInfo, err := reconciler.ensureServingState(context.Background(), rgd, newProcessedGraph(), mark)
	require.Error(t, err)
	assert.EqualError(t, err, "failed to setup labeler: duplicate labels: found key 'kro.run/resource-graph-definition-name' in both maps")
	assert.Nil(t, topologicalOrder)
	assert.Nil(t, resourcesInfo)
	assertConditionExact(t, rgd, ControllerReady, metav1.ConditionFalse, "FailedLabelerSetup", "duplicate labels: found key 'kro.run/resource-graph-definition-name' in both maps")
	assertConditionExact(t, rgd, Ready, metav1.ConditionFalse, "FailedLabelerSetup", "duplicate labels: found key 'kro.run/resource-graph-definition-name' in both maps")
}

func TestGraphRevisionName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		rgdName     string
		revision    int64
		wantExact   string // if set, assert exact match
		wantMax     int    // max length (0 = no check)
		wantHash    bool   // expect hash suffix (overflow path)
		notContains []string
	}{
		{
			name:      "simple name — no hash suffix",
			rgdName:   "demo",
			revision:  1,
			wantExact: "demo-r00001",
		},
		{
			name:      "zero-padded revision number",
			rgdName:   "demo",
			revision:  1234,
			wantExact: "demo-r01234",
		},
		{
			name:      "large revision number beyond padding",
			rgdName:   "demo",
			revision:  100000,
			wantExact: "demo-r100000",
		},
		{
			name:     "long name triggers overflow with hash suffix",
			rgdName:  strings.Repeat("a", 260),
			revision: 1,
			wantMax:  253,
			wantHash: true,
		},
		{
			name:     "different long names produce different hashes",
			rgdName:  strings.Repeat("a", 250) + "bbbbbbbbbb",
			revision: 1,
			wantMax:  253,
			wantHash: true,
		},
		{
			name:        "truncation at dot boundary trims trailing dot",
			rgdName:     strings.Repeat("a", 240) + "." + strings.Repeat("b", 20),
			revision:    1,
			wantMax:     253,
			wantHash:    true,
			notContains: []string{".-", "--"},
		},
		{
			name:        "truncation at hyphen boundary trims trailing hyphen",
			rgdName:     strings.Repeat("a", 240) + "-" + strings.Repeat("b", 20),
			revision:    1,
			wantMax:     253,
			wantHash:    true,
			notContains: []string{"--r"},
		},
		{
			name:        "truncation trims multiple trailing dots and hyphens",
			rgdName:     strings.Repeat("a", 238) + ".-." + strings.Repeat("b", 20),
			revision:    1,
			wantMax:     253,
			wantHash:    true,
			notContains: []string{".-", "--"},
		},
		{
			name:     "degenerate name of only dots and hyphens uses fallback prefix",
			rgdName:  strings.Repeat(".-", 130),
			revision: 1,
			wantMax:  253,
			wantHash: true,
		},
		{
			name:      "name at exact boundary does not trigger overflow",
			rgdName:   strings.Repeat("a", 246),
			revision:  1,
			wantExact: strings.Repeat("a", 246) + "-r00001",
		},
		{
			name:     "name one char over boundary triggers overflow",
			rgdName:  strings.Repeat("a", 247),
			revision: 1,
			wantMax:  253,
			wantHash: true,
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
			if tt.wantHash {
				// Overflow path: name contains the hash suffix after the
				// revision number (e.g. "-r00001-aefd3536").
				assert.Regexp(t, `-r\d+-[0-9a-f]{8}$`, result)
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
			apiReader:         cl,
			metadataLabeler:   metadata.NewKROMetaLabeler(),
			rgBuilder:         newTestBuilder(),
			dynamicController: newRunningDynamicController(t),
			crdManager:        manager,
			clientSet:         newKROFakeSet(),
			instanceLogger:    logr.Discard(),
			newEventRecorder:  newFakeEventRecorderFactory(),
			revisionsRegistry: registry,
			cfg: Config{
				MaxGraphRevisions:    20,
				ProgressRequeueDelay: 3 * time.Second,
			},
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
					apiReader:         cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newTestBuilder(),
					dynamicController: newRunningDynamicController(t),
					crdManager:        manager,
					clientSet:         newKROFakeSet(),
					instanceLogger:    logr.Discard(),
					newEventRecorder:  newFakeEventRecorderFactory(),
					revisionsRegistry: registry,
					cfg: Config{
						MaxGraphRevisions:    20,
						ProgressRequeueDelay: 3 * time.Second,
					},
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
				assert.True(t, conditionFor(t, rgd, GraphAccepted).IsTrue())
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
					apiReader:         cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newFailingBuilder(errors.New("naming convention violation")),
					revisionsRegistry: revisions.NewRegistry(),
					cfg: Config{
						MaxGraphRevisions:    20,
						ProgressRequeueDelay: 3 * time.Second,
					},
				}, rgd, nil
			},
			check: func(t *testing.T, result ctrl.Result, topologicalOrder []string, resourcesInfo []v1alpha1.ResourceInformation, err error, rgd *v1alpha1.ResourceGraphDefinition, _ *stubCRDManager) {
				require.Error(t, err)
				assert.Equal(t, ctrl.Result{}, result)
				assert.Nil(t, topologicalOrder)
				assert.Nil(t, resourcesInfo)

				var graphErr *graphError
				require.ErrorAs(t, err, &graphErr)
				assert.True(t, conditionFor(t, rgd, GraphAccepted).IsFalse())
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
					Client:    cl,
					apiReader: cl,
					metadataLabeler: metadata.GenericLabeler{
						metadata.ResourceGraphDefinitionNameLabel: "conflict",
					},
					rgBuilder:         newTestBuilder(),
					revisionsRegistry: revisions.NewRegistry(),
					cfg: Config{
						MaxGraphRevisions:    20,
						ProgressRequeueDelay: 3 * time.Second,
					},
				}, rgd, nil
			},
			check: func(t *testing.T, result ctrl.Result, topologicalOrder []string, resourcesInfo []v1alpha1.ResourceInformation, err error, rgd *v1alpha1.ResourceGraphDefinition, _ *stubCRDManager) {
				require.Error(t, err)
				assert.Equal(t, ctrl.Result{}, result)
				assert.Nil(t, topologicalOrder)
				assert.Nil(t, resourcesInfo)
				assert.Contains(t, err.Error(), "failed to setup graph revision labels")
				assertConditionExact(t, rgd, GraphAccepted, metav1.ConditionFalse, "InvalidResourceGraph", "failed to setup graph revision labels: duplicate labels: found key 'kro.run/resource-graph-definition-name' in both maps")
				assertConditionExact(t, rgd, Ready, metav1.ConditionFalse, "InvalidResourceGraph", "failed to setup graph revision labels: duplicate labels: found key 'kro.run/resource-graph-definition-name' in both maps")
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
					apiReader:         cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newTestBuilder(),
					dynamicController: newRunningDynamicController(t),
					crdManager:        manager,
					clientSet:         newKROFakeSet(),
					instanceLogger:    logr.Discard(),
					newEventRecorder:  newFakeEventRecorderFactory(),
					revisionsRegistry: registry,
					cfg: Config{
						MaxGraphRevisions:    20,
						ProgressRequeueDelay: 3 * time.Second,
					},
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
					apiReader:         cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newTestBuilder(),
					dynamicController: newDynamicController(t),
					crdManager:        &stubCRDManager{},
					clientSet:         newKROFakeSet(),
					instanceLogger:    logr.Discard(),
					newEventRecorder:  newFakeEventRecorderFactory(),
					revisionsRegistry: registry,
					cfg: Config{
						MaxGraphRevisions:    20,
						ProgressRequeueDelay: 3 * time.Second,
					},
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
		apiReader:         cl,
		metadataLabeler:   metadata.NewKROMetaLabeler(),
		rgBuilder:         newTestBuilder(),
		dynamicController: newRunningDynamicController(t),
		crdManager:        &stubCRDManager{},
		clientSet:         newKROFakeSet(),
		instanceLogger:    logr.Discard(),
		newEventRecorder:  newFakeEventRecorderFactory(),
		revisionsRegistry: registry,
		cfg: Config{
			MaxGraphRevisions:    20,
			ProgressRequeueDelay: 3 * time.Second,
		},
	}

	// GC failure is best-effort: it increments a metric but does not produce
	// an error or block the reconcile from succeeding.
	result, topologicalOrder, resourcesInfo, err := reconciler.reconcileResourceGraphDefinition(context.Background(), rgd)
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
	assert.Equal(t, []string{"vpc", "subnetA", "subnetB"}, topologicalOrder)
	assert.Equal(t, expectedResourcesInfo(), resourcesInfo)
	assert.True(t, conditionFor(t, rgd, GraphAccepted).IsTrue())
	assert.True(t, conditionFor(t, rgd, KindReady).IsTrue())
	assert.True(t, conditionFor(t, rgd, ControllerReady).IsTrue())
	assert.True(t, rgdConditionTypes.For(rgd).IsRootReady())
}

func TestReconcileResourceGraphDefinitionEarlyFailures(t *testing.T) {
	t.Parallel()

	t.Run("marks graph invalid when spec hashing fails", func(t *testing.T) {
		t.Parallel()

		rgd := newTestRGD("rgd-hash-error")
		rgd.Spec.Schema.Spec.Raw = []byte("{")

		reconciler := &ResourceGraphDefinitionReconciler{
			revisionsRegistry: revisions.NewRegistry(),
			cfg: Config{
				ProgressRequeueDelay: 3 * time.Second,
			},
		}

		result, topologicalOrder, resourcesInfo, err := reconciler.reconcileResourceGraphDefinition(context.Background(), rgd)
		require.Error(t, err)
		assert.EqualError(t, err, "normalize schema.spec: parse raw extension payload: unexpected end of JSON input")
		assert.Equal(t, ctrl.Result{}, result)
		assert.Nil(t, topologicalOrder)
		assert.Nil(t, resourcesInfo)
		assertConditionExact(t, rgd, GraphAccepted, metav1.ConditionFalse, "InvalidResourceGraph", "normalize schema.spec: parse raw extension payload: unexpected end of JSON input")
		assertConditionExact(t, rgd, GraphRevisionsResolved, metav1.ConditionUnknown, "AwaitingReconciliation", "condition \"GraphRevisionsResolved\" is awaiting reconciliation")
		assertConditionExact(t, rgd, KindReady, metav1.ConditionUnknown, "AwaitingReconciliation", "condition \"KindReady\" is awaiting reconciliation")
		assertConditionExact(t, rgd, ControllerReady, metav1.ConditionUnknown, "AwaitingReconciliation", "condition \"ControllerReady\" is awaiting reconciliation")
		assertConditionExact(t, rgd, Ready, metav1.ConditionFalse, "InvalidResourceGraph", "normalize schema.spec: parse raw extension payload: unexpected end of JSON input")
	})

	t.Run("returns list failures with untouched unknown conditions", func(t *testing.T) {
		t.Parallel()

		rgd := newTestRGD("rgd-list-error")
		cl := newTestClient(t, interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return errors.New("list boom")
			},
		})
		reconciler := &ResourceGraphDefinitionReconciler{
			Client:            cl,
			apiReader:         cl,
			revisionsRegistry: revisions.NewRegistry(),
			cfg: Config{
				ProgressRequeueDelay: 3 * time.Second,
			},
		}

		result, topologicalOrder, resourcesInfo, err := reconciler.reconcileResourceGraphDefinition(context.Background(), rgd)
		require.Error(t, err)
		assert.EqualError(t, err, "listing graph revisions: list boom")
		assert.Equal(t, ctrl.Result{}, result)
		assert.Nil(t, topologicalOrder)
		assert.Nil(t, resourcesInfo)
		assertConditionExact(t, rgd, GraphAccepted, metav1.ConditionUnknown, "AwaitingReconciliation", "condition \"GraphAccepted\" is awaiting reconciliation")
		assertConditionExact(t, rgd, GraphRevisionsResolved, metav1.ConditionUnknown, "AwaitingReconciliation", "condition \"GraphRevisionsResolved\" is awaiting reconciliation")
		assertConditionExact(t, rgd, KindReady, metav1.ConditionUnknown, "AwaitingReconciliation", "condition \"KindReady\" is awaiting reconciliation")
		assertConditionExact(t, rgd, ControllerReady, metav1.ConditionUnknown, "AwaitingReconciliation", "condition \"ControllerReady\" is awaiting reconciliation")
		assertConditionExact(t, rgd, Ready, metav1.ConditionUnknown, "AwaitingReconciliation", "condition \"GraphRevisionsResolved\" is awaiting reconciliation")
	})
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
					apiReader:         cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newFailingBuilder(errors.New("builder should not be called before warmup")),
					revisionsRegistry: registry,
					cfg: Config{
						MaxGraphRevisions:    20,
						ProgressRequeueDelay: 3 * time.Second,
					},
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
				assert.Equal(t, 3*time.Second, result.RequeueAfter)
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
					apiReader:         cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newFailingBuilder(errors.New("builder should not be called when cache is hot")),
					dynamicController: newRunningDynamicController(t),
					crdManager:        &stubCRDManager{},
					clientSet:         newKROFakeSet(),
					instanceLogger:    logr.Discard(),
					newEventRecorder:  newFakeEventRecorderFactory(),
					revisionsRegistry: registry,
					cfg: Config{
						MaxGraphRevisions:    20,
						ProgressRequeueDelay: 3 * time.Second,
					},
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
				assert.True(t, conditionFor(t, rgd, GraphAccepted).IsTrue())

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
					apiReader:         cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newFailingBuilder(errors.New("builder should not be called while latest revision is pending")),
					revisionsRegistry: registry,
					cfg: Config{
						MaxGraphRevisions:    20,
						ProgressRequeueDelay: 3 * time.Second,
					},
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
				assert.Equal(t, 3*time.Second, result.RequeueAfter)
				assert.Equal(t, rgd.Status.TopologicalOrder, topologicalOrder)
				assert.Equal(t, rgd.Status.Resources, resourcesInfo)
				assert.Equal(t, int64(4), rgd.Status.LastIssuedRevision)
				assertConditionExact(t, rgd, GraphRevisionsResolved, metav1.ConditionUnknown, "WaitingForGraphRevisionCompilation", "waiting for graph revision 4 to compile")
				assert.True(t, conditionFor(t, rgd, Ready).IsUnknown(), "Ready must be Unknown while revision is pending")
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
					apiReader:         cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newFailingBuilder(errors.New("builder should not be called when latest revision failed")),
					revisionsRegistry: registry,
					cfg: Config{
						MaxGraphRevisions:    20,
						ProgressRequeueDelay: 3 * time.Second,
					},
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
				assert.True(t, conditionFor(t, rgd, GraphAccepted).IsFalse())
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
					apiReader:         cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newTestBuilder(),
					dynamicController: newRunningDynamicController(t),
					crdManager:        &stubCRDManager{},
					clientSet:         newKROFakeSet(),
					instanceLogger:    logr.Discard(),
					newEventRecorder:  newFakeEventRecorderFactory(),
					revisionsRegistry: registry,
					cfg: Config{
						MaxGraphRevisions:    20,
						ProgressRequeueDelay: 3 * time.Second,
					},
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
				assert.Equal(t, 3*time.Second, result.RequeueAfter)
				assert.Nil(t, topologicalOrder)
				assert.Nil(t, resourcesInfo)
				assert.Equal(t, int64(10), rgd.Status.LastIssuedRevision)

				revisionList := &internalv1alpha1.GraphRevisionList{}
				require.NoError(t, reconciler.Client.List(context.Background(), revisionList))
				require.Len(t, revisionList.Items, 2)
				assert.Contains(t, []int64{revisionList.Items[0].Spec.Revision, revisionList.Items[1].Spec.Revision}, int64(10))

				_, ok := reconciler.revisionsRegistry.Get(rgd.Name, 10)
				assert.False(t, ok, "RGD controller should wait for the GR controller to warm the registry")
				assertConditionExact(t, rgd, GraphRevisionsResolved, metav1.ConditionUnknown, "WaitingForGraphRevisionCompilation", "graph revision 10 issued and awaiting compilation")
				assertConditionExact(t, rgd, GraphAccepted, metav1.ConditionTrue, "Valid", "resource graph and schema are valid")
				assertConditionExact(t, rgd, Ready, metav1.ConditionUnknown, "WaitingForGraphRevisionCompilation", "graph revision 10 issued and awaiting compilation")
			},
		},
		{
			name: "re-issues a revision when the latest graph revision is deleted",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition) {
				t.Helper()

				rgd := newTestRGD("rgd-latest-deleted")
				rgd.Status.LastIssuedRevision = 5

				// No GRs in the fake client — the latest was deleted.
				// Registry still has the stale entry from the old revision.
				cl := newTestClient(t, interceptor.Funcs{})
				registry := revisions.NewRegistry()
				registry.Put(revisions.Entry{
					RGDName:       rgd.Name,
					Revision:      5,
					SpecHash:      "old-hash",
					State:         revisions.RevisionStateActive,
					CompiledGraph: newProcessedGraph(),
				})

				return &ResourceGraphDefinitionReconciler{
					Client:            cl,
					apiReader:         cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newTestBuilder(),
					revisionsRegistry: registry,
					cfg: Config{
						MaxGraphRevisions:    20,
						ProgressRequeueDelay: 3 * time.Second,
					},
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
				assert.Equal(t, 3*time.Second, result.RequeueAfter)

				// Should issue revision 6 (LastIssuedRevision 5 + 1).
				assert.Equal(t, int64(6), rgd.Status.LastIssuedRevision)

				revisionList := &internalv1alpha1.GraphRevisionList{}
				require.NoError(t, reconciler.Client.List(context.Background(), revisionList))
				require.Len(t, revisionList.Items, 1)
				assert.Equal(t, int64(6), revisionList.Items[0].Spec.Revision)

				// Stale registry entries should have been cleared.
				_, ok := reconciler.revisionsRegistry.Get(rgd.Name, 5)
				assert.False(t, ok, "stale registry entry should be cleared when no live revisions exist")

				// New revision should not be in registry — GR controller owns that.
				_, ok = reconciler.revisionsRegistry.Get(rgd.Name, 6)
				assert.False(t, ok, "RGD controller should not seed the registry for newly issued revisions")

				assertConditionExact(t, rgd, GraphRevisionsResolved, metav1.ConditionUnknown, "WaitingForGraphRevisionCompilation", "graph revision 6 issued and awaiting compilation")
				assertConditionExact(t, rgd, GraphAccepted, metav1.ConditionTrue, "Valid", "resource graph and schema are valid")
				assert.True(t, conditionFor(t, rgd, Ready).IsUnknown(), "Ready must be Unknown while revision is compiling")
			},
		},
		{
			name: "marks CreateGraphRevisionFailed when revision creation fails",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, *v1alpha1.ResourceGraphDefinition) {
				t.Helper()

				rgd := newTestRGD("rgd-create-fail")

				cl := newTestClient(t, interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						if _, ok := obj.(*internalv1alpha1.GraphRevision); ok {
							return errors.New("api server unavailable")
						}
						return c.Create(ctx, obj, opts...)
					},
				})
				registry := revisions.NewRegistry()

				return &ResourceGraphDefinitionReconciler{
					Client:            cl,
					apiReader:         cl,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newTestBuilder(),
					revisionsRegistry: registry,
					cfg: Config{
						MaxGraphRevisions:    20,
						ProgressRequeueDelay: 3 * time.Second,
					},
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
				assert.ErrorContains(t, err, "api server unavailable")
				assert.Equal(t, ctrl.Result{}, result)
				assertConditionExact(t, rgd, GraphAccepted, metav1.ConditionFalse, "InvalidResourceGraph", "creating graph revision \"rgd-create-fail-r00001\": api server unavailable")
				assertConditionExact(t, rgd, GraphRevisionsResolved, metav1.ConditionUnknown, "AwaitingReconciliation", "condition \"GraphRevisionsResolved\" is awaiting reconciliation")
				assertConditionExact(t, rgd, Ready, metav1.ConditionFalse, "InvalidResourceGraph", "creating graph revision \"rgd-create-fail-r00001\": api server unavailable")
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

func TestResolveGraphRevisions_RequeuesWhenRevisionExistsButNotInRegistry(t *testing.T) {
	t.Parallel()

	rgd := newTestRGD("rgd-not-in-registry")

	revision := &internalv1alpha1.GraphRevision{
		ObjectMeta: metav1.ObjectMeta{Name: "rgd-not-in-registry-r3-abc"},
		Spec:       internalv1alpha1.GraphRevisionSpec{Revision: 3},
	}

	// warmed=true but RuntimeEntry=nil: the revision exists in the informer
	// and was found by getLatestGraphRevisionView, but the GR controller
	// hasn't processed it into the registry yet.
	view := latestGraphRevisionView{
		RevisionNumber: 3,
		Revision:       revision,
		RuntimeEntry:   nil,
	}

	reconciler := &ResourceGraphDefinitionReconciler{}
	mark := NewConditionsMarkerFor(rgd)

	err := reconciler.resolveGraphRevisions(rgd, "some-hash", view, false, true, mark)

	require.ErrorIs(t, err, errGraphRevisionsNotResolved)
	assert.Equal(t, int64(3), rgd.Status.LastIssuedRevision)

	cond := conditionFor(t, rgd, GraphRevisionsResolved)
	require.NotNil(t, cond)
	assert.True(t, cond.IsUnknown())
	require.NotNil(t, cond.Reason)
	assert.Equal(t, "WaitingForGraphRevisionWarmup", *cond.Reason)
}

func TestResolveGraphRevisions_RequeuesWhenRevisionStateIsUnknown(t *testing.T) {
	t.Parallel()

	rgd := newTestRGD("rgd-unknown-runtime-state")
	revision := &internalv1alpha1.GraphRevision{
		ObjectMeta: metav1.ObjectMeta{Name: "rgd-unknown-runtime-state-r6"},
		Spec:       internalv1alpha1.GraphRevisionSpec{Revision: 6},
	}
	view := latestGraphRevisionView{
		RevisionNumber: 6,
		Revision:       revision,
		RuntimeEntry: &revisions.Entry{
			RGDName:  rgd.Name,
			Revision: 6,
			SpecHash: "same-hash",
			State:    revisions.RevisionState("Unknown"),
		},
	}

	reconciler := &ResourceGraphDefinitionReconciler{}
	mark := NewConditionsMarkerFor(rgd)

	err := reconciler.resolveGraphRevisions(rgd, "same-hash", view, false, true, mark)

	require.ErrorIs(t, err, errGraphRevisionsNotResolved)
	assert.Equal(t, int64(6), rgd.Status.LastIssuedRevision)
	assertConditionExact(t, rgd, GraphRevisionsResolved, metav1.ConditionUnknown, waitingForGraphRevisionSettlementReason, "waiting for graph revision 6 to settle")
	assertConditionExact(t, rgd, Ready, metav1.ConditionUnknown, "AwaitingReconciliation", "condition \"GraphAccepted\" is awaiting reconciliation")
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
		apiReader:         cl,
		metadataLabeler:   metadata.NewKROMetaLabeler(),
		rgBuilder:         newTestBuilder(),
		dynamicController: newRunningDynamicController(t),
		crdManager:        &stubCRDManager{},
		clientSet:         newKROFakeSet(),
		instanceLogger:    logr.Discard(),
		newEventRecorder:  newFakeEventRecorderFactory(),
		revisionsRegistry: registry,
		cfg: Config{
			MaxGraphRevisions:    20,
			ProgressRequeueDelay: 3 * time.Second,
		},
	}

	result, _, _, reconcileErr := reconciler.reconcileResourceGraphDefinition(context.Background(), rgd)
	require.NoError(t, reconcileErr)
	assert.Equal(t, 3*time.Second, result.RequeueAfter)
	assert.Equal(t, int64(1), rgd.Status.LastIssuedRevision)

	revisionList := &internalv1alpha1.GraphRevisionList{}
	require.NoError(t, reconciler.Client.List(context.Background(), revisionList))
	require.Len(t, revisionList.Items, 1)
	assert.Equal(t, int64(1), revisionList.Items[0].Spec.Revision)

	_, ok := reconciler.revisionsRegistry.Latest(rgd.Name)
	assert.False(t, ok, "newly issued revisions should not appear in the registry until the GR controller processes them")
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
				gr.DeletionTimestamp = new(metav1.Now())
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
				apiReader:         cl,
				metadataLabeler:   metadata.NewKROMetaLabeler(),
				rgBuilder:         newTestBuilder(),
				dynamicController: newRunningDynamicController(t),
				crdManager:        &stubCRDManager{},
				clientSet:         newKROFakeSet(),
				instanceLogger:    logr.Discard(),
				newEventRecorder:  newFakeEventRecorderFactory(),
				revisionsRegistry: registry,
				cfg: Config{
					MaxGraphRevisions:    20,
					ProgressRequeueDelay: 3 * time.Second,
				},
			}

			result, _, _, reconcileErr := reconciler.reconcileResourceGraphDefinition(context.Background(), rgd)

			if tt.wantRequeue {
				require.NoError(t, reconcileErr)
				assert.Equal(t, 3*time.Second, result.RequeueAfter,
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

				_, ok := registry.Get(rgd.Name, 1)
				assert.False(t, ok, "RGD issuance should not seed runtime registry state")
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
		apiReader:         cl,
		metadataLabeler:   metadata.NewKROMetaLabeler(),
		rgBuilder:         newFailingBuilder(errors.New("builder should not be called when hash matches latest revision")),
		dynamicController: newRunningDynamicController(t),
		crdManager:        &stubCRDManager{},
		clientSet:         newKROFakeSet(),
		instanceLogger:    logr.Discard(),
		newEventRecorder:  newFakeEventRecorderFactory(),
		revisionsRegistry: registry,
		cfg: Config{
			MaxGraphRevisions:    20,
			ProgressRequeueDelay: 3 * time.Second,
		},
	}

	result, topologicalOrder, resourcesInfo, err := reconciler.reconcileResourceGraphDefinition(context.Background(), rgd)
	require.Error(t, err)
	assert.ErrorContains(t, err, "latest graph revision 6 failed")
	assert.Equal(t, ctrl.Result{}, result)
	assert.Equal(t, []string{"existing-order"}, topologicalOrder)
	assert.Equal(t, []v1alpha1.ResourceInformation{{ID: "existing-resource"}}, resourcesInfo)
	assert.True(t, conditionFor(t, rgd, GraphAccepted).IsFalse())
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
	assert.True(t, conditionFor(t, rgd, GraphAccepted).IsTrue())
	assert.True(t, conditionFor(t, rgd, KindReady).IsTrue())
	assert.True(t, conditionFor(t, rgd, ControllerReady).IsTrue())
	assert.Equal(t, revision, rgd.Status.LastIssuedRevision)

	revisionList := &internalv1alpha1.GraphRevisionList{}
	require.NoError(t, reconciler.Client.List(context.Background(), revisionList))
	require.Len(t, revisionList.Items, 1)
	assert.Equal(t, revision, revisionList.Items[0].Spec.Revision)
}

func TestGarbageCollectGraphRevisionsPrunesObjects(t *testing.T) {
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
		apiReader:         cl,
		revisionsRegistry: registry,
		cfg: Config{
			MaxGraphRevisions:    2,
			ProgressRequeueDelay: 3 * time.Second,
		},
	}

	require.NoError(t, reconciler.garbageCollectGraphRevisions(context.Background(), rgd))

	// GC deletes old API objects but does not evict from the registry.
	// The GR controller's finalizer handles registry eviction as each
	// revision is fully deleted.
	revisionList := &internalv1alpha1.GraphRevisionList{}
	require.NoError(t, cl.List(context.Background(), revisionList))
	require.Len(t, revisionList.Items, 2)
	assert.ElementsMatch(t, []int64{3, 4}, []int64{revisionList.Items[0].Spec.Revision, revisionList.Items[1].Spec.Revision})

	// Registry entries are preserved — finalizer handles eviction.
	for revision := int64(1); revision <= 4; revision++ {
		_, ok := registry.Get(rgd.Name, revision)
		assert.True(t, ok, "registry entry %d should be preserved", revision)
	}
}

func TestGarbageCollectGraphRevisionsDeleteError(t *testing.T) {
	t.Parallel()

	rgd := newTestRGD("rgd-gc-delete-error")
	oldRevision := newListedGraphRevision(rgd, 1, "hash-1")
	newRevision := newListedGraphRevision(rgd, 2, "hash-2")
	cl := newTestClient(t, interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
			if gr, ok := obj.(*internalv1alpha1.GraphRevision); ok && gr.Name == oldRevision.Name {
				return errors.New("delete boom")
			}
			return nil
		},
	}, oldRevision, newRevision)

	reconciler := &ResourceGraphDefinitionReconciler{
		Client:    cl,
		apiReader: cl,
		cfg:       Config{MaxGraphRevisions: 1},
	}

	err := reconciler.garbageCollectGraphRevisions(context.Background(), rgd)
	require.Error(t, err)
	assert.EqualError(t, err, fmt.Sprintf("deleting graph revision %q: delete boom", oldRevision.Name))
}

func TestGarbageCollectGraphRevisionsSkipsNotFound(t *testing.T) {
	t.Parallel()

	rgd := newTestRGD("rgd-gc-notfound")
	old := newListedGraphRevision(rgd, 1, "hash-1")
	kept := newListedGraphRevision(rgd, 2, "hash-2")
	cl := newTestClient(t, interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
			if gr, ok := obj.(*internalv1alpha1.GraphRevision); ok && gr.Name == old.Name {
				return apierrors.NewNotFound(
					schema.GroupResource{Group: "internal.kro.run", Resource: "graphrevisions"}, old.Name,
				)
			}
			return nil
		},
	}, old, kept)

	reconciler := &ResourceGraphDefinitionReconciler{
		Client:    cl,
		apiReader: cl,
		cfg:       Config{MaxGraphRevisions: 1},
	}

	require.NoError(t, reconciler.garbageCollectGraphRevisions(context.Background(), rgd))
}

func TestGraphRevisionRetentionFloor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		revisions         []int64
		maxGraphRevisions int
		wantFloor         int64
		wantOK            bool
	}{
		{
			name:              "exact match: len equals max",
			revisions:         []int64{3, 4},
			maxGraphRevisions: 2,
			wantFloor:         3,
			wantOK:            true,
		},
		{
			name:              "more revisions than max",
			revisions:         []int64{1, 2, 3, 4},
			maxGraphRevisions: 2,
			wantFloor:         3,
			wantOK:            true,
		},
		{
			name:              "single revision kept",
			revisions:         []int64{5, 6, 7},
			maxGraphRevisions: 1,
			wantFloor:         7,
			wantOK:            true,
		},
		{
			name:              "unsorted input",
			revisions:         []int64{4, 1, 3, 2},
			maxGraphRevisions: 3,
			wantFloor:         2,
			wantOK:            true,
		},
		{
			name:              "empty input is invalid",
			revisions:         nil,
			maxGraphRevisions: 1,
			wantFloor:         0,
			wantOK:            false,
		},
		{
			name:              "max zero is invalid",
			revisions:         []int64{1, 2, 3},
			maxGraphRevisions: 0,
			wantFloor:         0,
			wantOK:            false,
		},
		{
			name:              "len less than max is invalid",
			revisions:         []int64{1, 2},
			maxGraphRevisions: 3,
			wantFloor:         0,
			wantOK:            false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rgd := newTestRGD("floor-test")
			grs := make([]internalv1alpha1.GraphRevision, 0, len(tt.revisions))
			for _, rev := range tt.revisions {
				grs = append(grs, *newListedGraphRevision(rgd, rev, "hash"))
			}
			got, ok := graphRevisionRetentionFloor(grs, tt.maxGraphRevisions)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantFloor, got)
		})
	}
}

func TestIsOwnedBy(t *testing.T) {
	t.Parallel()

	rgdUID := types.UID("current-uid")
	staleUID := types.UID("old-uid")
	otherUID := types.UID("other-uid")

	tests := []struct {
		name string
		gr   *internalv1alpha1.GraphRevision
		want bool
	}{
		{
			name: "no ownerReferences",
			gr:   &internalv1alpha1.GraphRevision{},
			want: false,
		},
		{
			name: "owned by current RGD",
			gr: &internalv1alpha1.GraphRevision{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						metadata.NewResourceGraphDefinitionOwnerReference("demo", rgdUID),
					},
				},
			},
			want: true,
		},
		{
			name: "stale UID from previous RGD incarnation",
			gr: &internalv1alpha1.GraphRevision{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						metadata.NewResourceGraphDefinitionOwnerReference("demo", staleUID),
					},
				},
			},
			want: false,
		},
		{
			name: "owned by a different kind with same UID",
			gr: &internalv1alpha1.GraphRevision{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							Kind:       "SomethingElse",
							APIVersion: "other.io/v1",
							UID:        rgdUID,
							Controller: &[]bool{true}[0],
						},
					},
				},
			},
			want: false,
		},
		{
			name: "non-controller RGD ref with matching UID",
			gr: &internalv1alpha1.GraphRevision{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							Kind:       metadata.KRORGOwnerReferenceKind,
							APIVersion: metadata.KRORGOwnerReferenceAPIVersion,
							UID:        rgdUID,
						},
					},
				},
			},
			want: false,
		},
		{
			name: "multiple refs, one matching",
			gr: &internalv1alpha1.GraphRevision{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{Kind: "Other", APIVersion: "v1", UID: otherUID, Controller: &[]bool{true}[0]},
						metadata.NewResourceGraphDefinitionOwnerReference("demo", rgdUID),
					},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isOwnedBy(tt.gr, rgdUID))
		})
	}
}

func TestSetRGDOwnerReference(t *testing.T) {
	t.Parallel()

	desired := metadata.NewResourceGraphDefinitionOwnerReference("demo", "new-uid")
	stale := metadata.NewResourceGraphDefinitionOwnerReference("demo", "old-uid")
	unrelated := metav1.OwnerReference{
		Kind: "Deployment", APIVersion: "apps/v1", UID: "deploy-uid",
		Controller: &[]bool{true}[0],
	}

	tests := []struct {
		name string
		refs []metav1.OwnerReference
		want []metav1.OwnerReference
	}{
		{
			name: "appends when no refs exist",
			refs: nil,
			want: []metav1.OwnerReference{desired},
		},
		{
			name: "appends when no RGD ref exists",
			refs: []metav1.OwnerReference{unrelated},
			want: []metav1.OwnerReference{unrelated, desired},
		},
		{
			name: "replaces stale RGD ref in place",
			refs: []metav1.OwnerReference{stale},
			want: []metav1.OwnerReference{desired},
		},
		{
			name: "replaces stale ref preserving other refs",
			refs: []metav1.OwnerReference{unrelated, stale},
			want: []metav1.OwnerReference{unrelated, desired},
		},
		{
			name: "replaces when desired is already present (idempotent)",
			refs: []metav1.OwnerReference{desired},
			want: []metav1.OwnerReference{desired},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := setRGDOwnerReference(tt.refs, desired)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestListGraphRevisions_DetectsOrphans(t *testing.T) {
	t.Parallel()

	rgdUID := types.UID("current-uid")
	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", UID: rgdUID},
	}

	ownedGR := newListedGraphRevision(rgd, 1, "hash-1")
	ownedGR.OwnerReferences = []metav1.OwnerReference{
		metadata.NewResourceGraphDefinitionOwnerReference("demo", rgdUID),
	}

	orphanNoRef := newListedGraphRevision(rgd, 2, "hash-2")
	// No ownerReferences — simulates orphan-policy delete.

	staleUID := types.UID("old-uid")
	orphanStaleRef := newListedGraphRevision(rgd, 3, "hash-3")
	orphanStaleRef.OwnerReferences = []metav1.OwnerReference{
		metadata.NewResourceGraphDefinitionOwnerReference("demo", staleUID),
	}

	cl := newFakeClientBuilder().
		WithObjects(ownedGR, orphanNoRef, orphanStaleRef).
		Build()
	reconciler := &ResourceGraphDefinitionReconciler{Client: cl, apiReader: cl}

	live, hasTerminating, orphans, err := reconciler.listGraphRevisions(context.Background(), rgd)
	require.NoError(t, err)
	assert.False(t, hasTerminating)
	require.Len(t, live, 3)
	// Revisions 2 (no ref) and 3 (stale ref) are orphans.
	require.Len(t, orphans, 2)
	assert.Equal(t, int64(2), live[orphans[0]].Spec.Revision)
	assert.Equal(t, int64(3), live[orphans[1]].Spec.Revision)
}

func TestListGraphRevisions_NoOrphansWhenAllOwned(t *testing.T) {
	t.Parallel()

	rgdUID := types.UID("current-uid")
	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", UID: rgdUID},
	}

	gr1 := newListedGraphRevision(rgd, 1, "hash-1")
	gr1.OwnerReferences = []metav1.OwnerReference{
		metadata.NewResourceGraphDefinitionOwnerReference("demo", rgdUID),
	}
	gr2 := newListedGraphRevision(rgd, 2, "hash-2")
	gr2.OwnerReferences = []metav1.OwnerReference{
		metadata.NewResourceGraphDefinitionOwnerReference("demo", rgdUID),
	}

	cl := newFakeClientBuilder().
		WithObjects(gr1, gr2).
		Build()
	reconciler := &ResourceGraphDefinitionReconciler{Client: cl, apiReader: cl}

	live, _, orphans, err := reconciler.listGraphRevisions(context.Background(), rgd)
	require.NoError(t, err)
	require.Len(t, live, 2)
	assert.Empty(t, orphans)
}

func TestListGraphRevisions_DoesNotCrossBoundaries(t *testing.T) {
	t.Parallel()

	rgdA := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "rgd-a", UID: "uid-a"},
	}
	rgdB := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "rgd-b", UID: "uid-b"},
	}

	// GR belonging to rgd-a.
	grA := newListedGraphRevision(rgdA, 1, "hash")
	grA.OwnerReferences = []metav1.OwnerReference{
		metadata.NewResourceGraphDefinitionOwnerReference("rgd-a", "uid-a"),
	}

	// GR belonging to rgd-b — must not appear when listing for rgd-a.
	grB := newListedGraphRevision(rgdB, 1, "hash")
	grB.OwnerReferences = []metav1.OwnerReference{
		metadata.NewResourceGraphDefinitionOwnerReference("rgd-b", "uid-b"),
	}

	cl := newFakeClientBuilder().
		WithObjects(grA, grB).
		Build()
	reconciler := &ResourceGraphDefinitionReconciler{Client: cl, apiReader: cl}

	liveA, _, orphansA, err := reconciler.listGraphRevisions(context.Background(), rgdA)
	require.NoError(t, err)
	require.Len(t, liveA, 1)
	assert.Equal(t, "rgd-a", liveA[0].Spec.Snapshot.Name)
	assert.Empty(t, orphansA)

	liveB, _, orphansB, err := reconciler.listGraphRevisions(context.Background(), rgdB)
	require.NoError(t, err)
	require.Len(t, liveB, 1)
	assert.Equal(t, "rgd-b", liveB[0].Spec.Snapshot.Name)
	assert.Empty(t, orphansB)
}

func TestAdoptGraphRevisions(t *testing.T) {
	t.Parallel()

	rgdUID := types.UID("current-uid")
	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", UID: rgdUID},
	}

	t.Run("stamps ownerRef on orphan with no refs", func(t *testing.T) {
		t.Parallel()
		gr := newListedGraphRevision(rgd, 1, "hash")
		cl := newFakeClientBuilder().WithObjects(gr).Build()
		reconciler := &ResourceGraphDefinitionReconciler{Client: cl, apiReader: cl}

		live := []internalv1alpha1.GraphRevision{*gr}
		err := reconciler.adoptGraphRevisions(
			ctrl.LoggerInto(context.Background(), logr.Discard()),
			rgd, live, []int{0},
		)
		require.NoError(t, err)

		var updated internalv1alpha1.GraphRevision
		require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Name: gr.Name}, &updated))
		require.Len(t, updated.OwnerReferences, 1)
		assert.Equal(t, rgdUID, updated.OwnerReferences[0].UID)
		assert.Equal(t, metadata.KRORGOwnerReferenceKind, updated.OwnerReferences[0].Kind)
	})

	t.Run("replaces stale RGD ownerRef", func(t *testing.T) {
		t.Parallel()
		gr := newListedGraphRevision(rgd, 2, "hash")
		gr.OwnerReferences = []metav1.OwnerReference{
			metadata.NewResourceGraphDefinitionOwnerReference("demo", "old-uid"),
		}
		cl := newFakeClientBuilder().WithObjects(gr).Build()
		reconciler := &ResourceGraphDefinitionReconciler{Client: cl, apiReader: cl}

		live := []internalv1alpha1.GraphRevision{*gr}
		err := reconciler.adoptGraphRevisions(
			ctrl.LoggerInto(context.Background(), logr.Discard()),
			rgd, live, []int{0},
		)
		require.NoError(t, err)

		var updated internalv1alpha1.GraphRevision
		require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Name: gr.Name}, &updated))
		require.Len(t, updated.OwnerReferences, 1)
		assert.Equal(t, rgdUID, updated.OwnerReferences[0].UID)
	})

	t.Run("preserves non-RGD ownerRefs", func(t *testing.T) {
		t.Parallel()
		gr := newListedGraphRevision(rgd, 3, "hash")
		unrelated := metav1.OwnerReference{
			Kind: "Deployment", APIVersion: "apps/v1", UID: "deploy-uid",
		}
		gr.OwnerReferences = []metav1.OwnerReference{
			unrelated,
			metadata.NewResourceGraphDefinitionOwnerReference("demo", "old-uid"),
		}
		cl := newFakeClientBuilder().WithObjects(gr).Build()
		reconciler := &ResourceGraphDefinitionReconciler{Client: cl, apiReader: cl}

		live := []internalv1alpha1.GraphRevision{*gr}
		err := reconciler.adoptGraphRevisions(
			ctrl.LoggerInto(context.Background(), logr.Discard()),
			rgd, live, []int{0},
		)
		require.NoError(t, err)

		var updated internalv1alpha1.GraphRevision
		require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Name: gr.Name}, &updated))
		require.Len(t, updated.OwnerReferences, 2)
		assert.Equal(t, "Deployment", updated.OwnerReferences[0].Kind)
		assert.Equal(t, rgdUID, updated.OwnerReferences[1].UID)
	})

	t.Run("adopts multiple orphans", func(t *testing.T) {
		t.Parallel()
		gr1 := newListedGraphRevision(rgd, 4, "hash")
		gr2 := newListedGraphRevision(rgd, 5, "hash")
		cl := newFakeClientBuilder().WithObjects(gr1, gr2).Build()
		reconciler := &ResourceGraphDefinitionReconciler{Client: cl, apiReader: cl}

		live := []internalv1alpha1.GraphRevision{*gr1, *gr2}
		err := reconciler.adoptGraphRevisions(
			ctrl.LoggerInto(context.Background(), logr.Discard()),
			rgd, live, []int{0, 1},
		)
		require.NoError(t, err)

		for _, name := range []string{gr1.Name, gr2.Name} {
			var updated internalv1alpha1.GraphRevision
			require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Name: name}, &updated))
			require.Len(t, updated.OwnerReferences, 1)
			assert.Equal(t, rgdUID, updated.OwnerReferences[0].UID)
		}
	})

	t.Run("returns error on update failure", func(t *testing.T) {
		t.Parallel()
		gr := newListedGraphRevision(rgd, 6, "hash")
		cl := newFakeClientBuilder().
			WithObjects(gr).
			WithInterceptorFuncs(interceptor.Funcs{
				Update: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.UpdateOption) error {
					return fmt.Errorf("api server gone")
				},
			}).
			Build()
		reconciler := &ResourceGraphDefinitionReconciler{Client: cl, apiReader: cl}

		live := []internalv1alpha1.GraphRevision{*gr}
		err := reconciler.adoptGraphRevisions(
			ctrl.LoggerInto(context.Background(), logr.Discard()),
			rgd, live, []int{0},
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "api server gone")
	})
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
