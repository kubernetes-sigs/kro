// Copyright 2025 The Kubernetes Authors.
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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

func TestListGraphRevisions_UsesRGDNameLabel(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	revisionWithOldUID := &v1alpha1.GraphRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo-r1",
			Labels: map[string]string{
				metadata.ResourceGraphDefinitionNameLabel: "demo",
				metadata.ResourceGraphDefinitionIDLabel:   "old-uid",
			},
		},
		Spec: v1alpha1.GraphRevisionSpec{
			ResourceGraphDefinitionName: "demo",
			ResourceGraphDefinitionUID:  types.UID("old-uid"),
			Revision:                    1,
			SpecHash:                    "hash-1",
		},
	}
	revisionWithNewUID := &v1alpha1.GraphRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo-r2",
			Labels: map[string]string{
				metadata.ResourceGraphDefinitionNameLabel: "demo",
				metadata.ResourceGraphDefinitionIDLabel:   "new-uid",
			},
		},
		Spec: v1alpha1.GraphRevisionSpec{
			ResourceGraphDefinitionName: "demo",
			ResourceGraphDefinitionUID:  types.UID("new-uid"),
			Revision:                    2,
			SpecHash:                    "hash-2",
		},
	}
	otherRGDRevision := &v1alpha1.GraphRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: "other-r1",
			Labels: map[string]string{
				metadata.ResourceGraphDefinitionNameLabel: "other",
				metadata.ResourceGraphDefinitionIDLabel:   "old-uid",
			},
		},
		Spec: v1alpha1.GraphRevisionSpec{
			ResourceGraphDefinitionName: "other",
			ResourceGraphDefinitionUID:  types.UID("old-uid"),
			Revision:                    1,
			SpecHash:                    "other-hash",
		},
	}

	cl := fake.NewClientBuilder().
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

	got, err := reconciler.listGraphRevisions(context.Background(), recreatedRGD)
	require.NoError(t, err)
	require.Len(t, got, 2)

	byRevision := map[int64]v1alpha1.GraphRevision{}
	for _, revision := range got {
		byRevision[revision.Spec.Revision] = revision
	}

	assert.Equal(t, types.UID("old-uid"), byRevision[1].Spec.ResourceGraphDefinitionUID)
	assert.Equal(t, types.UID("new-uid"), byRevision[2].Spec.ResourceGraphDefinitionUID)
}

func TestCreateGraphRevision_SetsOwnerReferences(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
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

	stored := &v1alpha1.GraphRevision{}
	err = cl.Get(context.Background(), client.ObjectKey{Name: created.Name}, stored)
	require.NoError(t, err)
	require.Len(t, stored.OwnerReferences, 1)
	assert.Equal(t, metadata.KRORGOwnerReferenceKind, stored.OwnerReferences[0].Kind)
	assert.Equal(t, metadata.KRORGOwnerReferenceAPIVersion, stored.OwnerReferences[0].APIVersion)
	assert.Equal(t, rgd.Name, stored.OwnerReferences[0].Name)
	assert.Equal(t, rgd.UID, stored.OwnerReferences[0].UID)
	assert.Equal(t, "demo", stored.Labels[metadata.ResourceGraphDefinitionNameLabel])
	assert.Equal(t, "new-uid", stored.Labels[metadata.ResourceGraphDefinitionIDLabel])

	entry, ok := registry.Get("demo", 3)
	require.True(t, ok)
	assert.Equal(t, revisions.RevisionStatePending, entry.State)
}

func TestCreateGraphRevision_AlreadyExistsReturnsError(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	existing := &v1alpha1.GraphRevision{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-r3"},
		Spec: v1alpha1.GraphRevisionSpec{
			ResourceGraphDefinitionName: "demo",
			ResourceGraphDefinitionUID:  types.UID("new-uid"),
			Revision:                    3,
			SpecHash:                    "hash-3",
		},
	}

	cl := fake.NewClientBuilder().
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
