// Copyright 2025 The Kubernetes Authors.
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
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("RGD Compilation", func() {
	It("should compile a complex graph with collections, externalRef, and type-safe assignments", func(ctx SpecContext) {
		const targetNamespace = "default"
		By("creating a complex ResourceGraphDefinition")
		instanceSpecSchema := map[string]interface{}{
			"name":                                 "string",
			"enabled":                              "boolean",
			"replicas":                             "integer",
			"labels":                               "map[string]string",
			"workers":                              "[]workerSpec",
			"containers":                           "[]containerSpec",
			"vpcCIDRBlocks":                        "[]string",
			"vpcDisallowSecurityGroupDefaultRules": "boolean",
			"vpcEnableDNSHostnames":                "boolean",
			"vpcEnableDNSSupport":                  "boolean",
			"vpcInstanceTenancy":                   "string",
			"vpcIpv4NetmaskLength":                 "integer",
			"vpcTags":                              "[]vpcTagSpec",
		}
		instanceStatusSchema := map[string]interface{}{
			"sourceName": "${source.metadata.name}",
			"deployment": map[string]interface{}{
				"name": "${app.metadata.name}",
			},
			"network": map[string]interface{}{
				"vpc": map[string]interface{}{
					"name":        "${vpc.metadata.name}",
					"primaryCIDR": "${vpc.spec.cidrBlocks[0]}",
					"tagCount":    "${size(vpc.spec.tags)}",
					"dnsSupport":  "${vpc.spec.enableDNSSupport}",
				},
			},
			"observability": map[string]interface{}{
				"workSummary": "copies=${string(size(copies))}",
				"copies": map[string]interface{}{
					"count":     "${size(copies)}",
					"hasCanary": "${copies.exists(x, x.metadata.labels.tier == \"canary\")}",
				},
			},
		}
		instanceCustomTypes := map[string]interface{}{
			"workerSpec": map[string]interface{}{
				"name": "string",
				"tier": "string",
			},
			"containerPortSpec": map[string]interface{}{
				"containerPort": "integer",
			},
			"containerSpec": map[string]interface{}{
				"name":  "string",
				"image": "string",
				"ports": "[]containerPortSpec",
			},
			"vpcTagSpec": map[string]interface{}{
				"key":   "string",
				"value": "string",
			},
		}
		rgd := generator.NewResourceGraphDefinition("test-static-analysis-complex",
			generator.WithSchema(
				"StaticAnalysisComplex", "v1alpha1",
				instanceSpecSchema,
				instanceStatusSchema,
				generator.WithTypes(instanceCustomTypes),
			),
			generator.WithExternalRef("source", &krov1alpha1.ExternalRef{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Metadata: krov1alpha1.ExternalRefMetadata{
					Name:      "complex-source",
					Namespace: targetNamespace,
				},
			}, nil, nil),
			generator.WithResource(
				"vpc",
				map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "VPC",
					"metadata": map[string]interface{}{
						"name":      "${schema.spec.name}-vpc",
						"namespace": targetNamespace,
						"labels":    "${schema.spec.labels}",
					},
					"spec": map[string]interface{}{
						"cidrBlocks":                        "${schema.spec.vpcCIDRBlocks}",
						"disallowSecurityGroupDefaultRules": "${schema.spec.vpcDisallowSecurityGroupDefaultRules}",
						"enableDNSHostnames":                "${schema.spec.vpcEnableDNSHostnames}",
						"enableDNSSupport":                  "${schema.spec.vpcEnableDNSSupport}",
						"instanceTenancy":                   "${schema.spec.vpcInstanceTenancy}",
						"ipv4NetmaskLength":                 "${schema.spec.vpcIpv4NetmaskLength}",
						"tags":                              "${schema.spec.vpcTags}",
					},
				},
				nil,
				nil,
			),
			generator.WithResourceCollection(
				"copies",
				map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-${worker.name}",
						"labels": map[string]interface{}{
							"tier": "${worker.tier == \"canary\" ? \"canary\" : \"stable\"}",
						},
					},
					"data": map[string]interface{}{
						"worker":    "${worker.name}",
						"from":      "${source.data.value}",
						"isEnabled": "${schema.spec.enabled ? \"true\" : \"false\"}",
					},
				},
				[]krov1alpha1.ForEachDimension{
					{"worker": "${schema.spec.workers}"},
				},
				[]string{"${each.metadata.name != \"\"}"},
				[]string{"${schema.spec.enabled}"},
			),
			generator.WithResource(
				"app",
				map[string]interface{}{
					"apiVersion": "apps/v1",
					"kind":       "Deployment",
					"metadata": map[string]interface{}{
						"name":   "${schema.spec.name}-app",
						"labels": "${schema.spec.labels}",
						"annotations": map[string]interface{}{
							"copies":   "${string(size(copies))}",
							"from":     "${source.data.value}",
							"from-3x":  "${source.data.value + \":\" + source.data.value + \":\" + source.data.value}",
							"vpc-cidr": "${vpc.spec.cidrBlocks[0]}",
						},
					},
					"spec": map[string]interface{}{
						"replicas": "${schema.spec.replicas}",
						"paused":   "${!schema.spec.enabled}",
						"selector": map[string]interface{}{
							"matchLabels": "${schema.spec.labels}",
						},
						"template": map[string]interface{}{
							"metadata": map[string]interface{}{
								"labels": "${schema.spec.labels}",
							},
							"spec": map[string]interface{}{
								"containers": "${schema.spec.containers}",
							},
						},
					},
				},
				[]string{"${app.metadata.name != \"\"}"},
				[]string{"${schema.spec.enabled}"},
			),
			generator.WithResource(
				"summary",
				map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-summary",
					},
					"data": map[string]interface{}{
						"copies": "${string(size(copies))}",
						"canary": "${copies.exists(x, x.metadata.labels.tier == \"canary\") ? \"yes\" : \"no\"}",
						"app":    "${app.metadata.name}",
						"from":   "${source.data.value}",
						"vpc":    "${vpc.metadata.name}",
					},
				},
				nil,
				[]string{"${schema.spec.enabled && size(schema.spec.workers) > 0}"},
			),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			g.Expect(createdRGD.Status.TopologicalOrder).To(ContainElements("source", "vpc", "copies", "app", "summary"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("validating active conditions and status shape")
		conditionStatusByType := map[string]string{}
		for _, cond := range createdRGD.Status.Conditions {
			conditionStatusByType[string(cond.Type)] = string(cond.Status)
		}
		Expect(conditionStatusByType).To(HaveKeyWithValue("Ready", "True"))
		Expect(conditionStatusByType).To(HaveKeyWithValue("ResourceGraphAccepted", "True"))
		Expect(conditionStatusByType).To(HaveKeyWithValue("KindReady", "True"))
		Expect(conditionStatusByType).To(HaveKeyWithValue("ControllerReady", "True"))

		By("validating dependency graph")
		expectedNodeIDs := []string{"source", "vpc", "copies", "app", "summary"}
		expectedDependencyNodeIDs := []string{"copies", "app", "summary"}
		Expect(createdRGD.Status.TopologicalOrder).To(HaveLen(len(expectedNodeIDs)))
		Expect(createdRGD.Status.TopologicalOrder).To(ContainElements("source", "vpc", "copies", "app", "summary"))
		Expect(createdRGD.Status.Resources).To(HaveLen(len(expectedDependencyNodeIDs)))

		expectedIDSet := make(map[string]struct{}, len(expectedNodeIDs))
		for _, id := range expectedNodeIDs {
			expectedIDSet[id] = struct{}{}
		}
		resourceIDs := make([]string, 0, len(createdRGD.Status.Resources))
		for _, res := range createdRGD.Status.Resources {
			resourceIDs = append(resourceIDs, res.ID)
			_, knownResource := expectedIDSet[res.ID]
			Expect(knownResource).To(BeTrue(), "unexpected resource ID %q", res.ID)
			for _, dep := range res.Dependencies {
				_, knownDep := expectedIDSet[dep.ID]
				Expect(knownDep).To(BeTrue(), "resource %q has unknown dependency %q", res.ID, dep.ID)
				Expect(dep.ID).ToNot(Equal(res.ID), "resource %q cannot depend on itself", res.ID)
			}
		}
		Expect(resourceIDs).To(ContainElements("copies", "app", "summary"))

		depsByID := complexDepsByID(createdRGD.Status.Resources)
		Expect(depsByID["source"]).To(BeEmpty())
		Expect(depsByID["copies"]).To(ConsistOf("source"))
		Expect(depsByID["app"]).To(ConsistOf("copies", "source", "vpc"))
		Expect(dependencyCount(depsByID["app"], "source")).To(Equal(1))
		Expect(depsByID["summary"]).To(ConsistOf("copies", "app", "source", "vpc"))
		orderIndex := complexIndexByID(createdRGD.Status.TopologicalOrder)
		for id, deps := range depsByID {
			for _, dep := range deps {
				Expect(orderIndex[dep]).To(BeNumerically("<", orderIndex[id]))
			}
		}

		By("validating inferred instance status schema types")
		group := createdRGD.Spec.Schema.Group
		if group == "" {
			group = krov1alpha1.KRODomainName
		}
		instanceGVR := metadata.GetResourceGraphDefinitionInstanceGVR(
			group,
			createdRGD.Spec.Schema.APIVersion,
			createdRGD.Spec.Schema.Kind,
		)
		instanceCRDName := fmt.Sprintf("%s.%s", instanceGVR.Resource, instanceGVR.Group)

		expectedSpecSchema := complexExpectedSpecSchema()
		expectedStatusSchema := complexExpectedStatusSchema()

		instanceCRD := &apiextensionsv1.CustomResourceDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: instanceCRDName}, instanceCRD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(instanceCRD.Spec.Group).To(Equal(group))
			g.Expect(instanceCRD.Spec.Scope).To(Equal(apiextensionsv1.NamespaceScoped))
			g.Expect(instanceCRD.Spec.Names.Kind).To(Equal(createdRGD.Spec.Schema.Kind))
			g.Expect(instanceCRD.Spec.Names.Plural).To(Equal(instanceGVR.Resource))
			g.Expect(instanceCRD.Spec.Versions).To(HaveLen(1))

			version := instanceCRD.Spec.Versions[0]
			g.Expect(version.Name).To(Equal(createdRGD.Spec.Schema.APIVersion))
			g.Expect(version.Served).To(BeTrue())
			g.Expect(version.Storage).To(BeTrue())
			g.Expect(version.Subresources).ToNot(BeNil())
			g.Expect(version.Subresources.Status).ToNot(BeNil())
			g.Expect(version.Schema).ToNot(BeNil())
			g.Expect(version.Schema.OpenAPIV3Schema).ToNot(BeNil())
			g.Expect(version.Schema.OpenAPIV3Schema.Type).To(Equal("object"))

			openapi := version.Schema.OpenAPIV3Schema.Properties
			g.Expect(openapi).To(HaveKey("apiVersion"))
			g.Expect(openapi).To(HaveKey("kind"))
			g.Expect(openapi).To(HaveKey("metadata"))
			g.Expect(openapi).To(HaveKey("spec"))
			g.Expect(openapi).To(HaveKey("status"))
			assertExactJSONSchema(g, openapi["spec"], expectedSpecSchema)
			assertExactJSONSchema(g, openapi["status"], expectedStatusSchema)
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should fail static analysis when expression output is not assignable to field type", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-static-analysis-complex-bad",
			generator.WithSchema(
				"StaticAnalysisComplexBad", "v1alpha1",
				map[string]interface{}{
					"name":     "string",
					"replicas": "integer",
					"enabled":  "boolean",
				},
				nil,
			),
			generator.WithResource("badApp", map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name": "bad-app",
				},
				"spec": map[string]interface{}{
					// Should be integer, but expression returns string.
					"replicas": "${schema.spec.name}",
					"selector": map[string]interface{}{
						"matchLabels": map[string]interface{}{
							"app": "bad-app",
						},
					},
					"template": map[string]interface{}{
						"metadata": map[string]interface{}{
							"labels": map[string]interface{}{
								"app": "bad-app",
							},
						},
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{
									"name":  "main",
									"image": "nginx",
								},
							},
						},
					},
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		expectRGDInactiveWithError(ctx, rgd, "type mismatch")
		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
	})
})

