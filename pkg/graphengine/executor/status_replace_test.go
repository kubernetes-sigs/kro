// Copyright 2025 The Kube Resource Orchestrator Authors
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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

func statusPod(status map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "target", "namespace": "default"},
		"status":   status,
	}}
}

func TestReplaceStatus_NoWriteWhenEquivalent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		live, rendered map[string]any
	}{
		{
			name: "JSON-equivalent numbers with controller fields",
			live: map[string]any{
				"replicas": int64(3), "state": "ACTIVE",
				"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
			},
			rendered: map[string]any{"replicas": float64(3)},
		},
		{name: "empty projection and absent status", rendered: map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updates, reads := 0, 0
			cl := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					reads++
					return nil
				},
				SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
					updates++
					return nil
				},
			}).Build()
			desired := statusPod(tc.rendered)
			require.NoError(t, NewSimple(cl).replaceStatus(t.Context(), desired, statusPod(tc.live)))
			assert.Zero(t, updates, "count requests, not just resourceVersion changes")
			assert.Zero(t, reads, "reuse the target read already available to applyPatchOne")
		})
	}
}

func TestReplaceStatus_ConflictRetry(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"concurrent controller status", "deleted target", "exhausted conflicts"} {
		t.Run(scenario, func(t *testing.T) {
			pod := statusPod(map[string]any{
				"phase": "Pending", "message": "stale", "state": "IN_PROGRESS",
				"conditions": []any{map[string]any{"type": "Ready", "status": "False"}},
			})
			conditions := []any{map[string]any{"type": "Ready", "status": "True"}}
			updates, reads := 0, 0
			cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(pod).
				WithStatusSubresource(pod).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					reads++
					return cl.Get(ctx, key, obj, opts...)
				},
				SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					updates++
					assert.Equal(t, "status", sub)
					if updates == 1 || scenario == "exhausted conflicts" {
						switch scenario {
						case "concurrent controller status":
							fresh := pod.DeepCopy()
							require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(pod), fresh))
							fresh.Object["status"].(map[string]any)["conditions"] = conditions
							fresh.Object["status"].(map[string]any)["state"] = "ACTIVE"
							require.NoError(t, cl.Status().Update(ctx, fresh))
						case "deleted target":
							require.NoError(t, cl.Delete(ctx, pod))
						}
						return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, obj.GetName(),
							errors.New("resourceVersion changed"))
					}
					return cl.SubResource(sub).Update(ctx, obj, opts...)
				},
			}).Build()
			g := generator.NewGraph("g", generator.WithNamespace("default"),
				generator.WithPatch("p", "v1", "Pod", "target", map[string]any{
					"status": map[string]any{"phase": "Running"},
				}),
			)
			res, err := NewSimple(cl).Apply(t.Context(),
				compileAndBuild(t, g, compiler.WithStatusReplace("p")), watchrouter.NoopWatcher{})
			if scenario != "concurrent controller status" {
				require.ErrorIs(t, err, ErrNotReady)
				assert.NotErrorIs(t, err, ErrFieldManagerConflict, "resourceVersion conflicts are not ownership conflicts")
				assert.Contains(t, res.Unresolved, "p")
				assert.Empty(t, res.Contributions)
				if scenario == "deleted target" {
					assert.Equal(t, 1, updates)
					assert.Equal(t, 2, reads)
				} else {
					assert.True(t, apierrors.IsConflict(err))
					assert.Greater(t, updates, 1, "retry before reporting soft not-ready")
					assert.Equal(t, updates, reads, "every retry rereads")
				}
				return
			}
			require.NoError(t, err)
			require.Len(t, res.Contributions, 1)
			assert.Equal(t, 2, updates)
			assert.Equal(t, 2, reads)
			got := pod.DeepCopy()
			require.NoError(t, cl.Get(t.Context(), client.ObjectKeyFromObject(pod), got))
			assert.Equal(t, map[string]any{
				"phase": "Running", "conditions": conditions, "state": "ACTIVE",
			}, got.Object["status"], "retry must carry fresh controller fields while removing stale author data")
		})
	}
}
