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

package graph

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	memory2 "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

func TestGraphBuilder_Validation(t *testing.T) {
	fakeResolver, fakeDiscovery := k8s.NewFakeResolver()
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(memory2.NewMemCacheClient(fakeDiscovery))
	builder := &Builder{
		schemaResolver: fakeResolver,
		restMapper:     restMapper,
	}

	tests := []struct {
		name                        string
		resourceGraphDefinitionOpts []generator.ResourceGraphDefinitionOption
		wantErr                     bool
		errMsg                      string
	}{
		{
			name: "invalid resource type",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
			name: "invalid resource id with operator",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("vpc-1", map[string]interface{}{ // Invalid id with operator
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
			name: "invalid KRO kind name",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"invalidKind", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
			},
			wantErr: true,
			errMsg:  "is not a valid KRO kind name",
		},
		{
			name: "resource without a valid GVK",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
			name: "cluster-scoped resource with namespace",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("crd", map[string]interface{}{
					"apiVersion": "apiextensions.k8s.io/v1",
					"kind":       "CustomResourceDefinition",
					"metadata": map[string]interface{}{
						"name":      "tests.kro.run",
						"namespace": "default",
					},
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "cluster-scoped and must not set metadata.namespace",
		},
		{
			name: "invalid CEL syntax in readyWhen",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
			name: "includeWhen expression reference a different resource",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
				generator.WithResource("subnet", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "VPC",
					"metadata": map[string]interface{}{
						"name": "test-vpc",
					},
				}, nil, []string{"${vpc.status.state == 'available'}"}),
			},
			wantErr: true,
			errMsg:  "failed to parse includeWhen expressions",
		},
		{
			name: "missing required field",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
			errMsg:  "found unknown resources",
		},
		{
			name: "valid VPC with valid conditional subnets",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
				}, []string{"${subnet1.status.state == 'available'}"}, []string{"${schema.spec.enableSubnets == true}"}),
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
				}, []string{"${subnet2.status.state == 'available'}"}, []string{"${schema.spec.enableSubnets}"})},
			wantErr: false,
		},
		{
			name: "invalid resource type",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
			errMsg:  "expected array type for path spec.cidrBlocks, got string",
		},
		{
			name: "crds aren't allowed to have variables in their spec fields",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
						"scope": "Namespaced-${schema.spec.name}",
					},
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "CEL expressions in CRDs are only supported for metadata fields",
		},
		{
			name: "crds are allowed to have CEL expressions in metadata.name",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"crdName": "string",
					},
					nil,
				),
				generator.WithResource("somecrd", map[string]interface{}{
					"apiVersion": "apiextensions.k8s.io/v1",
					"kind":       "CustomResourceDefinition",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.crdName}",
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
						"scope": "Namespaced",
					},
				}, nil, nil),
			},
			wantErr: false,
		},
		{
			name: "crds with dynamic external references work",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"crdName": "string",
					},
					nil,
				),
				generator.WithExternalRef("crd", &krov1alpha1.ExternalRef{
					APIVersion: "apiextensions.k8s.io/v1",
					Kind:       "CustomResourceDefinition",
					Metadata: krov1alpha1.ExternalRefMetadata{
						Name: "${schema.spec.crdName}",
					},
				}, nil, nil),
			},
			wantErr: false,
		},
		{
			name: "crds with string template in metadata.name work",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"crdName": "string",
					},
					nil,
				),
				generator.WithResource("somecrd", map[string]interface{}{
					"apiVersion": "apiextensions.k8s.io/v1",
					"kind":       "CustomResourceDefinition",
					"metadata": map[string]interface{}{
						// Non-standalone expression (string template) - should not panic
						"name": "crd-${schema.spec.crdName}",
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
						"scope": "Namespaced",
					},
				}, nil, nil),
			},
			wantErr: false,
		},
		{
			name: "valid instance definition with complex types",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
		{
			name: "valid instance definition with optional type",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					map[string]interface{}{
						"state": "${vpc.status.?state}",
						"vpcID": "${vpc.status.?vpcID}",
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
		{
			name: "invalid externalRef with forEach",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"items": "[]string",
					},
					nil,
				),
				generator.WithExternalRefAndForEach("vpcs", &krov1alpha1.ExternalRef{
					APIVersion: "ec2.services.k8s.aws/v1alpha1",
					Kind:       "VPC",
					Metadata: krov1alpha1.ExternalRefMetadata{
						Name: "external-vpc",
					},
				}, []krov1alpha1.ForEachDimension{
					{"value": "${schema.spec.items}"},
				}),
			},
			wantErr: true,
			errMsg:  "cannot use externalRef with forEach",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := generator.NewResourceGraphDefinition("test-group", tt.resourceGraphDefinitionOpts...)
			_, err := builder.NewResourceGraphDefinition(rgd)

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
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(memory2.NewMemCacheClient(fakeDiscovery))
	builder := &Builder{
		schemaResolver: fakeResolver,
		restMapper:     restMapper,
	}

	tests := []struct {
		name                        string
		resourceGraphDefinitionOpts []generator.ResourceGraphDefinitionOption
		wantErr                     bool
		errMsg                      string
		validateDeps                func(*testing.T, *Graph)
	}{
		{
			name: "complex eks setup dependencies",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
				assert.Empty(t, g.Resources["vpc"].Meta.Dependencies)
				assert.Empty(t, g.Resources["clusterpolicy"].Meta.Dependencies)

				assert.Equal(t, []string{"vpc"}, g.Resources["subnet1"].Meta.Dependencies)
				assert.Equal(t, []string{"vpc"}, g.Resources["subnet2"].Meta.Dependencies)
				assert.Equal(t, []string{"clusterpolicy"}, g.Resources["clusterrole"].Meta.Dependencies)

				clusterDeps := g.Resources["cluster"].Meta.Dependencies
				assert.Len(t, clusterDeps, 3)
				assert.Contains(t, clusterDeps, "clusterrole")
				assert.Contains(t, clusterDeps, "subnet1")
				assert.Contains(t, clusterDeps, "subnet2")

				// Validate topological order
				assert.Equal(t, []string{"vpc", "clusterpolicy", "clusterrole", "subnet1", "subnet2", "cluster"}, g.TopologicalOrder)
			},
		},
		{
			name: "missing dependency",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
			errMsg:  "found unknown resources",
		},
		{
			name: "cyclic dependency",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
			errMsg:  "graph contains a cycle",
		},
		{
			name: "independent pods",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
				assert.Empty(t, g.Resources["pod1"].Meta.Dependencies)
				assert.Empty(t, g.Resources["pod2"].Meta.Dependencies)
				assert.Empty(t, g.Resources["pod3"].Meta.Dependencies)
				assert.Empty(t, g.Resources["pod4"].Meta.Dependencies)
				// Order doesn't matter as they're all independent
				assert.Len(t, g.TopologicalOrder, 4)
			},
		},
		{
			name: "cyclic pod dependencies",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
			errMsg:  "graph contains a cycle",
		},
		{
			name: "shared infrastructure dependencies",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
				assert.Empty(t, g.Resources["vpc"].Meta.Dependencies)
				assert.Empty(t, g.Resources["policy"].Meta.Dependencies)

				assert.Equal(t, []string{"vpc"}, g.Resources["subnet1"].Meta.Dependencies)
				assert.Equal(t, []string{"vpc"}, g.Resources["subnet2"].Meta.Dependencies)
				assert.Equal(t, []string{"vpc"}, g.Resources["subnet3"].Meta.Dependencies)
				assert.Equal(t, []string{"vpc"}, g.Resources["secgroup"].Meta.Dependencies)
				assert.Equal(t, []string{"policy"}, g.Resources["role"].Meta.Dependencies)

				// Cluster dependencies
				clusterDeps := []string{"role", "subnet1", "subnet2", "subnet3"}
				assert.ElementsMatch(t, clusterDeps, g.Resources["cluster1"].Meta.Dependencies)
				assert.ElementsMatch(t, clusterDeps, g.Resources["cluster2"].Meta.Dependencies)
				assert.ElementsMatch(t, clusterDeps, g.Resources["cluster3"].Meta.Dependencies)

				// Monitor pod dependencies
				monitorDeps := []string{"cluster1", "cluster2", "cluster3"}
				assert.ElementsMatch(t, monitorDeps, g.Resources["monitor"].Meta.Dependencies)

				// Validate topological order
				assert.Equal(t, []string{
					"vpc",
					"subnet1",
					"subnet2",
					"subnet3",
					"secgroup",
					"policy",
					"role",
					"cluster1",
					"cluster2",
					"cluster3",
					"monitor",
				}, g.TopologicalOrder)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := generator.NewResourceGraphDefinition("testrgd", tt.resourceGraphDefinitionOpts...)
			g, err := builder.NewResourceGraphDefinition(rgd)

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
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(memory2.NewMemCacheClient(fakeDiscovery))
	builder := &Builder{
		schemaResolver: fakeResolver,
		restMapper:     restMapper,
	}

	tests := []struct {
		name                        string
		resourceGraphDefinitionOpts []generator.ResourceGraphDefinitionOption
		validateVars                func(*testing.T, *Graph)
	}{
		{
			name: "complex resource variable parsing",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"replicas":         "integer | default=3",
						"environment":      "string | default=dev",
						"region":           "string | default=us-west-2",
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
								"value": "${schema.spec.environment}",
							},
						},
					},
				}, []string{"${subnet.status.state == 'available'}"}, nil),
				// Non-standalone expressions
				generator.WithResource("cluster", map[string]interface{}{
					"apiVersion": "eks.services.k8s.aws/v1alpha1",
					"kind":       "Cluster",
					"metadata": map[string]interface{}{
						"name": "${vpc.metadata.name}cluster${schema.spec.environment}",
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
					"${schema.spec.createMonitoring}",
				}),
				// All the above combined
				generator.WithResource("monitor", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "monitor",
						"labels": map[string]interface{}{
							"environment":  "${schema.spec.environment}",
							"cluster":      "${cluster.metadata.name}",
							"combined":     "${cluster.metadata.name}-${schema.spec.environment}",
							"two.statics":  "${schema.spec.environment}-${schema.spec.region}",
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
										"name":  "REGION",
										"value": "${schema.spec.region}",
									},
								},
							},
						},
					},
				}, []string{
					"${monitor.status.phase == 'Running'}",
				}, []string{
					"${schema.spec.createMonitoring == true}",
				}),
			},
			validateVars: func(t *testing.T, g *Graph) {
				// Verify resource with no expressions
				policy := g.Resources["policy"]
				assert.Empty(t, policy.Variables)
				assert.Empty(t, policy.ReadyWhen)
				assert.Empty(t, policy.IncludeWhen)

				// Verify resource with only readyWhen
				vpc := g.Resources["vpc"]
				assert.Empty(t, vpc.Variables)
				assert.Equal(t, []string{
					"vpc.status.state == 'available'",
					"vpc.status.vpcID != ''",
				}, vpc.ReadyWhen)
				assert.Empty(t, vpc.IncludeWhen)

				// Verify resource with mixed expressions
				subnet := g.Resources["subnet"]
				assert.Len(t, subnet.Variables, 2)
				// Create expected variables to match against
				validateVariables(t, subnet.Variables, []expectedVar{
					{
						path:                 "spec.vpcID",
						expressions:          []string{"vpc.status.vpcID"},
						kind:                 variable.ResourceVariableKindDynamic,
						standaloneExpression: true,
					},
					{
						path:                 "spec.tags[0].value",
						expressions:          []string{"schema.spec.environment"},
						kind:                 variable.ResourceVariableKindStatic,
						standaloneExpression: true,
					},
				})

				// Verify resource with multiple expressions in one field
				cluster := g.Resources["cluster"]
				assert.Len(t, cluster.Variables, 2)
				validateVariables(t, cluster.Variables, []expectedVar{
					{
						path:                 "metadata.name",
						expressions:          []string{"vpc.metadata.name", "schema.spec.environment"},
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
				assert.Equal(t, []string{"schema.spec.createMonitoring"}, cluster.IncludeWhen)

				// Verify monitor pod with all types of expressions
				monitor := g.Resources["monitor"]
				assert.Len(t, monitor.Variables, 7)
				validateVariables(t, monitor.Variables, []expectedVar{
					{
						path:                 "metadata.labels.environment",
						expressions:          []string{"schema.spec.environment"},
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
						expressions:          []string{"cluster.metadata.name", "schema.spec.environment"},
						kind:                 variable.ResourceVariableKindDynamic,
						standaloneExpression: false,
					},
					{
						path:                 "metadata.labels[\"two.statics\"]",
						expressions:          []string{"schema.spec.environment", "schema.spec.region"},
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
						expressions:          []string{"schema.spec.region"},
						kind:                 variable.ResourceVariableKindStatic,
						standaloneExpression: true,
					},
				})
				assert.Equal(t, []string{"monitor.status.phase == 'Running'"}, monitor.ReadyWhen)
				assert.Equal(t, []string{"schema.spec.createMonitoring == true"}, monitor.IncludeWhen)
			},
		},
		{
			name: "crds not failing when cel is present in other resources",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
						"name": "somecrd.ec2.services.k8s.aws",
					},
					"spec": map[string]interface{}{
						"group":   "ec2.services.k8s.aws",
						"version": "v1alpha1",
						"names": map[string]interface{}{
							"kind":     "SomeCRD",
							"listKind": "SomeCRDList",
							"singular": "SomeCRD",
							"plural":   "SomeCRDs",
						},
						"scope": "Namespaced",
					},
				}, nil, nil),
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
				generator.WithResource("subnet1", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "subnet1",
					},
					"spec": map[string]interface{}{
						"vpcID":     "${vpc.metadata.name}",
						"cidrBlock": "10.0.1.0/24",
					},
				}, nil, nil),
			},
			validateVars: func(t *testing.T, g *Graph) {
				somecrd := g.Resources["somecrd"]
				assert.Empty(t, somecrd.Variables)
				assert.Empty(t, somecrd.ReadyWhen)
				assert.Empty(t, somecrd.IncludeWhen)

				// Verify resource with only readyWhen
				vpc := g.Resources["vpc"]
				assert.Empty(t, vpc.Variables)
				assert.Equal(t, []string{
					"vpc.status.state == 'available'",
					"vpc.status.vpcID != ''",
				}, vpc.ReadyWhen)
				assert.Empty(t, vpc.IncludeWhen)

				// Verify resource with mixed expressions
				subnet := g.Resources["subnet1"]
				assert.Len(t, subnet.Variables, 1)
				// Create expected variables to match against
				validateVariables(t, subnet.Variables, []expectedVar{
					{
						path:                 "spec.vpcID",
						expressions:          []string{"vpc.metadata.name"},
						kind:                 variable.ResourceVariableKindDynamic,
						standaloneExpression: true,
					},
				})

			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := generator.NewResourceGraphDefinition("testrgd", tt.resourceGraphDefinitionOpts...)
			g, err := builder.NewResourceGraphDefinition(rgd)
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
		actualVars[i] = expectedVar{
			path:                 v.Path,
			expressions:          v.Expressions,
			kind:                 v.Kind,
			standaloneExpression: v.StandaloneExpression,
		}
	}

	assert.ElementsMatch(t, expected, actualVars)
}