func complexDepsByID(resources []krov1alpha1.ResourceInformation) map[string][]string {
	out := make(map[string][]string, len(resources))
	for _, res := range resources {
		deps := make([]string, 0, len(res.Dependencies))
		for _, dep := range res.Dependencies {
			deps = append(deps, dep.ID)
		}
		out[res.ID] = deps
	}
	return out
}

func complexIndexByID(order []string) map[string]int {
	out := make(map[string]int, len(order))
	for i, id := range order {
		out[id] = i
	}
	return out
}

func dependencyCount(deps []string, target string) int {
	count := 0
	for _, dep := range deps {
		if dep == target {
			count++
		}
	}
	return count
}

func complexExpectedSpecSchema() apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"name":     {Type: "string"},
			"enabled":  {Type: "boolean"},
			"replicas": {Type: "integer"},
			"labels": {
				Type: "object",
				AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
					Schema: &apiextensionsv1.JSONSchemaProps{Type: "string"},
				},
			},
			"workers": {
				Type: "array",
				Items: &apiextensionsv1.JSONSchemaPropsOrArray{
					Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"name": {Type: "string"},
							"tier": {Type: "string"},
						},
					},
				},
			},
			"containers": {
				Type: "array",
				Items: &apiextensionsv1.JSONSchemaPropsOrArray{
					Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"name":  {Type: "string"},
							"image": {Type: "string"},
							"ports": {
								Type: "array",
								Items: &apiextensionsv1.JSONSchemaPropsOrArray{
									Schema: &apiextensionsv1.JSONSchemaProps{
										Type: "object",
										Properties: map[string]apiextensionsv1.JSONSchemaProps{
											"containerPort": {Type: "integer"},
										},
									},
								},
							},
						},
					},
				},
			},
			"vpcCIDRBlocks": {
				Type: "array",
				Items: &apiextensionsv1.JSONSchemaPropsOrArray{
					Schema: &apiextensionsv1.JSONSchemaProps{Type: "string"},
				},
			},
			"vpcDisallowSecurityGroupDefaultRules": {Type: "boolean"},
			"vpcEnableDNSHostnames":                {Type: "boolean"},
			"vpcEnableDNSSupport":                  {Type: "boolean"},
			"vpcInstanceTenancy":                   {Type: "string"},
			"vpcIpv4NetmaskLength":                 {Type: "integer"},
			"vpcTags": {
				Type: "array",
				Items: &apiextensionsv1.JSONSchemaPropsOrArray{
					Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"key":   {Type: "string"},
							"value": {Type: "string"},
						},
					},
				},
			},
		},
	}
}

