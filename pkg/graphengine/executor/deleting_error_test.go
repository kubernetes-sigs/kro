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

package executor

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ResourceDeletingError has to satisfy two sentinels at once: ErrResourceDeleting
// so callers can distinguish "the live object is terminating" from other soft
// conditions, and ErrNotReady so the executor's and reconciler's generic
// soft-not-ready handling gates dependents and requeues instead of failing the
// reconcile. Losing the ErrNotReady arm would turn a routine terminating
// resource into a hard error; losing the ErrResourceDeleting arm would make the
// case indistinguishable and cost the specific status message.
func TestResourceDeletingErrorSatisfiesBothSentinels(t *testing.T) {
	t.Parallel()

	err := &ResourceDeletingError{NodeID: "bucket", Namespace: "default", Name: "my-bucket"}

	assert.True(t, errors.Is(err, ErrResourceDeleting),
		"must be distinguishable as a terminating-resource signal")
	assert.True(t, errors.Is(err, ErrNotReady),
		"must also satisfy ErrNotReady so generic soft handling requeues and gates dependents")
	assert.False(t, errors.Is(err, errors.New("unrelated")),
		"must not claim to be an unrelated error")

	t.Run("both sentinels survive wrapping", func(t *testing.T) {
		wrapped := fmt.Errorf("apply %q: %w", "bucket", err)
		assert.True(t, errors.Is(wrapped, ErrResourceDeleting))
		assert.True(t, errors.Is(wrapped, ErrNotReady))
	})

	t.Run("the typed error is recoverable with errors.As", func(t *testing.T) {
		wrapped := fmt.Errorf("apply %q: %w", "bucket", err)
		var target *ResourceDeletingError
		require.True(t, errors.As(wrapped, &target))
		assert.Equal(t, "bucket", target.NodeID)
		assert.Equal(t, "my-bucket", target.Name)
	})
}

// The message format is a contract: integration specs assert on the exact
// `resource "<namespace>/<name>"` substring that reaches the instance's
// ResourcesDeleting condition, so a reformat here breaks them.
func TestResourceDeletingErrorMessage(t *testing.T) {
	t.Parallel()

	t.Run("a namespaced resource renders namespace/name", func(t *testing.T) {
		t.Parallel()
		err := &ResourceDeletingError{NodeID: "bucket", Namespace: "default", Name: "my-bucket"}
		msg := err.Error()
		assert.Contains(t, msg, `resource "default/my-bucket"`)
		assert.Contains(t, msg, `node "bucket"`)
		assert.Contains(t, msg, "being deleted")
	})

	t.Run("a cluster-scoped resource renders the bare name", func(t *testing.T) {
		t.Parallel()
		err := &ResourceDeletingError{NodeID: "ns", Name: "team-a"}
		msg := err.Error()
		assert.Contains(t, msg, `resource "team-a"`)
		assert.NotContains(t, msg, `"/team-a"`,
			"a cluster-scoped resource must not render a leading slash")
	})
}
