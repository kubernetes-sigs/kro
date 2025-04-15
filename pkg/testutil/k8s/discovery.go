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

package k8s

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/testing"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// FakeResolver implements resolver.SchemaResolver for testing
type FakeResolver struct {
	schemas map[schema.GroupVersionKind]*spec.Schema
}

// ResolveSchema implements resolver.SchemaResolver
func (f *FakeResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	if schema, ok := f.schemas[gvk]; ok {
		return schema, nil
	}
	return nil, fmt.Errorf("schema not found for GVK: %v", gvk)
}

// AddSchema adds a new schema to the resolver
func (f *FakeResolver) AddSchema(gvk schema.GroupVersionKind, schema *spec.Schema) {
	f.schemas[gvk] = schema
}

// Helper to create ACK common status schema
func ackStatusSchema() map[string]spec.Schema {
	return map[string]spec.Schema{
		"ackResourceMetadata": {
			SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"arn":            {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"ownerAccountID": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"region":         {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				},
			},
		},
		"conditions": {
			SchemaProps: spec.SchemaProps{
				Type: []string{"array"},
				Items: &spec.SchemaOrArray{
					Schema: &spec.Schema{
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: map[string]spec.Schema{
								"lastTransitionTime": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"status":             {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"type":               {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
							},
						},
					},
				},
			},
		},
	}
}

func NewFakeResolver() (*FakeResolver, *fake.FakeDiscovery) {
	schemas := map[schema.GroupVersionKind]*spec.Schema{
		// ACK EC2 resources
		{Group: "ec2.services.k8s.aws", Version: "v1alpha1", Kind: "VPC"}: {
			SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"metadata":   metadataSchema(),
					"spec": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: map[string]spec.Schema{
								"cidrBlocks":         {SchemaProps: spec.SchemaProps{Type: []string{"array"}, Items: &spec.SchemaOrArray{Schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}}}}},
								"enableDNSHostnames": {SchemaProps: spec.SchemaProps{Type: []string{"boolean"}}},
								"enableDNSSupport":   {SchemaProps: spec.SchemaProps{Type: []string{"boolean"}}},
								"tags":               awsTagsSchema(),
							},
						},
					},
					"status": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: mergeSchemas(ackStatusSchema(), map[string]spec.Schema{
								"vpcID": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"state": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
							}),
						},
					},
				},
			},
		},
		{Group: "ec2.services.k8s.aws", Version: "v1alpha1", Kind: "Subnet"}: {
			SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"metadata":   metadataSchema(),
					"spec": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: map[string]spec.Schema{
								"cidrBlock": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"vpcID":     {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"tags":      awsTagsSchema(),
							},
						},
					},
					"status": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: mergeSchemas(ackStatusSchema(), map[string]spec.Schema{
								"subnetID": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"state":    {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
							}),
						},
					},
				},
			},
		},
		{Group: "ec2.services.k8s.aws", Version: "v1alpha1", Kind: "SecurityGroup"}: {
			SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"metadata":   metadataSchema(),
					"spec": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: map[string]spec.Schema{
								"vpcID":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"description": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"tags":        awsTagsSchema(),
							},
						},
					},
					"status": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: mergeSchemas(ackStatusSchema(), map[string]spec.Schema{
								"id":    {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"state": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
							}),
						},
					},
				},
			},
		},
		// ACK EKS resources
		{Group: "eks.services.k8s.aws", Version: "v1alpha1", Kind: "Cluster"}: {
			SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"metadata":   metadataSchema(),
					"spec": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: map[string]spec.Schema{
								"name":    {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"roleARN": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"version": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"accessConfig": {
									SchemaProps: spec.SchemaProps{
										Type: []string{"object"},
										Properties: map[string]spec.Schema{
											"authenticationMode": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
										},
									},
								},
								"resourcesVPCConfig": {
									SchemaProps: spec.SchemaProps{
										Type: []string{"object"},
										Properties: map[string]spec.Schema{
											"endpointPrivateAccess": {SchemaProps: spec.SchemaProps{Type: []string{"boolean"}}},
											"endpointPublicAccess":  {SchemaProps: spec.SchemaProps{Type: []string{"boolean"}}},
											"subnetIDs": {
												SchemaProps: spec.SchemaProps{
													Type: []string{"array"},
													Items: &spec.SchemaOrArray{
														Schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
													},
												},
											},
										},
									},
								},
							},
						},
					},
					"status": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: mergeSchemas(ackStatusSchema(), map[string]spec.Schema{
								"status": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
							}),
						},
					},
				},
			},
		},
		{Group: "eks.services.k8s.aws", Version: "v1alpha1", Kind: "Nodegroup"}: {
			SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"metadata":   metadataSchema(),
					"spec": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: map[string]spec.Schema{
								"name":           {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"clusterName":    {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"nodeRole":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"version":        {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"releaseVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"subnets": {
									SchemaProps: spec.SchemaProps{
										Type: []string{"array"},
										Items: &spec.SchemaOrArray{
											Schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
										},
									},
								},
								"scalingConfig": {
									SchemaProps: spec.SchemaProps{
										Type: []string{"object"},
										Properties: map[string]spec.Schema{
											"minSize":     {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
											"maxSize":     {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
											"desiredSize": {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
										},
									},
								},
							},
						},
					},
					"status": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: mergeSchemas(ackStatusSchema(), map[string]spec.Schema{
								"status": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
							}),
						},
					},
				},
			},
		},
		// iam services
		{Group: "iam.services.k8s.aws", Version: "v1alpha1", Kind: "Policy"}: {
			SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"metadata":   metadataSchema(),
					"spec": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: map[string]spec.Schema{
								"name":     {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"document": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"tags":     awsTagsSchema(),
							},
						},
					},
					"status": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: mergeSchemas(ackStatusSchema(), map[string]spec.Schema{
								"policyID": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
							}),
						},
					},
				},
			},
		},
		{Group: "iam.services.k8s.aws", Version: "v1alpha1", Kind: "Role"}: {
			SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"metadata":   metadataSchema(),
					"spec": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: map[string]spec.Schema{
								"name":                     {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"assumeRolePolicyDocument": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"tags":                     awsTagsSchema(),
							},
						},
					},
					"status": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: mergeSchemas(ackStatusSchema(), map[string]spec.Schema{
								"roleID": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
							}),
						},
					},
				},
			},
		},
		// v1 resources
		{Version: "v1", Kind: "Pod"}: {
			SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"metadata":   metadataSchema(),
					"spec": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: map[string]spec.Schema{
								"nodeName": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"containers": {
									SchemaProps: spec.SchemaProps{
										Type: []string{"array"},
										Items: &spec.SchemaOrArray{
											Schema: &spec.Schema{
												SchemaProps: spec.SchemaProps{
													Type: []string{"object"},
													Properties: map[string]spec.Schema{
														"name":  {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
														"image": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
														"env": {
															SchemaProps: spec.SchemaProps{
																Type: []string{"array"},
																Items: &spec.SchemaOrArray{
																	Schema: &spec.Schema{
																		SchemaProps: spec.SchemaProps{
																			Type: []string{"object"},
																			Properties: map[string]spec.Schema{
																				"name":  {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
																				"value": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
																			},
																		},
																	},
																},
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
					"status": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: map[string]spec.Schema{
								"phase":  {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"hostIP": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"podIP":  {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
							},
						},
					},
				},
			},
		},
		// CRDs
		{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}: {
			SchemaProps: spec.SchemaProps{
				Type: []string{"object"},
				Properties: map[string]spec.Schema{
					"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					"metadata":   metadataSchema(),
					"spec": {
						SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							Properties: map[string]spec.Schema{
								"group":   {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"version": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
								"names": {
									SchemaProps: spec.SchemaProps{
										Type: []string{"object"},
										Properties: map[string]spec.Schema{
											"kind":     {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
											"listKind": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
											"singular": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
											"plural":   {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
										},
									},
								},
								"scope": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
							},
						},
					},
				},
			},
		},
	}

	fakeDiscovery := &fake.FakeDiscovery{Fake: &testing.Fake{}}

	fakeDiscovery.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "ec2.services.k8s.aws/v1alpha1",
			APIResources: []metav1.APIResource{
				{
					Name:       "vpcs",
					Namespaced: true,
					Kind:       "VPC",
					Verbs:      []string{"get", "list", "watch", "create", "update", "patch", "delete"},
				},
				{
					Name:       "subnets",
					Namespaced: true,
					Kind:       "Subnet",
					Verbs:      []string{"get", "list", "watch", "create", "update", "patch", "delete"},
				},
				{
					Name:       "securitygroups",
					Namespaced: true,
					Kind:       "SecurityGroup",
					Verbs:      []string{"get", "list", "watch", "create", "update", "patch", "delete"},
				},
			},
		},
		{
			GroupVersion: "iam.services.k8s.aws/v1alpha1",
			APIResources: []metav1.APIResource{
				{
					Name:       "policies",
					Namespaced: true,
					Kind:       "Policy",
					Verbs:      []string{"get", "list", "watch", "create", "update", "patch", "delete"},
				},
				{
					Name:       "roles",
					Namespaced: true,
					Kind:       "Role",
					Verbs:      []string{"get", "list", "watch", "create", "update", "patch", "delete"},
				},
			},
		},
		{
			GroupVersion: "eks.services.k8s.aws/v1alpha1",
			APIResources: []metav1.APIResource{
				{
					Name:       "clusters",
					Namespaced: true,
					Kind:       "Cluster",
					Verbs:      []string{"get", "list", "watch", "create", "update", "patch", "delete"},
				},
				{
					Name:       "nodegroups",
					Namespaced: true,
					Kind:       "Nodegroup",
					Verbs:      []string{"get", "list", "watch", "create", "update", "patch", "delete"},
				},
			},
		},
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{
					Name:       "pods",
					Namespaced: true,
					Kind:       "Pod",
					Verbs:      []string{"get", "list", "watch", "create", "update", "patch", "delete"},
				},
			},
		},
		// CRD
		{
			GroupVersion: "apiextensions.k8s.io/v1",
			APIResources: []metav1.APIResource{
				{
					Name:       "customresourcedefinitions",
					Namespaced: false,
					Kind:       "CustomResourceDefinition",
					Verbs:      []string{"get", "list", "watch", "create", "update", "patch", "delete"},
				},
			},
		},
	}

	return &FakeResolver{schemas: schemas}, fakeDiscovery
}

// Helper to create AWS tags schema
func awsTagsSchema() spec.Schema {
	return spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"array"},
			Items: &spec.SchemaOrArray{
				Schema: &spec.Schema{
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"key":   {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
							"value": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
						},
					},
				},
			},
		},
	}
}

// Helper to create common metadata schema that matches Kubernetes ObjectMeta
func metadataSchema() spec.Schema {
	return spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"name": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"string"},
					},
				},
				"namespace": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"string"},
					},
				},
				"labels": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						AdditionalProperties: &spec.SchemaOrBool{
							Allows: true,
							Schema: &spec.Schema{
								SchemaProps: spec.SchemaProps{
									Type: []string{"string"},
								},
							},
						},
					},
				},
				"annotations": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						AdditionalProperties: &spec.SchemaOrBool{
							Allows: true,
							Schema: &spec.Schema{
								SchemaProps: spec.SchemaProps{
									Type: []string{"string"},
								},
							},
						},
					},
				},
				"generateName": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"string"},
					},
				},
				"uid": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"string"},
					},
				},
				"resourceVersion": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"string"},
					},
				},
				"generation": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"integer"},
					},
				},
				"creationTimestamp": {
					SchemaProps: spec.SchemaProps{
						Type:   []string{"string"},
						Format: "date-time",
					},
				},
				"deletionTimestamp": {
					SchemaProps: spec.SchemaProps{
						Type:   []string{"string"},
						Format: "date-time",
					},
				},
				"deletionGracePeriodSeconds": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"integer"},
					},
				},
				"finalizers": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"array"},
						Items: &spec.SchemaOrArray{
							Schema: &spec.Schema{
								SchemaProps: spec.SchemaProps{
									Type: []string{"string"},
								},
							},
						},
					},
				},
				"ownerReferences": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"array"},
						Items: &spec.SchemaOrArray{
							Schema: &spec.Schema{
								SchemaProps: spec.SchemaProps{
									Type: []string{"object"},
									Properties: map[string]spec.Schema{
										"apiVersion": {
											SchemaProps: spec.SchemaProps{
												Type: []string{"string"},
											},
										},
										"kind": {
											SchemaProps: spec.SchemaProps{
												Type: []string{"string"},
											},
										},
										"name": {
											SchemaProps: spec.SchemaProps{
												Type: []string{"string"},
											},
										},
										"uid": {
											SchemaProps: spec.SchemaProps{
												Type: []string{"string"},
											},
										},
										"controller": {
											SchemaProps: spec.SchemaProps{
												Type: []string{"boolean"},
											},
										},
										"blockOwnerDeletion": {
											SchemaProps: spec.SchemaProps{
												Type: []string{"boolean"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// Helper to merge two schema maps
func mergeSchemas(a, b map[string]spec.Schema) map[string]spec.Schema {
	merged := make(map[string]spec.Schema)
	for k, v := range a {
		merged[k] = v
	}
	for k, v := range b {
		merged[k] = v
	}
	return merged
}