func complexExpectedStatusSchema() apiextensionsv1.JSONSchemaProps {
	return apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"sourceName": {Type: "string"},
			"deployment": {
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"name": {Type: "string"},
				},
			},
			"network": {
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"vpc": {
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"name":        {Type: "string"},
							"primaryCIDR": {Type: "string"},
							"tagCount":    {Type: "integer"},
							"dnsSupport":  {Type: "boolean"},
						},
					},
				},
			},
			"observability": {
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"workSummary": {Type: "string"},
					"copies": {
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"count":     {Type: "integer"},
							"hasCanary": {Type: "boolean"},
						},
					},
				},
			},
			"state": {Type: "string"},
			"conditions": {
				Type: "array",
				Items: &apiextensionsv1.JSONSchemaPropsOrArray{
					Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"type":               {Type: "string"},
							"status":             {Type: "string"},
							"reason":             {Type: "string"},
							"message":            {Type: "string"},
							"lastTransitionTime": {Type: "string"},
							"observedGeneration": {Type: "integer"},
						},
					},
				},
			},
		},
	}
}

func assertExactJSONSchema(g Gomega, actual, expected apiextensionsv1.JSONSchemaProps) {
	actualJSON, err := json.Marshal(actual)
	g.Expect(err).ToNot(HaveOccurred())

	expectedJSON, err := json.Marshal(expected)
	g.Expect(err).ToNot(HaveOccurred())

	g.Expect(actualJSON).To(MatchJSON(expectedJSON))
}
