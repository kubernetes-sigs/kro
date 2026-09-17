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

package applyset

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

var configMapGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

// releasableOrphan is a member ConfigMap carrying the metadata kro stamps on a
// managed resource plus a label and an annotation of the author's own.
func releasableOrphan(applySetID string) *unstructured.Unstructured {
	cm := newConfigMap("orphan-cm", "default")
	cm.SetUID(types.UID("orphan-uid"))
	cm.SetResourceVersion("7")
	cm.SetLabels(map[string]string{
		ApplysetPartOfLabel:      applySetID,
		metadata.OwnedLabel:      "true",
		metadata.NodeIDLabel:     "cm",
		metadata.InstanceIDLabel: "test-parent-uid",
		"app":                    "web",
	})
	cm.SetAnnotations(map[string]string{
		metadata.ApplyOrderAnnotation:     "1",
		metadata.NodePathAnnotation:       "cm",
		metadata.DeletionPolicyAnnotation: "Detach",
		"team":                            "platform",
	})
	return cm
}

func releaseTestParent() *testParent {
	return &testParent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-instance",
			Namespace: "default",
			UID:       types.UID("test-parent-uid"),
			Annotations: map[string]string{
				ApplySetGKsAnnotation:                  "ConfigMap",
				ApplySetAdditionalNamespacesAnnotation: "default",
			},
		},
		gvk: schema.GroupVersionKind{Group: "kro.run", Version: "v1alpha1", Kind: "TestKind"},
	}
}

// Releasing has to leave the object in the cluster while dropping kro's claim
// on it, and it must stop matching the membership selector: an object that is
// still listed as a member would be rediscovered every cycle, and on teardown
// would keep the instance's finalizer in place forever.
func TestReleaseOrphan(t *testing.T) {
	ctx := t.Context()
	parent := releaseTestParent()
	applySetID := ID(parent)

	client := newFakeDynamicClient(releasableOrphan(applySetID))
	applier := New(Config{
		Client:          client,
		RESTMapper:      newTestRESTMapper(),
		Log:             logr.Discard(),
		ParentNamespace: "default",
	}, parent)

	candidates, err := applier.ListOrphans(ctx, PruneOptions{
		KeepUIDs: sets.New[types.UID](),
		Scope:    &PruneScope{GroupKinds: sets.New(schema.GroupKind{Kind: "ConfigMap"})},
	})
	require.NoError(t, err)
	require.Len(t, candidates, 1)

	result, err := applier.ReleaseOrphan(ctx, candidates[0])
	require.NoError(t, err)
	assert.True(t, result.Released)
	assert.False(t, result.Conflict)

	live, err := client.Resource(configMapGVR).Namespace("default").Get(ctx, "orphan-cm", metav1.GetOptions{})
	require.NoError(t, err, "released resource must survive in the cluster")
	assert.Equal(t, map[string]string{"app": "web"}, live.GetLabels())
	assert.Equal(t, map[string]string{"team": "platform"}, live.GetAnnotations())

	remaining, err := applier.ListOrphans(ctx, PruneOptions{
		KeepUIDs: sets.New[types.UID](),
		Scope:    &PruneScope{GroupKinds: sets.New(schema.GroupKind{Kind: "ConfigMap"})},
	})
	require.NoError(t, err)
	assert.Empty(t, remaining, "a released resource must no longer be listed as a member")
}

// A release is preconditioned on the listed resourceVersion, so an object that
// changed since listing is skipped rather than stripped. Without it a release
// could clear the labels off an object deleted and recreated under the same
// name in the meantime.
func TestReleaseOrphanConflict(t *testing.T) {
	ctx := t.Context()
	parent := releaseTestParent()
	applySetID := ID(parent)

	client := newFakeDynamicClient(releasableOrphan(applySetID))
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(
			configMapGVR.GroupResource(), "orphan-cm", assert.AnError)
	})
	applier := New(Config{
		Client:          client,
		RESTMapper:      newTestRESTMapper(),
		Log:             logr.Discard(),
		ParentNamespace: "default",
	}, parent)

	candidates, err := applier.ListOrphans(ctx, PruneOptions{
		KeepUIDs: sets.New[types.UID](),
		Scope:    &PruneScope{GroupKinds: sets.New(schema.GroupKind{Kind: "ConfigMap"})},
	})
	require.NoError(t, err)
	require.Len(t, candidates, 1)

	result, err := applier.ReleaseOrphan(ctx, candidates[0])
	require.NoError(t, err, "a concurrent change is a retry, not a failure")
	assert.True(t, result.Conflict)
	assert.False(t, result.Released)
}

// A resource somebody else already deleted is not an error: the outcome the
// release was after (kro no longer claiming it) already holds.
func TestReleaseOrphanNotFound(t *testing.T) {
	ctx := t.Context()
	parent := releaseTestParent()
	applySetID := ID(parent)

	client := newFakeDynamicClient(releasableOrphan(applySetID))
	applier := New(Config{
		Client:          client,
		RESTMapper:      newTestRESTMapper(),
		Log:             logr.Discard(),
		ParentNamespace: "default",
	}, parent)

	candidates, err := applier.ListOrphans(ctx, PruneOptions{
		KeepUIDs: sets.New[types.UID](),
		Scope:    &PruneScope{GroupKinds: sets.New(schema.GroupKind{Kind: "ConfigMap"})},
	})
	require.NoError(t, err)
	require.Len(t, candidates, 1)

	require.NoError(t, client.Resource(configMapGVR).Namespace("default").
		Delete(ctx, "orphan-cm", metav1.DeleteOptions{}))

	result, err := applier.ReleaseOrphan(ctx, candidates[0])
	require.NoError(t, err)
	assert.False(t, result.Released)
	assert.False(t, result.Conflict)
}

// An object carrying nothing of kro's is already released; the release must not
// issue a pointless write against it.
func TestReleaseOrphanNoKROMetadata(t *testing.T) {
	ctx := t.Context()
	parent := releaseTestParent()

	cm := newConfigMap("plain-cm", "default")
	cm.SetUID(types.UID("plain-uid"))
	cm.SetLabels(map[string]string{"app": "web"})

	client := newFakeDynamicClient(cm)
	var updates int
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		return false, nil, nil
	})
	applier := New(Config{
		Client:          client,
		RESTMapper:      newTestRESTMapper(),
		Log:             logr.Discard(),
		ParentNamespace: "default",
	}, parent)

	result, err := applier.ReleaseOrphan(ctx, OrphanCandidate{Object: cm, GVR: configMapGVR})
	require.NoError(t, err)
	assert.False(t, result.Released)
	assert.Zero(t, updates)
}