func TestGraphBuilder_CELTypeChecking(t *testing.T) {
	fakeResolver, fakeDiscovery := k8s.NewFakeResolver()
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(memory2.NewMemCacheClient(fakeDiscovery))
	builder := &Builder{
		schemaResolver: fakeResolver,
		restMapper:     restMapper,
	}

	tests := []struct {
		name                        string
		resourceGraphDefinitionOpts []generator.ResourceGraphDefinitionOption
		wantErr                     bool
		errMsg                      string
	}{
		// Test 1: ObjectMeta field access validation
		{
			name: "valid access to metadata.name",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					map[string]interface{}{
						"podName": "${pod.metadata.name}",
					},
				),
				generator.WithResource("pod", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "test-pod",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "nginx",
								"image": "nginx:latest",
							},
						},
					},
				}, nil, nil),
			},
			wantErr: false,
		},
		{
			name: "valid access to metadata.labels",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
						"cidrBlocks": []interface{}{"10.0.0.0/16"},
					},
				}, nil, nil),
				generator.WithResource("subnet", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name":   "test-subnet",
						"labels": "${vpc.metadata.labels}",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "10.0.1.0/24",
						"vpcID":     "${vpc.status.vpcID}",
					},
				}, nil, nil),
			},
			wantErr: false,
		},
		{
			name: "valid access to metadata.annotations",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
						"cidrBlocks": []interface{}{"10.0.0.0/16"},
					},
				}, nil, nil),
				generator.WithResource("subnet", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name":        "test-subnet",
						"annotations": "${vpc.metadata.annotations}",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "10.0.1.0/24",
						"vpcID":     "${vpc.status.vpcID}",
					},
				}, nil, nil),
			},
			wantErr: false,
		},
		{
			name: "invalid access to non-existent metadata field",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					map[string]interface{}{
						"invalid": "${pod.metadata.nonExistentField}",
					},
				),
				generator.WithResource("pod", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "test-pod",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "nginx",
								"image": "nginx:latest",
							},
						},
					},
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "undefined field 'nonExistentField'",
		},

		// Test 2: readyWhen and includeWhen type checking
		{
			name: "readyWhen returning non-boolean type",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
						"cidrBlocks": []interface{}{"10.0.0.0/16"},
					},
				}, []string{"${vpc.status.vpcID}"}, nil), // Returns string, not bool
			},
			wantErr: true,
			errMsg:  "must return bool",
		},
		{
			name: "includeWhen returning non-boolean type",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"count": "integer",
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
						"cidrBlocks": []interface{}{"10.0.0.0/16"},
					},
				}, nil, []string{"${schema.spec.count}"}), // Returns integer, not bool
			},
			wantErr: true,
			errMsg:  "must return bool",
		},
		{
			name: "valid readyWhen with boolean expression",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
						"cidrBlocks": []interface{}{"10.0.0.0/16"},
					},
				}, []string{"${vpc.status.state == 'available'}"}, nil),
			},
			wantErr: false,
		},
		{
			name: "valid includeWhen with boolean expression",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"enabled": "boolean",
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
						"cidrBlocks": []interface{}{"10.0.0.0/16"},
					},
				}, nil, []string{"${schema.spec.enabled}"}),
			},
			wantErr: false,
		},

		// Test 3: Optional type handling
		{
			name: "valid optional type in status field",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					map[string]interface{}{
						"state": "${vpc.status.?state}",
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
		{
			name: "optional type mismatch with helpful error",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"Test", "v1alpha1",
					map[string]interface{}{
						"count": "integer",
					},
					map[string]interface{}{
						"name": "${vpc.status.?state}", // state is string
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
			wantErr: false, // optional string is assignable to string
		},

		// Test 5: Metadata field type mismatches
		{
			name: "using metadata.name as object instead of string",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
						"cidrBlocks": []interface{}{"10.0.0.0/16"},
					},
				}, nil, nil),
				generator.WithResource("subnet", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "test-subnet",
					},
					"spec": map[string]interface{}{
						"vpcID": "${vpc.metadata.name.field}", // name is string, not object
					},
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "does not support field selection",
		},
		{
			name: "using metadata.labels value with correct type",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
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
						"labels": map[string]interface{}{
							"env": "prod",
						},
					},
					"spec": map[string]interface{}{
						"cidrBlocks": []interface{}{"10.0.0.0/16"},
					},
				}, nil, nil),
				generator.WithResource("subnet", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "${vpc.metadata.labels['env']}-subnet", // Accessing map value
					},
					"spec": map[string]interface{}{
						"cidrBlock": "10.0.1.0/24",
						"vpcID":     "${vpc.status.vpcID}",
					},
				}, nil, nil),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := generator.NewResourceGraphDefinition("test-cel-types", tt.resourceGraphDefinitionOpts...)
			_, err := builder.NewResourceGraphDefinition(rgd)

			if tt.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errMsg)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestNewBuilder(t *testing.T) {
	builder, err := NewBuilder(&rest.Config{}, &http.Client{})
	assert.Nil(t, err)
	assert.NotNil(t, builder)
}

