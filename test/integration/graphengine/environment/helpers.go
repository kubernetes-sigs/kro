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

package environment

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// RawExt JSON-marshals v into a RawExtension. Convenience for inline
// template/def payloads in tests so suites don't repeat the boilerplate.
func RawExt(t *testing.T, v any) *runtime.RawExtension {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal RawExt: %v", err)
	}
	return &runtime.RawExtension{Raw: raw}
}

// CreateGraph creates the Graph and registers cleanup. The Graph is
// returned post-create so the caller has the populated ResourceVersion.
func (e *Env) CreateGraph(t *testing.T, g *expv1alpha1.Graph) *expv1alpha1.Graph {
	t.Helper()
	if err := e.Client.Create(e.Ctx, g); err != nil {
		t.Fatalf("create graph %s/%s: %v", g.Namespace, g.Name, err)
	}
	t.Cleanup(func() {
		_ = e.deleteGraphAndWait(g)
	})
	return g
}

// deleteGraphAndWait issues a Delete and waits for the API server to
// release the object — important so subsequent tests don't observe
// half-deleted state.
func (e *Env) deleteGraphAndWait(g *expv1alpha1.Graph) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	key := client.ObjectKeyFromObject(g)
	current := &expv1alpha1.Graph{}
	if err := e.Client.Get(ctx, key, current); err != nil {
		return nil
	}
	if err := e.Client.Delete(ctx, current); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		err := e.Client.Get(ctx, key, current)
		if err != nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("graph %s did not finalize within timeout", key)
}

// GetGraph fetches the current Graph object. Convenience around the
// typed client.
func (e *Env) GetGraph(t *testing.T, key types.NamespacedName) *expv1alpha1.Graph {
	t.Helper()
	g := &expv1alpha1.Graph{}
	if err := e.Client.Get(e.Ctx, key, g); err != nil {
		t.Fatalf("get graph %s: %v", key, err)
	}
	return g
}

// AwaitCondition polls until the Graph carries a condition of the given
// type with the expected status. Timeout fails the test with the last
// observed condition value.
func (e *Env) AwaitCondition(t *testing.T, key types.NamespacedName, condType expv1alpha1.ConditionType, want metav1.ConditionStatus, timeout time.Duration) *expv1alpha1.Condition {
	t.Helper()
	var observed *expv1alpha1.Condition
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		g := &expv1alpha1.Graph{}
		if err := e.Client.Get(e.Ctx, key, g); err == nil {
			observed = findCondition(g.Status.Conditions, condType)
			if observed != nil && observed.Status == want {
				return observed
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("condition %s=%s not observed within %s (last=%+v)", condType, want, timeout, observed)
	return nil
}

func findCondition(conds expv1alpha1.Conditions, t expv1alpha1.ConditionType) *expv1alpha1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}

// AwaitObject polls for a resource to exist and optionally satisfy
// match(). Used for asserting templates produced their child objects.
func (e *Env) AwaitObject(t *testing.T, gvk schema.GroupVersionKind, key types.NamespacedName, match func(*unstructured.Unstructured) error, timeout time.Duration) *unstructured.Unstructured {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var (
		lastErr error
		obj     = &unstructured.Unstructured{}
	)
	obj.SetGroupVersionKind(gvk)
	for time.Now().Before(deadline) {
		if err := e.Client.Get(e.Ctx, key, obj); err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if match == nil {
			return obj
		}
		if err := match(obj); err == nil {
			return obj
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("object %s %s not converged within %s: %v", gvk, key, timeout, lastErr)
	return nil
}

// AwaitGraphGone polls until the Graph object is gone from the API
// server (finalizer dropped, cascade complete).
func (e *Env) AwaitGraphGone(t *testing.T, key types.NamespacedName, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		g := &expv1alpha1.Graph{}
		err := e.Client.Get(e.Ctx, key, g)
		if err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("graph %s still present after %s", key, timeout)
}

// AwaitDeleted polls until the resource is gone (404).
func (e *Env) AwaitDeleted(t *testing.T, gvk schema.GroupVersionKind, key types.NamespacedName, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		err := e.Client.Get(e.Ctx, key, obj)
		if err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("object %s %s still present after %s", gvk, key, timeout)
}

// MustGet wraps client.Get for the common "fetch into a typed value"
// pattern. Fails the test on error.
func (e *Env) MustGet(t *testing.T, key types.NamespacedName, into client.Object) {
	t.Helper()
	if err := e.Client.Get(e.Ctx, key, into); err != nil {
		t.Fatalf("get %T %s: %v", into, key, err)
	}
}

// UpdateGraphSpec applies `mutate` to a freshly-fetched Graph and
// retries on conflict. Used by tests that race the controller's
// status-update churn — without retry, a 409 here flakes the suite
// (the controller bumps ResourceVersion via status writes between our
// Get and Update). Retries up to 10 times before failing the test.
func (e *Env) UpdateGraphSpec(t *testing.T, key types.NamespacedName, mutate func(*expv1alpha1.Graph)) {
	t.Helper()
	for i := 0; i < 10; i++ {
		g := &expv1alpha1.Graph{}
		if err := e.Client.Get(e.Ctx, key, g); err != nil {
			t.Fatalf("UpdateGraphSpec get %s: %v", key, err)
		}
		mutate(g)
		err := e.Client.Update(e.Ctx, g)
		if err == nil {
			return
		}
		if !isConflictErr(err) {
			t.Fatalf("UpdateGraphSpec update %s: %v", key, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("UpdateGraphSpec %s: gave up after 10 conflict retries", key)
}

// isConflictErr reports whether err is a 409 from the API server.
// Kept inline (rather than depending on apimachinery's IsConflict)
// because the import surface here is already busy.
func isConflictErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "the object has been modified")
}

// MutateObject re-fetches `obj` by the supplied key, applies `mutate`,
// and Updates with retry on 409. Used by drift tests that race the
// controller's SSA reconcile loop on the same child resource.
func (e *Env) MutateObject(t *testing.T, key types.NamespacedName, obj client.Object, mutate func(client.Object)) {
	t.Helper()
	for i := 0; i < 10; i++ {
		if err := e.Client.Get(e.Ctx, key, obj); err != nil {
			t.Fatalf("MutateObject get %s: %v", key, err)
		}
		mutate(obj)
		err := e.Client.Update(e.Ctx, obj)
		if err == nil {
			return
		}
		if !isConflictErr(err) {
			t.Fatalf("MutateObject update %s: %v", key, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("MutateObject %s: gave up after 10 conflict retries", key)
}
