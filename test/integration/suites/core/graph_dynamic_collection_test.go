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
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/environment"
)

// Graph Dynamic Collection covers the graph.md "Dynamic GVKs" pattern: a static
// ref to a CRD object, a dynamic-GVK selector ref over that CRD's instances, and
// a forEach template plus comprehensions over that schemaless collection.
var _ = Describe("Graph Dynamic Collection", func() {
	It("fans a forEach template out over a dynamic-GVK selector ref", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)
		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}

		// A CRD under a unique group so parallel specs never collide; the
		// Graph discovers its instances' GVK from the CRD object itself.
		group := fmt.Sprintf("dyncoll-%s.example.com", rand.String(5))
		kind := fmt.Sprintf("Widget%s", rand.String(5))
		crd := installCRD(t, env, group, kind, "dyncoll")
		instanceGVK := schema.GroupVersionKind{Group: group, Version: "v1", Kind: kind}

		createInstance := func(name, value string) {
			t.Helper()
			inst := &unstructured.Unstructured{}
			inst.SetGroupVersionKind(instanceGVK)
			inst.SetName(name)
			inst.SetNamespace(ns)
			if err := unstructured.SetNestedField(inst.Object, value, "spec", "value"); err != nil {
				t.Fatalf("set spec.value on %s: %v", name, err)
			}
			if err := env.Client.Create(ctx, inst); err != nil {
				t.Fatalf("create %s instance %s: %v", kind, name, err)
			}
			t.Cleanup(func() { _ = env.Client.Delete(context.Background(), inst) })
		}
		createInstance("alpha", "one")
		createInstance("beta", "two")

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "dyncoll", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{
					{
						// Static ref: the CRD object, typed against the
						// apiextensions.k8s.io/v1 schema.
						ID: "crd",
						Ref: &expv1alpha1.ExternalRef{
							APIVersion: "apiextensions.k8s.io/v1",
							Kind:       "CustomResourceDefinition",
							Metadata:   expv1alpha1.ExternalRefMetadata{Name: crd.Name},
						},
					},
					{
						// Dynamic-GVK selector ref: a read-only collection of the
						// CRD's instances whose GVK is only known at reconcile time.
						ID: "insts",
						Ref: &expv1alpha1.ExternalRef{
							APIVersion: "${crd.spec.group + '/' + crd.spec.versions[0].name}",
							Kind:       "${crd.spec.names.kind}",
							Metadata: expv1alpha1.ExternalRefMetadata{
								Namespace: ns,
								Selector:  runtime.RawExtension{Raw: []byte(`{}`)},
							},
						},
					},
					{
						// forEach directly over the dynamic collection: one
						// ConfigMap per instance, reading the instance's fields.
						ID:      "cms",
						ForEach: []expv1alpha1.ForEachDimension{{"w": "${insts}"}},
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "${w.metadata.name}-cm"},
							"data":       map[string]any{"value": "${w.spec.value}"},
						}),
					},
					{
						// Comprehensions and size() over the same collection.
						ID: "summary",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "summary"},
							"data": map[string]any{
								"count": "${string(size(insts))}",
								"names": "${insts.map(x, x.metadata.name).join(',')}",
								"twos":  "${string(insts.filter(x, x.spec.value == 'two').size())}",
							},
						}),
					},
				},
			},
		}
		env.CreateGraph(t, g)

		graphKey := types.NamespacedName{Namespace: ns, Name: "dyncoll"}
		env.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeAccepted, metav1.ConditionTrue, 20*time.Second)
		env.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 30*time.Second)

		awaitConfigMaps := func(want []string) {
			t.Helper()
			environment.Eventually(t, 20*time.Second, 200*time.Millisecond, func() error {
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
		awaitConfigMaps([]string{"alpha-cm", "beta-cm", "summary"})

		// Per-instance fan-out read the instance's own spec.
		env.AwaitObject(t, configMapGVK, types.NamespacedName{Namespace: ns, Name: "alpha-cm"}, func(u *unstructured.Unstructured) error {
			if v, _, _ := unstructured.NestedString(u.Object, "data", "value"); v != "one" {
				return fmt.Errorf("alpha-cm.data.value=%q want one", v)
			}
			return nil
		}, 10*time.Second)
		env.AwaitObject(t, configMapGVK, types.NamespacedName{Namespace: ns, Name: "beta-cm"}, func(u *unstructured.Unstructured) error {
			if v, _, _ := unstructured.NestedString(u.Object, "data", "value"); v != "two" {
				return fmt.Errorf("beta-cm.data.value=%q want two", v)
			}
			return nil
		}, 10*time.Second)

		// The comprehensions ranged over the listed instances.
		env.AwaitObject(t, configMapGVK, types.NamespacedName{Namespace: ns, Name: "summary"}, func(u *unstructured.Unstructured) error {
			count, _, _ := unstructured.NestedString(u.Object, "data", "count")
			names, _, _ := unstructured.NestedString(u.Object, "data", "names")
			twos, _, _ := unstructured.NestedString(u.Object, "data", "twos")
			gotNames := strings.Split(names, ",")
			sort.Strings(gotNames)
			if count != "2" || twos != "1" || !sliceEqual(gotNames, []string{"alpha", "beta"}) {
				return fmt.Errorf("summary data count=%q twos=%q names=%q", count, twos, names)
			}
			return nil
		}, 10*time.Second)

		// A new matching instance re-enqueues the Graph and extends the fan-out.
		createInstance("gamma", "three")
		awaitConfigMaps([]string{"alpha-cm", "beta-cm", "gamma-cm", "summary"})
		env.AwaitObject(t, configMapGVK, types.NamespacedName{Namespace: ns, Name: "summary"}, func(u *unstructured.Unstructured) error {
			if count, _, _ := unstructured.NestedString(u.Object, "data", "count"); count != "3" {
				return fmt.Errorf("summary.data.count=%q want 3", count)
			}
			return nil
		}, 20*time.Second)
	})
})
