// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	"github.com/awslabs/kro/internal/graph/emulator"
	"github.com/awslabs/kro/internal/graph/variable"
	"github.com/awslabs/kro/internal/testutil/generator"
	"github.com/awslabs/kro/internal/testutil/k8s"
)

func TestGraphBuilder_Validation(t *testing.T) {
	fakeResolver, fakeDiscovery := k8s.NewFakeResolver()
	builder := &Builder{
		schemaResolver:   fakeResolver,
		discoveryClient:  fakeDiscovery,
		resourceEmulator: emulator.NewEmulator(),
	}

	tests := []struct {
		name              string
		resourceGroupOpts []generator.ResourceGroupOption
		wantErr           bool
		errMsg            string
	}{
		{
			name: "invalid resource type",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("vpc", map[string]interface{}{
					"apiVersion": "unknown.k8s.aws/v1alpha1", // Unknown API group
					"kind":       "VPC",
					"metadata": map[string]interface{}{
						"name": "test-vpc",
					},
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "schema not found",
		},
		{
			name: "invalid resource name with operator",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("vpc-1", map[string]interface{}{ // Invalid name with operator
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "VPC",
					"metadata": map[string]interface{}{
						"name": "test-vpc",
					},
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "naming convention violation",
		},
		{
			name: "resource without a valid GVK",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("vpc", map[string]interface{}{ // Invalid name with operator
					"vvvvv": "ec2.services.k8s.aws/v1alpha1",
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "is not a valid Kubernetes object",
		},
		{
			name: "invalid CEL syntax in readyWhen",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("vpc", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "VPC",
					"metadata": map[string]interface{}{
						"name": "test-vpc",
					},
				}, []string{"invalid ! syntax"}, nil),
			},
			wantErr: true,
			errMsg:  "failed to parse readyWhen expressions",
		},
		{
			name: "invalid CEL syntax in includeWhen expression",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("vpc", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "VPC",
					"metadata": map[string]interface{}{
						"name": "test-vpc",
					},
				}, nil, []string{"invalid ! syntax"}),
			},
			wantErr: true,
			errMsg:  "failed to parse includeWhen expressions",
		},
		{
			name: "missing required field",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("vpc", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "VPC",
					// Missing metadata
					"spec": map[string]interface{}{
						"cidrBlocks": []interface{}{"10.0.0.0/16"},
					},
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "metadata field not found",
		},
		{
			name: "invalid field reference",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("subnet", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "test-subnet",
					},
					"spec": map[string]interface{}{
						"vpcID": "${vpc.status.nonexistentField}", // Invalid field
					},
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "failed to validate resource CEL expression",
		},
		{
			name: "valid VPC with valid conditional subnets",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name":          "string",
						"enableSubnets": "boolean",
					},
					nil,
				),
				generator.WithResource("vpc", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "VPC",
					"metadata": map[string]interface{}{
						"name": "test-vpc",
					},
					"spec": map[string]interface{}{
						"cidrBlocks":         []interface{}{"10.0.0.0/16"},
						"enableDNSSupport":   true,
						"enableDNSHostnames": true,
					},
				}, []string{"${vpc.status.state == 'available'}"}, nil),
				generator.WithResource("subnet1", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "test-subnet",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "10.0.1.0/24",
						"vpcID":     "${vpc.status.vpcID}",
					},
				}, []string{"${subnet1.status.state == 'available'}"}, []string{"${spec.enableSubnets == true}"}),
				generator.WithResource("subnet2", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "test-subnet-2",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "10.0.127.0/24",
						"vpcID":     "${vpc.status.vpcID}",
					},
				}, []string{"${subnet2.status.state == 'available'}"}, []string{"${spec.enableSubnets}"})},
			wantErr: false,
		},
		{
			name: "invalid resource type",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("vpc", map[string]interface{}{
					"apiVersion": "unknown.k8s.aws/v1alpha1", // Unknown API group
					"kind":       "VPC",
					"metadata": map[string]interface{}{
						"name": "test-vpc",
					},
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "schema not found",
		},
		{
			name: "invalid instance spec field type",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"port": "wrongtype",
					},
					nil,
				),
			},
			wantErr: true,
			errMsg:  "failed to build OpenAPI schema for instance",
		},
		{
			name: "invalid instance status field reference",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					map[string]interface{}{
						"status": "${nonexistent.status}", // invalid reference
					},
				),
				generator.WithResource("vpc", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "VPC",
					"metadata": map[string]interface{}{
						"name": "test-vpc",
					},
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "undeclared reference to 'nonexistent'",
		},
		{
			name: "invalid field type in resource spec",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("vpc", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "VPC",
					"metadata": map[string]interface{}{
						"name": "test-vpc",
					},
					"spec": map[string]interface{}{
						"cidrBlocks": "10.0.0.0/16", // should be array
					},
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "expected string type or AdditionalProperties for path spec.cidrBlocks",
		},
		{
			name: "crds aren't allowed to have variables in their spec fields",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("somecrd", map[string]interface{}{
					"apiVersion": "apiextensions.k8s.io/v1",
					"kind":       "CustomResourceDefinition",
					"metadata": map[string]interface{}{
						"name": "vpcs.ec2.services.k8s.aws",
					},
					"spec": map[string]interface{}{
						"group":   "ec2.services.k8s.aws",
						"version": "v1alpha1",
						"names": map[string]interface{}{
							"kind":     "VPC",
							"listKind": "VPCList",
							"singular": "vpc",
							"plural":   "vpcs",
						},
						"scope": "Namespaced-${spec.name}",
					},
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "CEL expressions are not supported for CRDs",
		},
		{
			name: "valid instance definition with complex types",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name":     "string",
						"port":     "integer | default=80",
						"tags":     "map[string]string",
						"replicas": "integer | default=3",
					},
					map[string]interface{}{
						"state": "${vpc.status.state}",
						"id":    "${vpc.status.vpcID}",
					},
				),
				generator.WithResource("vpc", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "VPC",
					"metadata": map[string]interface{}{
						"name": "test-vpc",
					},
					"spec": map[string]interface{}{
						"cidrBlocks": []interface{}{"10.0.0.0/16"},
					},
				}, nil, nil),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rg := generator.NewResourceGroup("test-group", tt.resourceGroupOpts...)
			_, err := builder.NewResourceGroup(rg)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestGraphBuilder_DependencyValidation(t *testing.T) {
	fakeResolver, fakeDiscovery := k8s.NewFakeResolver()
	builder := &Builder{
		schemaResolver:   fakeResolver,
		discoveryClient:  fakeDiscovery,
		resourceEmulator: emulator.NewEmulator(),
	}

	tests := []struct {
		name              string
		resourceGroupOpts []generator.ResourceGroupOption
		wantErr           bool
		errMsg            string
		validateDeps      func(*testing.T, *Graph)
	}{
		{
			name: "complex eks setup dependencies",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				// First layer: Base resources with no dependencies
				generator.WithResource("vpc", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "VPC",
					"metadata": map[string]interface{}{
						"name": "testvpc",
					},
					"spec": map[string]interface{}{
						"cidrBlocks": []interface{}{"10.0.0.0/16"},
					},
				}, nil, nil),
				generator.WithResource("clusterpolicy", map[string]interface{}{
					"apiVersion": "iam.services.k8s.aws/v1alpha1",
					"kind":       "Policy",
					"metadata": map[string]interface{}{
						"name": "clusterpolicy",
					},
					"spec": map[string]interface{}{
						"name":     "testclusterpolicy",
						"document": "{}",
					},
				}, nil, nil),
				// Second layer: Resources depending on first layer
				generator.WithResource("clusterrole", map[string]interface{}{
					"apiVersion": "iam.services.k8s.aws/v1alpha1",
					"kind":       "Role",
					"metadata": map[string]interface{}{
						"name": "clusterrole",
					},
					"spec": map[string]interface{}{
						"name":                     "${clusterpolicy.status.policyID}role",
						"assumeRolePolicyDocument": "{}",
					},
				}, nil, nil),
				generator.WithResource("subnet1", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "subnet1",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "10.0.1.0/24",
						"vpcID":     "${vpc.status.vpcID}",
					},
				}, nil, nil),
				generator.WithResource("subnet2", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "subnet2",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "10.0.2.0/24",
						"vpcID":     "${vpc.status.vpcID}",
					},
				}, nil, nil),
				// Third layer: EKS Cluster depending on roles and subnets
				generator.WithResource("cluster", map[string]interface{}{
					"apiVersion": "eks.services.k8s.aws/v1alpha1",
					"kind":       "Cluster",
					"metadata": map[string]interface{}{
						"name": "cluster",
					},
					"spec": map[string]interface{}{
						"name":    "testcluster",
						"roleARN": "${clusterrole.status.roleID}",
						"resourcesVPCConfig": map[string]interface{}{
							"subnetIDs": []interface{}{
								"${subnet1.status.subnetID}",
								"${subnet2.status.subnetID}",
							},
						},
					},
				}, nil, nil)},
			validateDeps: func(t *testing.T, g *Graph) {
				// Validate dependencies
				assert.Empty(t, g.Resources["vpc"].GetDependencies())
				assert.Empty(t, g.Resources["clusterpolicy"].GetDependencies())

				assert.Equal(t, []string{"vpc"}, g.Resources["subnet1"].GetDependencies())
				assert.Equal(t, []string{"vpc"}, g.Resources["subnet2"].GetDependencies())
				assert.Equal(t, []string{"clusterpolicy"}, g.Resources["clusterrole"].GetDependencies())

				clusterDeps := g.Resources["cluster"].GetDependencies()
				assert.Len(t, clusterDeps, 3)
				assert.Contains(t, clusterDeps, "clusterrole")
				assert.Contains(t, clusterDeps, "subnet1")
				assert.Contains(t, clusterDeps, "subnet2")

				// Validate topological order
				assert.Equal(t, []string{"clusterpolicy", "clusterrole", "vpc", "subnet1", "subnet2", "cluster"}, g.TopologicalOrder)
			},
		},
		{
			name: "missing dependency",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("subnet", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "subnet",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "10.0.0.0/24",
						"vpcID":     "${missingvpc.status.vpcID}",
					},
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "undeclared reference to 'missingvpc'",
		},
		{
			name: "cyclic dependency",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("role1", map[string]interface{}{
					"apiVersion": "iam.services.k8s.aws/v1alpha1",
					"kind":       "Role",
					"metadata": map[string]interface{}{
						"name": "${role2.metadata.name}1",
					},
					"spec": map[string]interface{}{
						"name":                     "testrole1",
						"assumeRolePolicyDocument": "{}",
					},
				}, nil, nil),
				generator.WithResource("role2", map[string]interface{}{
					"apiVersion": "iam.services.k8s.aws/v1alpha1",
					"kind":       "Role",
					"metadata": map[string]interface{}{
						"name": "${role1.metadata.name}2",
					},
					"spec": map[string]interface{}{
						"name":                     "testrole2",
						"assumeRolePolicyDocument": "{}",
					},
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "This would create a cycle",
		},
		{
			name: "independent pods",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("pod1", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "pod1",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "nginx1",
								"image": "nginx:latest",
							},
						},
					},
				}, nil, nil),
				generator.WithResource("pod2", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "pod2",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "nginx2",
								"image": "nginx:latest",
							},
						},
					},
				}, nil, nil),
				generator.WithResource("pod3", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "pod3",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "nginx3",
								"image": "nginx:latest",
							},
						},
					},
				}, nil, nil),
				generator.WithResource("pod4", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "pod4",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "nginx4",
								"image": "nginx:latest",
							},
						},
					},
				}, nil, nil),
			},
			validateDeps: func(t *testing.T, g *Graph) {
				assert.Len(t, g.Resources, 4)
				assert.Empty(t, g.Resources["pod1"].GetDependencies())
				assert.Empty(t, g.Resources["pod2"].GetDependencies())
				assert.Empty(t, g.Resources["pod3"].GetDependencies())
				assert.Empty(t, g.Resources["pod4"].GetDependencies())
				// Order doesn't matter as they're all independent
				assert.Len(t, g.TopologicalOrder, 4)
			},
		},
		{
			name: "cyclic pod dependencies",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("pod1", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "${pod4.status.podIP}app1",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "nginx1",
								"image": "nginx:latest",
							},
						},
					},
				}, nil, nil),
				generator.WithResource("pod2", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "${pod1.status.podIP}app2",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "nginx2",
								"image": "nginx:latest",
							},
						},
					},
				}, nil, nil),
				generator.WithResource("pod3", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "${pod2.status.podIP}app3",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "nginx3",
								"image": "nginx:latest",
							},
						},
					},
				}, nil, nil),
				generator.WithResource("pod4", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "${pod3.status.podIP}app4",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "nginx4",
								"image": "nginx:latest",
							},
						},
					},
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "This would create a cycle",
		},
		{
			name: "shared infrastructure dependencies",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				// Base infrastructure
				generator.WithResource("vpc", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "VPC",
					"metadata": map[string]interface{}{
						"name": "vpc",
					},
					"spec": map[string]interface{}{
						"cidrBlocks": []interface{}{"10.0.0.0/16"},
					},
				}, nil, nil),
				generator.WithResource("subnet1", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "subnet1",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "10.0.1.0/24",
						"vpcID":     "${vpc.status.vpcID}",
					},
				}, nil, nil),
				generator.WithResource("subnet2", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "subnet2",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "10.0.2.0/24",
						"vpcID":     "${vpc.status.vpcID}",
					},
				}, nil, nil),
				generator.WithResource("subnet3", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "subnet3",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "10.0.3.0/24",
						"vpcID":     "${vpc.status.vpcID}",
					},
				}, nil, nil),
				generator.WithResource("secgroup", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "SecurityGroup",
					"metadata": map[string]interface{}{
						"name": "secgroup",
					},
					"spec": map[string]interface{}{
						"vpcID": "${vpc.status.vpcID}",
					},
				}, nil, nil),
				generator.WithResource("policy", map[string]interface{}{
					"apiVersion": "iam.services.k8s.aws/v1alpha1",
					"kind":       "Policy",
					"metadata": map[string]interface{}{
						"name": "policy",
					},
					"spec": map[string]interface{}{
						"document": "{}",
					},
				}, nil, nil),
				generator.WithResource("role", map[string]interface{}{
					"apiVersion": "iam.services.k8s.aws/v1alpha1",
					"kind":       "Role",
					"metadata": map[string]interface{}{
						"name": "role",
					},
					"spec": map[string]interface{}{
						"name":                     "${policy.status.policyID}role",
						"assumeRolePolicyDocument": "{}",
					},
				}, nil, nil),
				// Three clusters using the same infrastructure
				generator.WithResource("cluster1", map[string]interface{}{
					"apiVersion": "eks.services.k8s.aws/v1alpha1",
					"kind":       "Cluster",
					"metadata": map[string]interface{}{
						"name": "cluster1",
					},
					"spec": map[string]interface{}{
						"roleARN": "${role.status.roleID}",
						"resourcesVPCConfig": map[string]interface{}{
							"subnetIDs": []interface{}{
								"${subnet1.status.subnetID}",
								"${subnet2.status.subnetID}",
								"${subnet3.status.subnetID}",
							},
						},
					},
				}, nil, nil),
				generator.WithResource("cluster2", map[string]interface{}{
					"apiVersion": "eks.services.k8s.aws/v1alpha1",
					"kind":       "Cluster",
					"metadata": map[string]interface{}{
						"name": "cluster2",
					},
					"spec": map[string]interface{}{
						"roleARN": "${role.status.roleID}",
						"resourcesVPCConfig": map[string]interface{}{
							"subnetIDs": []interface{}{
								"${subnet1.status.subnetID}",
								"${subnet2.status.subnetID}",
								"${subnet3.status.subnetID}",
							},
						},
					},
				}, nil, nil),
				generator.WithResource("cluster3", map[string]interface{}{
					"apiVersion": "eks.services.k8s.aws/v1alpha1",
					"kind":       "Cluster",
					"metadata": map[string]interface{}{
						"name": "cluster3",
					},
					"spec": map[string]interface{}{
						"roleARN": "${role.status.roleID}",
						"resourcesVPCConfig": map[string]interface{}{
							"subnetIDs": []interface{}{
								"${subnet1.status.subnetID}",
								"${subnet2.status.subnetID}",
								"${subnet3.status.subnetID}",
							},
						},
					},
				}, nil, nil),
				// Pod depending on all clusters
				generator.WithResource("monitor", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "monitor",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "monitor",
								"image": "monitor:latest",
								"env": []interface{}{
									map[string]interface{}{
										"name":  "CLUSTER1_ARN",
										"value": "${cluster1.status.ackResourceMetadata.arn}",
									},
									map[string]interface{}{
										"name":  "CLUSTER2_ARN",
										"value": "${cluster2.status.ackResourceMetadata.arn}",
									},
									map[string]interface{}{
										"name":  "CLUSTER3_ARN",
										"value": "${cluster3.status.ackResourceMetadata.arn}",
									},
								},
							},
						},
					},
				}, nil, nil),
			},
			validateDeps: func(t *testing.T, g *Graph) {
				// Base infrastructure dependencies
				assert.Empty(t, g.Resources["vpc"].GetDependencies())
				assert.Empty(t, g.Resources["policy"].GetDependencies())

				assert.Equal(t, []string{"vpc"}, g.Resources["subnet1"].GetDependencies())
				assert.Equal(t, []string{"vpc"}, g.Resources["subnet2"].GetDependencies())
				assert.Equal(t, []string{"vpc"}, g.Resources["subnet3"].GetDependencies())
				assert.Equal(t, []string{"vpc"}, g.Resources["secgroup"].GetDependencies())
				assert.Equal(t, []string{"policy"}, g.Resources["role"].GetDependencies())

				// Cluster dependencies
				clusterDeps := []string{"role", "subnet1", "subnet2", "subnet3"}
				assert.ElementsMatch(t, clusterDeps, g.Resources["cluster1"].GetDependencies())
				assert.ElementsMatch(t, clusterDeps, g.Resources["cluster2"].GetDependencies())
				assert.ElementsMatch(t, clusterDeps, g.Resources["cluster3"].GetDependencies())

				// Monitor pod dependencies
				monitorDeps := []string{"cluster1", "cluster2", "cluster3"}
				assert.ElementsMatch(t, monitorDeps, g.Resources["monitor"].GetDependencies())

				// Validate topological order
				assert.Equal(t, []string{
					"policy",
					"role",
					"vpc",
					"subnet1",
					"subnet2",
					"subnet3",
					"cluster1",
					"cluster2",
					"cluster3",
					"monitor",
					"secgroup",
				}, g.TopologicalOrder)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rg := generator.NewResourceGroup("testgroup", tt.resourceGroupOpts...)
			g, err := builder.NewResourceGroup(rg)

			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
				return
			}

			require.NoError(t, err)
			if tt.validateDeps != nil {
				tt.validateDeps(t, g)
			}
		})
	}
}

