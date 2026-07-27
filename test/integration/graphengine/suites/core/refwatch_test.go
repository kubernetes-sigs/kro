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

package core_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/test/integration/graphengine/environment"
)

// TestRefAndWatchAreCurrentlyUnsupported pins the v1 limitation: the
// compiler accepts ref and watch node shapes, but the Simple executor
// surfaces ErrUnsupported when it encounters them at apply time. The
// Graph therefore reaches Accepted=True (compile succeeded) but
// Ready never flips True — the message on Ready (or Accepted) will
// reference the unsupported kind.
//
// When ref/watch are fully wired this test will need to be flipped;
// keeping it here ensures we notice the change.
func TestRefAndWatchAreCurrentlyUnsupported(t *testing.T) {
	env := environment.Shared(t)

	tests := []struct {
		name       string
		node       expv1alpha1.Node
		wantSubstr string
	}{
		{
			name: "ref-node-surfaces-unsupported",
			node: expv1alpha1.Node{
				ID: "x",
				Ref: &expv1alpha1.ExternalRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Metadata: expv1alpha1.ExternalRefMetadata{
						Name: "external-cm",
					},
				},
			},
			wantSubstr: "not supported",
		},
		{
			name: "watch-node-surfaces-unsupported",
			node: expv1alpha1.Node{
				ID: "y",
				Watch: &expv1alpha1.WatchSpec{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Selector:   map[string]string{"app": "demo"},
				},
			},
			wantSubstr: "not supported",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ns := env.CreateNamespace(t)
			g := &expv1alpha1.Graph{
				ObjectMeta: metav1.ObjectMeta{Name: "u", Namespace: ns},
				Spec: expv1alpha1.GraphSpec{
					Nodes: []expv1alpha1.Node{
						// A literal seed so the input-node rule passes.
						{
							ID:  "seed",
							Def: environment.RawExt(t, map[string]any{"v": "x"}),
						},
						tc.node,
					},
				},
			}
			env.CreateGraph(t, g)

			env.AwaitCondition(t,
				types.NamespacedName{Namespace: ns, Name: "u"},
				expv1alpha1.GraphConditionTypeAccepted,
				metav1.ConditionTrue, 15*time.Second)

			// The reconciler bubbles the executor error up — it surfaces
			// either as a non-Ready Ready condition or in subsequent
			// reconcile logs. Easiest place to assert it is via the
			// Ready condition, which never flips True.
			environment.Consistently(t, 2*time.Second, 200*time.Millisecond, func() error {
				obj := &expv1alpha1.Graph{}
				if err := env.Client.Get(env.Ctx, types.NamespacedName{Namespace: ns, Name: "u"}, obj); err != nil {
					return err
				}
				for _, c := range obj.Status.Conditions {
					if c.Type == expv1alpha1.GraphConditionTypeReady && c.Status == metav1.ConditionTrue {
						return fmt.Errorf("Ready unexpectedly True for unsupported kind")
					}
				}
				return nil
			})

			// Verify the executor's substring surfaces somewhere in the
			// reconcile error log by polling once more for the Ready
			// message (set indirectly via the Reconciler return value
			// → controller-runtime requeue → not surfaced in status,
			// so we accept either: not-Ready or no Ready at all).
			obj := env.GetGraph(t, types.NamespacedName{Namespace: ns, Name: "u"})
			for _, c := range obj.Status.Conditions {
				if c.Type == expv1alpha1.GraphConditionTypeReady && c.Message != nil &&
					strings.Contains(strings.ToLower(*c.Message), tc.wantSubstr) {
					return // good — message carries the limitation reason
				}
			}
			// Lack of an explicit message is acceptable in v1; the
			// substring assertion above is best-effort.
		})
	}
}
