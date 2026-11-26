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

package core_test

import (
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Graph CRD templates", func() {
	It("uses a referenced RGD's field declarations to default and validate ApplicationPresets", func(ctx SpecContext) {
		t := GinkgoT()
		ns := env.CreateNamespace(t)
		source := generator.NewResourceGraphDefinition("my-application",
			generator.WithSchema("PresetSource", "v1alpha1",
				map[string]any{"replicas": "PoolSize"},
				map[string]any{"endpoints": []any{map[string]any{"name": "${schema.metadata.name}"}}},
				generator.WithTypes(map[string]any{"PoolSize": "integer | default=3 minimum=1"}),
			),
		)
		Expect(env.Client.Create(ctx, source)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) { Expect(env.Client.Delete(ctx, source)).To(Succeed()) })
		waitForRGDActive(ctx, source.Name)

		manifest, err := os.ReadFile("../../../../examples/graph/application-preset.yaml")
		Expect(err).NotTo(HaveOccurred())
		var graph krov1alpha1.Graph
		Expect(yaml.UnmarshalStrict(manifest, &graph)).To(Succeed())
		graph.Namespace = ns
		env.CreateGraph(t, &graph)
		awaitCRDEstablished(t, env, "applicationpresets.example.com")

		preset := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "example.com/v1", "kind": "ApplicationPreset",
			"metadata": map[string]any{"namespace": ns, "name": "default-size"},
		}}
		invalid := preset.DeepCopy()
		invalid.Object["spec"] = map[string]any{"replicas": int64(0)}

		Eventually(func() error {
			err := env.Client.Create(ctx, preset, client.DryRunAll)
			if err != nil && !meta.IsNoMatchError(err) {
				return StopTrying("could not create ApplicationPreset").Wrap(err)
			}
			return err
		}, 20*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
		Expect(preset.Object["spec"]).To(Equal(map[string]any{"replicas": int64(3)}))

		err = env.Client.Create(ctx, invalid, client.DryRunAll)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%v", err)
		Expect(err).To(MatchError(ContainSubstring("spec.replicas")))
	})
})