func TestGraphBuilder_StructuralTypeCompatibility(t *testing.T) {
	fakeResolver, fakeDiscovery := k8s.NewFakeResolver()
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(memory2.NewMemCacheClient(fakeDiscovery))
	builder := &Builder{
		schemaResolver: fakeResolver,
		restMapper:     restMapper,
	}

	tests := []struct {
		name                        string
		resourceGraphDefinitionOpts []generator.ResourceGraphDefinitionOption
		wantErr                     bool
		errMsg                      string
	}{
		{
			name: "pass - custom types with array in resources",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"PodSetup", "v1alpha1",
					map[string]interface{}{
						"podName":    "string",
						"containers": "[]containerConfig",
					},
					nil,
					generator.WithTypes(map[string]interface{}{
						"containerConfig": map[string]interface{}{
							"name":  "string",
							"image": "string",
						},
					}),
				),
				generator.WithResource("pod", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.podName}",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "${schema.spec.containers[0].name}",
								"image": "${schema.spec.containers[0].image}",
							},
						},
					},
				}, nil, nil),
			},
			wantErr: false,
		},
		{
			name: "pass - custom type struct field in resources",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"AppSetup", "v1alpha1",
					map[string]interface{}{
						"app": "appConfig",
					},
					nil,
					generator.WithTypes(map[string]interface{}{
						"appConfig": map[string]interface{}{
							"name":  "string",
							"image": "string",
						},
					}),
				),
				generator.WithResource("pod", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.app.name}",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "${schema.spec.app.name}",
								"image": "${schema.spec.app.image}",
							},
						},
					},
				}, nil, nil),
			},
			wantErr: false,
		},
		{
			name: "pass - subset struct in resources",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"EnvSetup", "v1alpha1",
					map[string]interface{}{
						"basic": "basicEnv",
						"full":  "fullEnv",
					},
					nil,
					generator.WithTypes(map[string]interface{}{
						"basicEnv": map[string]interface{}{
							"name": "string",
						},
						"fullEnv": map[string]interface{}{
							"name":  "string",
							"value": "string",
						},
					}),
				),
				generator.WithResource("podBasic", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "basic-pod",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "${schema.spec.basic.name}",
								"image": "nginx",
							},
						},
					},
				}, nil, nil),
				generator.WithResource("podFull", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "full-pod",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "${schema.spec.full.name}",
								"image": "nginx",
								"env": []interface{}{
									map[string]interface{}{
										"name":  "${schema.spec.full.name}",
										"value": "${schema.spec.full.value}",
									},
								},
							},
						},
					},
				}, nil, nil),
			},
			wantErr: false,
		},
		{
			name: "pass - nested array access with ports",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"ServiceWithPorts", "v1alpha1",
					map[string]interface{}{
						"appName": "string",
						"ports":   "[]portConfig",
					},
					nil,
					generator.WithTypes(map[string]interface{}{
						"portConfig": map[string]interface{}{
							"name":          "string",
							"containerPort": "integer",
							"protocol":      "string",
						},
					}),
				),
				generator.WithResource("pod", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.appName}",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "${schema.spec.appName}",
								"image": "nginx",
								"ports": []interface{}{
									map[string]interface{}{
										"name":          "${schema.spec.ports[0].name}",
										"containerPort": "${schema.spec.ports[0].containerPort}",
										"protocol":      "${schema.spec.ports[0].protocol}",
									},
								},
							},
						},
					},
				}, nil, nil),
			},
			wantErr: false,
		},
		{
			name: "pass - full containers array (subset type)",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"ContainerSetup", "v1alpha1",
					map[string]interface{}{
						"containers": "[]containerSubset",
					},
					nil,
					generator.WithTypes(map[string]interface{}{
						"containerSubset": map[string]interface{}{
							"name":  "string",
							"image": "string",
						},
					}),
				),
				generator.WithResource("pod", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "test-pod",
					},
					"spec": map[string]interface{}{
						"containers": "${schema.spec.containers}",
					},
				}, nil, nil),
			},
			wantErr: false,
		},
		{
			name: "fail - full containers array with extra field",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"ContainerSetup", "v1alpha1",
					map[string]interface{}{
						"containers": "[]containerWithExtra",
					},
					nil,
					generator.WithTypes(map[string]interface{}{
						"containerWithExtra": map[string]interface{}{
							"name":       "string",
							"image":      "string",
							"extraField": "string",
						},
					}),
				),
				generator.WithResource("pod", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "test-pod",
					},
					"spec": map[string]interface{}{
						"containers": "${schema.spec.containers}",
					},
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "exists in output but not in expected type",
		},
		{
			name: "fail - primitive type mismatch",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"ContainerSetup", "v1alpha1",
					map[string]interface{}{
						"containers": "[]containerWrongType",
					},
					nil,
					generator.WithTypes(map[string]interface{}{
						"containerWrongType": map[string]interface{}{
							"name":  "integer",
							"image": "string",
						},
					}),
				),
				generator.WithResource("pod", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "test-pod",
					},
					"spec": map[string]interface{}{
						"containers": "${schema.spec.containers}",
					},
				}, nil, nil),
			},
			wantErr: true,
			errMsg:  "kind mismatch",
		},
		{
			name: "pass - cross-pod reference with annotations, labels, containers",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"PodReplicator", "v1alpha1",
					map[string]interface{}{
						"sourcePodName": "string",
						"targetPodName": "string",
					},
					nil,
				),
				generator.WithResource("sourcePod", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.sourcePodName}",
						"labels": map[string]interface{}{
							"app": "source",
						},
						"annotations": map[string]interface{}{
							"description": "source pod",
						},
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "nginx",
								"image": "nginx:1.19",
							},
						},
					},
				}, nil, nil),
				generator.WithResource("targetPod", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name":        "${schema.spec.targetPodName}",
						"labels":      "${sourcePod.metadata.labels}",
						"annotations": "${sourcePod.metadata.annotations}",
					},
					"spec": map[string]interface{}{
						"containers": "${sourcePod.spec.containers}",
					},
				}, nil, nil),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := generator.NewResourceGraphDefinition("testrgd", tt.resourceGraphDefinitionOpts...)
			_, err := builder.NewResourceGraphDefinition(rgd)
			if tt.wantErr {
				if !assert.Error(t, err) {
					t.Logf("Expected error but got nil")
					return
				}
				if tt.errMsg != "" {
					assert.Contains(t, err.Error(), tt.errMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestGraphBuilder_ForEachParsing(t *testing.T) {
	fakeResolver, fakeDiscovery := k8s.NewFakeResolver()
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(memory2.NewMemCacheClient(fakeDiscovery))
	builder := &Builder{
		schemaResolver: fakeResolver,
		restMapper:     restMapper,
	}

	tests := []struct {
		name                        string
		resourceGraphDefinitionOpts []generator.ResourceGraphDefinitionOption
		wantErr                     bool
		errMsg                      string
		validateGraph               func(t *testing.T, graph *Graph)
	}{
		{
			name: "valid forEach with single iterator from schema",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"WorkerPool", "v1alpha1",
					map[string]interface{}{
						"workers": "[]string",
					},
					nil,
				),
				generator.WithResourceCollection("workerPods", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "${workerName}",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "worker",
								"image": "nginx:latest",
							},
						},
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"workerName": "${schema.spec.workers}"},
					},
					nil, nil),
			},
			wantErr: false,
			validateGraph: func(t *testing.T, graph *Graph) {
				resource := graph.Resources["workerPods"]
				require.NotNil(t, resource)
				iterators := resource.ForEach
				require.Len(t, iterators, 1)
				assert.Equal(t, "workerName", iterators[0].Name)
				assert.Equal(t, "schema.spec.workers", iterators[0].Expression)
			},
		},
		{
			name: "valid forEach with multiple iterators (cartesian product)",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"MultiRegion", "v1alpha1",
					map[string]interface{}{
						"regions": "[]string",
						"tiers":   "[]string",
					},
					nil,
				),
				generator.WithResourceCollection("pods", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "${region}-${tier}-pod",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "app",
								"image": "nginx:latest",
							},
						},
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"region": "${schema.spec.regions}"},
						{"tier": "${schema.spec.tiers}"},
					},
					nil, nil),
			},
			wantErr: false,
			validateGraph: func(t *testing.T, graph *Graph) {
				resource := graph.Resources["pods"]
				require.NotNil(t, resource)
				iterators := resource.ForEach
				require.Len(t, iterators, 2)
				assert.Equal(t, "region", iterators[0].Name)
				assert.Equal(t, "tier", iterators[1].Name)
			},
		},
		{
			name: "forEach with collection chaining (depends on another resource)",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"WorkerPool", "v1alpha1",
					map[string]interface{}{
						"workers": "[]string",
					},
					nil,
				),
				generator.WithResourceCollection("workerPods", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "${worker}",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "worker",
								"image": "nginx:latest",
							},
						},
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"worker": "${schema.spec.workers}"},
					},
					nil, nil),
				// monitorPod depends on workerPods collection
				// Since workerPods is a collection, it's typed as list(Pod)
				// We use size() to reference the collection and create a dependency
				generator.WithResource("monitorPod", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "${string(size(workerPods))}-monitor",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "monitor",
								"image": "monitor:latest",
							},
						},
					},
				}, nil, nil),
			},
			wantErr: false,
			validateGraph: func(t *testing.T, graph *Graph) {
				// monitorPod should depend on workerPods
				monitorPod := graph.Resources["monitorPod"]
				require.NotNil(t, monitorPod)
				assert.Contains(t, monitorPod.Meta.Dependencies, "workerPods")
			},
		},
		{
			name: "forEach expression must be standalone - embedded expression rejected",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"WorkerPool", "v1alpha1",
					map[string]interface{}{
						"workers": "[]string",
					},
					nil,
				),
				generator.WithResourceCollection("workerPods", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "worker",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "worker",
								"image": "nginx:latest",
							},
						},
					},
				},
					[]krov1alpha1.ForEachDimension{
						// Non-standalone: expression embedded in string
						{"name": "prefix-${schema.spec.workers}"},
					},
					nil, nil),
			},
			wantErr: true,
			errMsg:  "only standalone expressions are allowed",
		},
		{
			name: "forEach expression must be standalone - multiple expressions rejected",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"WorkerPool", "v1alpha1",
					map[string]interface{}{
						"workers": "[]string",
						"prefix":  "string",
					},
					nil,
				),
				generator.WithResourceCollection("workerPods", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "worker",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "worker",
								"image": "nginx:latest",
							},
						},
					},
				},
					[]krov1alpha1.ForEachDimension{
						// Non-standalone: multiple expressions
						{"name": "${schema.spec.prefix}-${schema.spec.workers}"},
					},
					nil, nil),
			},
			wantErr: true,
			errMsg:  "only standalone expressions are allowed",
		},
		{
			name: "resource without forEach has empty iterators",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"Simple", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("pod", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "simple-pod",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "app",
								"image": "nginx:latest",
							},
						},
					},
				}, nil, nil),
			},
			wantErr: false,
			validateGraph: func(t *testing.T, graph *Graph) {
				resource := graph.Resources["pod"]
				require.NotNil(t, resource)
				assert.Empty(t, resource.ForEach)
			},
		},
		{
			name: "collection cannot reference itself in readyWhen",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"PodCollection", "v1alpha1",
					map[string]interface{}{
						"names": "[]string",
					},
					nil,
				),
				generator.WithResourceCollection("pods", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "${name}",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "app",
								"image": "nginx:latest",
							},
						},
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"name": "${schema.spec.names}"},
					},
					// Self-reference: using pods.all() instead of each
					[]string{"${pods.all(p, p.status.phase == 'Running')}"},
					nil),
			},
			wantErr: true,
			errMsg:  "resource \"pods\" readyWhen expression",
		},
		{
			name: "collection readyWhen cannot reference other resources",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"PodCollection", "v1alpha1",
					map[string]interface{}{
						"names": "[]string",
					},
					nil,
				),
				// First resource: a regular Pod
				generator.WithResource("mainPod", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "main-pod",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "main",
								"image": "nginx:latest",
							},
						},
					},
				}, nil, nil),
				// Second resource: collection that incorrectly references mainPod in readyWhen
				generator.WithResourceCollection("workerPods", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "${name}",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "worker",
								"image": "nginx:latest",
							},
						},
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"name": "${schema.spec.names}"},
					},
					// Invalid: referencing another resource instead of each
					[]string{"${mainPod.status.phase == 'Running'}"},
					nil),
			},
			wantErr: true,
			errMsg:  "resource \"workerPods\" readyWhen expression",
		},
		{
			name: "collection with valid each-based readyWhen",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"PodCollection", "v1alpha1",
					map[string]interface{}{
						"names": "[]string",
					},
					nil,
				),
				generator.WithResourceCollection("pods", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "${name}",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "app",
								"image": "nginx:latest",
							},
						},
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"name": "${schema.spec.names}"},
					},
					// Valid: using each keyword for per-item readiness
					[]string{"${each.status.phase == 'Running'}"},
					nil),
			},
			wantErr: false,
			validateGraph: func(t *testing.T, graph *Graph) {
				resource := graph.Resources["pods"]
				require.NotNil(t, resource)
				assert.True(t, resource.Meta.Type == NodeTypeCollection)
				assert.Equal(t, []string{"each.status.phase == 'Running'"}, resource.ReadyWhen)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := generator.NewResourceGraphDefinition("testrgd", tt.resourceGraphDefinitionOpts...)
			graph, err := builder.NewResourceGraphDefinition(rgd)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errMsg != "" {
					assert.Contains(t, err.Error(), tt.errMsg)
				}
			} else {
				require.NoError(t, err)
				if tt.validateGraph != nil {
					tt.validateGraph(t, graph)
				}
			}
		})
	}
}

