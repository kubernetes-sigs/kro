// Copyright 2026 The Kube Resource Orchestrator Authors.
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

package executor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestRelease_DoesNotRecreateDeletedTarget pins finding 1489: releasing a
// contribution must not recreate a target that was legitimately deleted. A
// server-side Apply of an identity-only object with no live object present
// would CREATE a bare resource; the GET-first guard skips the patch instead.
func TestRelease_DoesNotRecreateDeletedTarget(t *testing.T) {
	t.Parallel()
	// Empty cluster: the release target does not exist.
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	err := NewSimple(cl).Release(context.Background(), []Contribution{{
		APIVersion:   "v1",
		Kind:         "ConfigMap",
		Namespace:    "default",
		Name:         "gone",
		FieldManager: "kro-graphengine.patch.abc.def",
	}})
	require.NoError(t, err, "releasing an absent target must be a tolerated no-op")

	// The target must NOT have been (re)created by the release apply.
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	getErr := cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gone"}, got)
	require.Error(t, getErr)
	assert.True(t, errors.IsNotFound(getErr), "release must not recreate a deleted target, got %v", getErr)
}

// TestRelease_ReleasesPresentTarget confirms the GET-first guard still allows a
// release when the target IS present (the normal path).
func TestRelease_ReleasesPresentTarget(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(liveCM("present")).Build()

	err := NewSimple(cl).Release(context.Background(), []Contribution{{
		APIVersion:   "v1",
		Kind:         "ConfigMap",
		Namespace:    "default",
		Name:         "present",
		FieldManager: "kro-graphengine.patch.abc.def",
	}})
	require.NoError(t, err, "releasing a present target must succeed")

	// The object must still exist (release relinquishes field ownership, it
	// does not delete the object).
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "present"}, got))
}

// TestRelease_ToleratesMissingCRD pins finding 1492: when the target type's CRD
// has been removed the GET returns a NoMatch error, which must be treated as
// already-released (nothing to relinquish) rather than a hard failure that
// wedges cleanup. A custom kind absent from the scheme's RESTMapper surfaces as
// NoMatch through the fake client's mapper.
func TestRelease_ToleratesMissingCRD(t *testing.T) {
	t.Parallel()
	// A scheme/mapper that does NOT know the custom kind → GET returns NoMatch.
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	err := NewSimple(cl).Release(context.Background(), []Contribution{{
		APIVersion:   "example.com/v1",
		Kind:         "WidgetThatNoLongerHasACRD",
		Namespace:    "default",
		Name:         "w",
		FieldManager: "kro-graphengine.patch.abc.def",
	}})
	require.NoError(t, err, "a removed target CRD (NoMatch) must be treated as already-released")
}
