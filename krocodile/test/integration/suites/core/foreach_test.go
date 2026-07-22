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
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	"sigs.k8s.io/krocodile/test/integration/environment"
)

// TestForEachExpandsToPerItemChildren covers the single-dimension
// expansion path: one forEach axis bound to a literal list yields one
// child per element. The names come from a CEL expression that
// references the loop variable.
func TestForEachExpandsToPerItemChildren(t *testing.T) {
	env := environment.Shared(t)
	ns := env.CreateNamespace(t)

	g := &expv1alpha1.Graph{
		ObjectMeta: metav1.ObjectMeta{Name: "fe", Namespace: ns},
		Spec: expv1alpha1.GraphSpec{
			Nodes: []expv1alpha1.Node{
				{
					ID:  "names",
					Def: environment.RawExt(t, map[string]any{"items": []string{"red", "green", "blue"}}),
				},
				{
					ID:      "cms",
					ForEach: []expv1alpha1.ForEachDimension{{"color": "${names.items}"}},
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${color}"},
						"data":       map[string]any{"who": "${color}"},
					}),
				},
			},
		},
	}
	env.CreateGraph(t, g)

	env.AwaitCondition(t,
		types.NamespacedName{Namespace: ns, Name: "fe"},
		expv1alpha1.GraphConditionTypeReady,
		metav1.ConditionTrue, 15*time.Second)

	wantNames := []string{"blue", "green", "red"}
	environment.Eventually(t, 15*time.Second, 100*time.Millisecond, func() error {
		got, err := listConfigMapNames(env, ns)
		if err != nil {
			return err
		}
		if !sliceEqual(got, wantNames) {
			return fmt.Errorf("ConfigMap names=%v want %v", got, wantNames)
		}
		return nil
	})
}

// TestForEachCartesianProduct exercises the multi-dimension path: two
// dimensions produce the cross of every combination.
func TestForEachCartesianProduct(t *testing.T) {
	env := environment.Shared(t)
	ns := env.CreateNamespace(t)

	g := &expv1alpha1.Graph{
		ObjectMeta: metav1.ObjectMeta{Name: "cart", Namespace: ns},
		Spec: expv1alpha1.GraphSpec{
			Nodes: []expv1alpha1.Node{
				{
					ID: "axes",
					Def: environment.RawExt(t, map[string]any{
						"shapes": []string{"sq", "tri"},
						"colors": []string{"r", "g"},
					}),
				},
				{
					ID: "cms",
					ForEach: []expv1alpha1.ForEachDimension{
						{"shape": "${axes.shapes}"},
						{"color": "${axes.colors}"},
					},
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${shape}-${color}"},
					}),
				},
			},
		},
	}
	env.CreateGraph(t, g)

	env.AwaitCondition(t,
		types.NamespacedName{Namespace: ns, Name: "cart"},
		expv1alpha1.GraphConditionTypeReady,
		metav1.ConditionTrue, 15*time.Second)

	want := []string{"sq-r", "sq-g", "tri-r", "tri-g"}
	sort.Strings(want)
	environment.Eventually(t, 15*time.Second, 100*time.Millisecond, func() error {
		got, err := listConfigMapNames(env, ns)
		if err != nil {
			return err
		}
		if !sliceEqual(got, want) {
			return fmt.Errorf("ConfigMap names=%v want %v", got, want)
		}
		return nil
	})
}

// listConfigMapNames returns the sorted names of every ConfigMap in
// the namespace except the auto-installed kube-root-ca.crt.
func listConfigMapNames(env *environment.Env, ns string) ([]string, error) {
	var list corev1.ConfigMapList
	if err := env.Client.List(env.Ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list.Items))
	for _, cm := range list.Items {
		if cm.Name == "kube-root-ca.crt" {
			continue
		}
		out = append(out, cm.Name)
	}
	sort.Strings(out)
	return out, nil
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