func TestGraphBuilder_ExpressionParsing(t *testing.T) {
	fakeResolver, fakeDiscovery := k8s.NewFakeResolver()
	builder := &Builder{
		schemaResolver:   fakeResolver,
		discoveryClient:  fakeDiscovery,
		resourceEmulator: emulator.NewEmulator(),
	}

	tests := []struct {
		name              string
		resourceGroupOpts []generator.ResourceGroupOption
		validateVars      func(*testing.T, *Graph)
	}{
		{
			name: "complex resource variable parsing",
			resourceGroupOpts: []generator.ResourceGroupOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"replicas":         "integer | default=3",
						"environment":      "string | default=dev",
						"createMonitoring": "boolean | default=false",
					},
					nil,
				),
				// Resource with no expressions
				generator.WithResource("policy", map[string]interface{}{
					"apiVersion": "iam.services.k8s.aws/v1alpha1",
					"kind":       "Policy",
					"metadata": map[string]interface{}{
						"name": "policy",
					},
					"spec": map[string]interface{}{
						"document": "{}",
					},
				}, nil, nil),
				// Resource with only readyWhen expressions
				generator.WithResource("vpc", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "VPC",
					"metadata": map[string]interface{}{
						"name": "vpc",
					},
					"spec": map[string]interface{}{
						"cidrBlocks": []interface{}{"10.0.0.0/16"},
					},
				}, []string{
					"${vpc.status.state == 'available'}",
					"${vpc.status.vpcID != ''}",
				}, nil),
				// Resource with mix of static and dynamic expressions
				generator.WithResource("subnet", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "subnet",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "10.0.1.0/24",
						"vpcID":     "${vpc.status.vpcID}",
						"tags": []interface{}{
							map[string]interface{}{
								"key":   "Environment",
								"value": "${spec.environment}",
							},
						},
					},
				}, []string{"${subnet.status.state == 'available'}"}, nil),
				// Non-standalone expressions
				generator.WithResource("cluster", map[string]interface{}{
					"apiVersion": "eks.services.k8s.aws/v1alpha1",
					"kind":       "Cluster",
					"metadata": map[string]interface{}{
						"name": "${vpc.metadata.name}cluster${spec.environment}",
					},
					"spec": map[string]interface{}{
						"name": "testcluster",
						"resourcesVPCConfig": map[string]interface{}{
							"subnetIDs": []interface{}{
								"${subnet.status.subnetID}",
							},
						},
					},
				}, []string{
					"${cluster.status.status == 'ACTIVE'}",
				}, []string{
					"${spec.createMonitoring}",
				}),
				// All the above combined
				generator.WithResource("monitor", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "monitor",
						"labels": map[string]interface{}{
							"environment":  "${spec.environment}",
							"cluster":      "${cluster.metadata.name}",
							"combined":     "${cluster.metadata.name}-${spec.environment}",
							"two.statics":  "${spec.environment}-static-${spec.replicas}",
							"two.dynamics": "${vpc.metadata.name}-${cluster.status.ackResourceMetadata.arn}",
						},
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "monitor",
								"image": "monitor:latest",
								"env": []interface{}{
									map[string]interface{}{
										"name":  "CLUSTER_ARN",
										"value": "${cluster.status.ackResourceMetadata.arn}",
									},
									map[string]interface{}{
										"name":  "REPLICAS",
										"value": "${spec.replicas}",
									},
								},
							},
						},
					},
				}, []string{
					"${monitor.status.phase == 'Running'}",
				}, []string{
					"${spec.createMonitoring == true}",
				}),
			},
			validateVars: func(t *testing.T, g *Graph) {
				// Verify resource with no expressions
				policy := g.Resources["policy"]
				assert.Empty(t, policy.variables)
				assert.Empty(t, policy.GetReadyWhenExpressions())
				assert.Empty(t, policy.GetIncludeWhenExpressions())

				// Verify resource with only readyWhen
				vpc := g.Resources["vpc"]
				assert.Empty(t, vpc.variables)
				assert.Equal(t, []string{
					"vpc.status.state == 'available'",
					"vpc.status.vpcID != ''",
				}, vpc.GetReadyWhenExpressions())
				assert.Empty(t, vpc.GetIncludeWhenExpressions())

				// Verify resource with mixed expressions
				subnet := g.Resources["subnet"]
				assert.Len(t, subnet.variables, 2)
				// Create expected variables to match against
				validateVariables(t, subnet.variables, []expectedVar{
					{
						path:                 "spec.vpcID",
						expressions:          []string{"vpc.status.vpcID"},
						kind:                 variable.ResourceVariableKindDynamic,
						standaloneExpression: true,
					},
					{
						path:                 "spec.tags[0].value",
						expressions:          []string{"spec.environment"},
						kind:                 variable.ResourceVariableKindStatic,
						standaloneExpression: true,
					},
				})

				// Verify resource with multiple expressions in one field
				cluster := g.Resources["cluster"]
				assert.Len(t, cluster.variables, 2)
				validateVariables(t, cluster.variables, []expectedVar{
					{
						path:                 "metadata.name",
						expressions:          []string{"vpc.metadata.name", "spec.environment"},
						kind:                 variable.ResourceVariableKindDynamic,
						standaloneExpression: false,
					},
					{
						path:                 "spec.resourcesVPCConfig.subnetIDs[0]",
						expressions:          []string{"subnet.status.subnetID"},
						kind:                 variable.ResourceVariableKindDynamic,
						standaloneExpression: true,
					},
				})
				assert.Equal(t, []string{"spec.createMonitoring"}, cluster.GetIncludeWhenExpressions())

				// Verify monitor pod with all types of expressions
				monitor := g.Resources["monitor"]
				assert.Len(t, monitor.variables, 7)
				validateVariables(t, monitor.variables, []expectedVar{
					{
						path:                 "metadata.labels.environment",
						expressions:          []string{"spec.environment"},
						kind:                 variable.ResourceVariableKindStatic,
						standaloneExpression: true,
					},
					{
						path:                 "metadata.labels.cluster",
						expressions:          []string{"cluster.metadata.name"},
						kind:                 variable.ResourceVariableKindDynamic,
						standaloneExpression: true,
					},
					{
						path:                 "metadata.labels.combined",
						expressions:          []string{"cluster.metadata.name", "spec.environment"},
						kind:                 variable.ResourceVariableKindDynamic,
						standaloneExpression: false,
					},
					{
						path:                 "metadata.labels[\"two.statics\"]",
						expressions:          []string{"spec.environment", "spec.replicas"},
						kind:                 variable.ResourceVariableKindStatic,
						standaloneExpression: false,
					},
					{
						path:                 "metadata.labels[\"two.dynamics\"]",
						expressions:          []string{"vpc.metadata.name", "cluster.status.ackResourceMetadata.arn"},
						kind:                 variable.ResourceVariableKindDynamic,
						standaloneExpression: false,
					},
					{
						path:                 "spec.containers[0].env[0].value",
						expressions:          []string{"cluster.status.ackResourceMetadata.arn"},
						kind:                 variable.ResourceVariableKindDynamic,
						standaloneExpression: true,
					},
					{
						path:                 "spec.containers[0].env[1].value",
						expressions:          []string{"spec.replicas"},
						kind:                 variable.ResourceVariableKindStatic,
						standaloneExpression: true,
					},
				})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rg := generator.NewResourceGroup("testgroup", tt.resourceGroupOpts...)
			g, err := builder.NewResourceGroup(rg)
			require.NoError(t, err)
			if tt.validateVars != nil {
				tt.validateVars(t, g)
			}
		})
	}
}

type expectedVar struct {
	path                 string
	expressions          []string
	kind                 variable.ResourceVariableKind
	standaloneExpression bool
}

func validateVariables(t *testing.T, actual []*variable.ResourceField, expected []expectedVar) {
	assert.Equal(t, len(expected), len(actual), "variable count mismatch")

	actualVars := make([]expectedVar, len(actual))
	for i, v := range actual {
		v.ExpectedSchema = nil
		actualVars[i] = expectedVar{
			path:                 v.Path,
			expressions:          v.Expressions,
			kind:                 v.Kind,
			standaloneExpression: v.StandaloneExpression,
		}
	}

	assert.ElementsMatch(t, expected, actualVars)
}

func TestNewBuilder(t *testing.T) {
	builder, err := NewBuilder(&rest.Config{})
	assert.Nil(t, err)
	assert.NotNil(t, builder)
}
