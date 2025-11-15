package core_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Unknown Fields", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))

		By("creating namespace")
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		By("cleaning up namespace")
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	Context("CRD with preserve unknown fields", func() {
		It("should allow instances with arbitrary unknown nested fields and resolve references", func(ctx SpecContext) {

			By("applying CRD AllowUnknown with x-kubernetes-preserve-unknown-fields")
			crd := &apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "allowunknowns.kro.run",
				},
				Spec: apiextensionsv1.CustomResourceDefinitionSpec{
					Group: "kro.run",
					Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
						{
							Name:    "v1alpha1",
							Served:  true,
							Storage: true,
							Schema: &apiextensionsv1.CustomResourceValidation{
								OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"spec": {
											Type: "object",
											Properties: map[string]apiextensionsv1.JSONSchemaProps{
												"config": {
													Type:                   "object",
													XPreserveUnknownFields: ptr.To(true),
												},
											},
										},
									},
								},
							},
						},
					},
					Scope: apiextensionsv1.NamespaceScoped,
					Names: apiextensionsv1.CustomResourceDefinitionNames{
						Plural:     "allowunknowns",
						Singular:   "allowunknown",
						Kind:       "AllowUnknown",
						ShortNames: []string{"alu"},
					},
				},
			}

			Expect(env.Client.Create(ctx, crd)).To(Succeed())

			By("waiting for CRD to be ready")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx,
					types.NamespacedName{Name: "allowunknowns.kro.run"},
					&apiextensionsv1.CustomResourceDefinition{},
				)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("creating ResourceGraphDefinition that uses unknown fields")
			rgd := generator.NewResourceGraphDefinition("check-unknown-fields",
				generator.WithSchema(
					"AllowUnknownInstance", "v1alpha1",
					map[string]any{
						"name":   "string",
						"nested": "object",
					},
					nil,
				),
				generator.WithResource("example",
					map[string]any{
						"apiVersion": "kro.run/v1alpha1",
						"kind":       "AllowUnknown",
						"metadata": map[string]any{
							"name": "${schema.spec.name}",
						},
						"spec": map[string]any{
							"config": map[string]any{
								"foo": "bar",
							},
						},
					},
					nil,
					nil,
				),
				generator.WithResource("configmap",
					map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name": "check-unknown-fields",
						},
						"data": map[string]any{
							"foo":    "${example.spec.config.foo}",
							"name":   "${schema.spec.name}",
							"nested": "${schema.spec.nested.field}",
						},
					},
					nil,
					nil,
				),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())

			By("waiting for RGD to become Active")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("creating instance including unknown nested fields")

			instance := &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "kro.run/v1alpha1",
					"kind":       "AllowUnknownInstance",
					"metadata": map[string]any{
						"name":      "check-unknown-fields",
						"namespace": namespace,
					},
					"spec": map[string]any{
						"name": "name",
						"nested": map[string]any{
							"field": "value",
						},
					},
				},
			}

			Expect(env.Client.Create(ctx, instance)).To(Succeed())

			By("waiting for Instance to become Ready")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx,
					types.NamespacedName{Name: "check-unknown-fields", Namespace: namespace},
					instance,
				)
				g.Expect(err).ToNot(HaveOccurred())
				state, found, err := unstructured.NestedString(instance.Object, "status", "state")
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(found).To(BeTrue())
				g.Expect(state).To(Equal("ACTIVE"))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("verifying ConfigMap resolved unknown-field expressions")

			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx,
					types.NamespacedName{Name: "check-unknown-fields", Namespace: namespace},
					cm,
				)
				g.Expect(err).ToNot(HaveOccurred())

				g.Expect(cm.Data["foo"]).To(Equal("bar"))
				g.Expect(cm.Data["name"]).To(Equal("name"))
				g.Expect(cm.Data["nested"]).To(Equal("value"))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		})
	})
})