func TestGraphBuilder_CollectionChaining(t *testing.T) {
	fakeResolver, fakeDiscovery := k8s.NewFakeResolver()
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(memory2.NewMemCacheClient(fakeDiscovery))
	builder := &Builder{
		schemaResolver: fakeResolver,
		restMapper:     restMapper,
	}

	tests := []struct {
		name                        string
		resourceGraphDefinitionOpts []generator.ResourceGraphDefinitionOption
		wantErr                     bool
		errMsg                      string
		checkGraph                  func(t *testing.T, g *Graph)
	}{
		{
			name: "collection with forEach referencing another resource (dynamic forEach)",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"CollectionChaining", "v1alpha1",
					map[string]interface{}{
						"name":       "string",
						"cidrBlocks": "[]string",
					},
					nil,
				),
				// First resource: a regular VPC
				generator.WithResource("vpc", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "VPC",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-vpc",
					},
					"spec": map[string]interface{}{
						"cidrBlocks": []interface{}{"10.0.0.0/16"},
					},
				}, nil, nil),
				// Second resource: collection with forEach that references the first resource
				// The expression uses a ternary that checks vpc, making it a dynamic dependency
				generator.WithResourceCollection("chainedSubnets", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-${cidr}",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "${cidr}",
						"vpcID":     "${vpc.status.vpcID}",
					},
				},
					[]krov1alpha1.ForEachDimension{
						// forEach expression references vpc (another resource)
						{"cidr": "${has(vpc.status.vpcID) ? schema.spec.cidrBlocks : []}"},
					},
					nil, nil),
			},
			wantErr: false,
			checkGraph: func(t *testing.T, g *Graph) {
				// Verify the collection resource depends on vpc
				chainedResource := g.Resources["chainedSubnets"]
				assert.NotNil(t, chainedResource)
				assert.True(t, chainedResource.Meta.Type == NodeTypeCollection)
				assert.Contains(t, chainedResource.Meta.Dependencies, "vpc",
					"collection with forEach referencing vpc should have vpc as dependency")
			},
		},
		{
			name: "collection-to-collection chaining (forEach iterating over another collection)",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"CollectionToCollection", "v1alpha1",
					map[string]interface{}{
						"name":       "string",
						"cidrBlocks": "[]string",
					},
					nil,
				),
				// First collection: creates multiple subnets
				generator.WithResourceCollection("subnets", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-${cidr}",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "${cidr}",
						"vpcID":     "vpc-123",
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"cidr": "${schema.spec.cidrBlocks}"},
					},
					nil, nil),
				// Second collection: iterates over the first collection
				// ${subnets} is typed as list(Subnet) so we can iterate over it
				generator.WithResourceCollection("securityGroups", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "SecurityGroup",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-sg-${subnet.metadata.name}",
					},
					"spec": map[string]interface{}{
						"description": "${subnet.status.subnetID}",
						"vpcID":       "vpc-123",
					},
				},
					[]krov1alpha1.ForEachDimension{
						// forEach iterates over another collection - ${subnets} returns list(Subnet)
						{"subnet": "${subnets}"},
					},
					nil, nil),
			},
			wantErr: false,
			checkGraph: func(t *testing.T, g *Graph) {
				// Verify first collection exists
				subnetsResource := g.Resources["subnets"]
				assert.NotNil(t, subnetsResource)
				assert.True(t, subnetsResource.Meta.Type == NodeTypeCollection)

				// Verify second collection depends on first collection
				sgResource := g.Resources["securityGroups"]
				assert.NotNil(t, sgResource)
				assert.True(t, sgResource.Meta.Type == NodeTypeCollection)
				assert.Contains(t, sgResource.Meta.Dependencies, "subnets",
					"securityGroups should depend on subnets collection")

				// Verify topological order: subnets before securityGroups
				subnetsIdx := -1
				sgIdx := -1
				for i, id := range g.TopologicalOrder {
					if id == "subnets" {
						subnetsIdx = i
					}
					if id == "securityGroups" {
						sgIdx = i
					}
				}
				assert.True(t, subnetsIdx < sgIdx,
					"subnets should come before securityGroups in topological order")
			},
		},
		{
			name: "collection with filter on another collection",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"FilteredCollection", "v1alpha1",
					map[string]interface{}{
						"name":       "string",
						"cidrBlocks": "[]string",
					},
					nil,
				),
				// First collection: creates multiple subnets
				generator.WithResourceCollection("subnets", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-${cidr}",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "${cidr}",
						"vpcID":     "vpc-123",
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"cidr": "${schema.spec.cidrBlocks}"},
					},
					nil, nil),
				// Second collection: uses filter() on the first collection
				generator.WithResourceCollection("filteredSecurityGroups", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "SecurityGroup",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-sg-${subnet.metadata.name}",
					},
					"spec": map[string]interface{}{
						"description": "${subnet.status.subnetID}",
						"vpcID":       "vpc-123",
					},
				},
					[]krov1alpha1.ForEachDimension{
						// forEach uses filter() on collection - CEL list function on list(Subnet)
						{"subnet": "${subnets.filter(s, has(s.status.subnetID))}"},
					},
					nil, nil),
			},
			wantErr: false,
			checkGraph: func(t *testing.T, g *Graph) {
				// Verify second collection depends on first collection
				filteredResource := g.Resources["filteredSecurityGroups"]
				assert.NotNil(t, filteredResource)
				assert.True(t, filteredResource.Meta.Type == NodeTypeCollection)
				assert.Contains(t, filteredResource.Meta.Dependencies, "subnets",
					"filteredSecurityGroups should depend on subnets collection")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := generator.NewResourceGraphDefinition("test-rgd", tt.resourceGraphDefinitionOpts...)
			graph, err := builder.NewResourceGraphDefinition(rgd)

			if tt.wantErr {
				require.Error(t, err)
				if tt.errMsg != "" {
					assert.Contains(t, err.Error(), tt.errMsg)
				}
				return
			}

			require.NoError(t, err)
			assert.NotNil(t, graph)

			if tt.checkGraph != nil {
				tt.checkGraph(t, graph)
			}
		})
	}
}

