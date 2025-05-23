package core_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kro-run/kro/api/v1alpha1"
	"github.com/kro-run/kro/pkg/testutil/generator"
)

var _ = Describe("CELFunctions", func() {
	It("should apply to resource and status fields", func() {
		ctx := context.Background()
		namespace := fmt.Sprintf("test-cel-%s", rand.String(5))

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())

		rgdName := "test-cel-rgd"
		instanceKind := "TestCELInstance"
		rgd := generator.NewResourceGraphDefinition(rgdName,
			generator.WithSchema(
				instanceKind, "v1alpha1",
				map[string]any{ // Spec schema
					"interjection": "string",
					"subject":      "string",
				},
				map[string]any{ // Status fields
					"exclaimedGreeting": "${concat(concat('hello ', testcm.kind), '!!!')}",
				},
			),
			generator.WithCELFunction(
				"concat",
				"s1 + s2",
				[]krov1alpha1.FunctionInput{
					{Name: "s1", Type: "string"},
					{Name: "s2", Type: "string"},
				},
				"string", // Return type
			),
			generator.WithResource("testcm", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": "cm-${schema.metadata.name}",
				},
				"data": map[string]any{
					"greeting": "${concat(schema.spec.interjection, concat(' ', schema.spec.subject))}",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Verify ResourceGraphDefinition is ready
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())

			// Matching all conditions ensures that failure causes are reported,
			// as opposed to just using the state field
			g.Expect(createdRGD.Status.Conditions).To(And(
				Not(BeEmpty()),
				HaveEach(HaveField("Status", metav1.ConditionTrue)),
			))
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).Should(Succeed())

		instanceName := "my-rgd-instance"
		instance := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       instanceKind,
				"metadata": map[string]any{
					"name":      instanceName,
					"namespace": namespace,
				},
				"spec": map[string]any{
					"interjection": "hello",
					"subject":      "world",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: namespace}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			conditions, _, _ := unstructured.NestedSlice(instance.Object, "status", "conditions")

			g.Expect(conditions).To(And(
				Not(BeEmpty()),
				HaveEach(HaveKeyWithValue("status", "True")),
			))

			exclaimedGreeting, _, _ := unstructured.NestedString(instance.Object, "status", "exclaimedGreeting")
			g.Expect(exclaimedGreeting).To(Equal("hello ConfigMap!!!"))

		}, 20*time.Second, time.Second).Should(Succeed())

		// Verify the ConfigMap is created and has the correct data from CEL function execution
		createdCM := &corev1.ConfigMap{}
		cmName := fmt.Sprintf("cm-%s", instanceName)
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: cmName, Namespace: namespace}, createdCM)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdCM.Data).ToNot(BeNil())
			g.Expect(createdCM.Data["greeting"]).To(Equal("hello world"))
		}, 20*time.Second, time.Second).Should(Succeed())

		// Cleanup
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func() bool {
			err := env.Client.Get(ctx, types.NamespacedName{Name: cmName, Namespace: namespace}, createdCM)
			return errors.IsNotFound(err)
		}, 20*time.Second, time.Second).Should(BeTrue())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Expect(env.Client.Delete(ctx, ns)).To(Succeed())
	})
})