func TestGraphBuilder_CollectionValidation(t *testing.T) {
	fakeResolver, fakeDiscovery := k8s.NewFakeResolver()
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(memory2.NewMemCacheClient(fakeDiscovery))
	builder := &Builder{
		schemaResolver: fakeResolver,
		restMapper:     restMapper,
	}

	tests := []struct {
		name                        string
		resourceGraphDefinitionOpts []generator.ResourceGraphDefinitionOption
		wantErr                     bool
		errMsg                      string
	}{
		{
			name: "valid collection with single iterator from schema list",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"MultiZoneVPC", "v1alpha1",
					map[string]interface{}{
						"name":       "string",
						"cidrBlocks": "[]string",
					},
					nil,
				),
				generator.WithResourceCollection("zonedSubnet", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-${cidr}",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "${cidr}",
						"vpcID":     "vpc-123",
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"cidr": "${schema.spec.cidrBlocks}"},
					},
					nil, nil),
			},
			wantErr: false,
		},
		{
			name: "valid collection with multiple iterators (cartesian product)",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"MultiRegionTierDeployment", "v1alpha1",
					map[string]interface{}{
						"name":       "string",
						"cidrBlocks": "[]string",
						"vpcIDs":     "[]string",
					},
					nil,
				),
				generator.WithResourceCollection("regionTierSubnet", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-${cidr}-${vpcID}",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "${cidr}",
						"vpcID":     "${vpcID}",
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"cidr": "${schema.spec.cidrBlocks}"},
						{"vpcID": "${schema.spec.vpcIDs}"},
					},
					nil, nil),
			},
			wantErr: false,
		},
		{
			name: "invalid collection - forEach expression does not return a list",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"InvalidCollection", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResourceCollection("badSubnet", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-${element}",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "${element}",
						"vpcID":     "vpc-123",
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"element": "${schema.spec.name}"}, // string, not a list
					},
					nil, nil),
			},
			wantErr: true,
			errMsg:  "must return a list",
		},
		{
			name: "collection with iterator variable used in template",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"WorkerPool", "v1alpha1",
					map[string]interface{}{
						"name":    "string",
						"workers": "[]string",
					},
					nil,
				),
				generator.WithResourceCollection("workerSubnet", map[string]interface{}{
					"apiVersion": "ec2.services.k8s.aws/v1alpha1",
					"kind":       "Subnet",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-${worker}",
					},
					"spec": map[string]interface{}{
						"cidrBlock": "${worker}",
						"vpcID":     "${schema.spec.name}",
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"worker": "${schema.spec.workers}"},
					},
					nil, nil),
			},
			wantErr: false,
		},
		{
			name: "invalid collection - forEach iterator references another iterator",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"InvalidIteratorRef", "v1alpha1",
					map[string]interface{}{
						"name":  "string",
						"items": "[]string",
					},
					nil,
				),
				generator.WithResourceCollection("badPod", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-${element}-${derived}",
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
					[]krov1alpha1.ForEachDimension{
						{"element": "${schema.spec.items}"},
						// This references the 'element' iterator - not allowed
						{"derived": "${element}"},
					},
					nil, nil),
			},
			wantErr: true,
			errMsg:  "cannot reference other iterators",
		},
		{
			name: "invalid collection - forEach dimension not used in identity",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"UnusedIterator", "v1alpha1",
					map[string]interface{}{
						"regions": "[]string",
						"tiers":   "[]string",
					},
					nil,
				),
				generator.WithResourceCollection("pods", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						// Only uses 'region', not 'tier' - should fail
						"name": "${region}-pod",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "app",
								"image": "nginx:latest",
							},
						},
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"region": "${schema.spec.regions}"},
						{"tier": "${schema.spec.tiers}"},
					},
					nil, nil),
			},
			wantErr: true,
			errMsg:  "all forEach dimensions must be used to produce a unique resource identity, missing: [tier]",
		},
		{
			name: "valid collection - all iterators used in name and namespace",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"MultiDimension", "v1alpha1",
					map[string]interface{}{
						"namespaces": "[]string",
						"names":      "[]string",
					},
					nil,
				),
				generator.WithResourceCollection("pods", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Pod",
					"metadata": map[string]interface{}{
						"name":      "${name}",
						"namespace": "${ns}",
					},
					"spec": map[string]interface{}{
						"containers": []interface{}{
							map[string]interface{}{
								"name":  "app",
								"image": "nginx:latest",
							},
						},
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"ns": "${schema.spec.namespaces}"},
						{"name": "${schema.spec.names}"},
					},
					nil, nil),
			},
			wantErr: false,
		},
		{
			name: "invalid collection - cluster-scoped resource with iterator only in namespace",
			resourceGraphDefinitionOpts: []generator.ResourceGraphDefinitionOption{
				generator.WithSchema(
					"ClusterScoped", "v1alpha1",
					map[string]interface{}{
						"names": "[]string",
					},
					nil,
				),
				// CRD is cluster-scoped, so namespace field doesnt count for identity
				generator.WithResourceCollection("crds", map[string]interface{}{
					"apiVersion": "apiextensions.k8s.io/v1",
					"kind":       "CustomResourceDefinition",
					"metadata": map[string]interface{}{
						"name": "static-name",
						// Iterator in namespace field doesn't count for cluster-scoped resources
						"namespace": "${name}",
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"name": "${schema.spec.names}"},
					},
					nil, nil),
			},
			wantErr: true,
			errMsg:  "cluster-scoped and must not set metadata.namespace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := generator.NewResourceGraphDefinition("test-rgd", tt.resourceGraphDefinitionOpts...)
			graph, err := builder.NewResourceGraphDefinition(rgd)

			if tt.wantErr {
				require.Error(t, err)
				if tt.errMsg != "" {
					assert.Contains(t, err.Error(), tt.errMsg)
				}
				return
			}

			require.NoError(t, err)
			assert.NotNil(t, graph)
		})
	}
}
