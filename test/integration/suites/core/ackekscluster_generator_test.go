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
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

func eksCluster(
	namespace, name string,
) (
	*krov1alpha1.ResourceGraphDefinition,
	func(namespace, name, version string) *unstructured.Unstructured,
) {
	resourcegraphdefinition := generator.NewResourceGraphDefinition(name,
		generator.WithSchema(
			"EKSCluster", "v1alpha1",
			map[string]any{
				"name":    "string",
				"version": "string",
			},
			map[string]any{
				"networkingInfo": map[string]any{
					"vpcID":     "${clusterVPC.status.vpcID}",
					"subnetAZA": "${clusterSubnetA.status.subnetID}",
					"subnetAZB": "${clusterSubnetB.status.subnetID}",
				},
				"clusterARN": "${cluster.status.ackResourceMetadata.arn}",
			},
		),
		generator.WithResource("clusterRole", clusterRoleDef(namespace), nil, nil),
		generator.WithResource("clusterVPC", eksVPCDef(namespace), nil, nil),
		generator.WithResource("clusterInternetGateway", igwDef(namespace), nil, nil),
		generator.WithResource("clusterRouteTable", routeTableDef(namespace), nil, nil),
		generator.WithResource(
			"clusterSubnetA",
			eksSubnetDef(namespace, "kro-cluster-public-subnet1", "us-west-2a", "192.168.0.0/18"), nil, nil,
		),
		generator.WithResource(
			"clusterSubnetB",
			eksSubnetDef(namespace, "kro-cluster-public-subnet2", "us-west-2b", "192.168.64.0/18"), nil, nil,
		),
		generator.WithResource("cluster", clusterDef(namespace), nil, nil),
		generator.WithResource("clusterAdminRole", adminRoleDef(namespace), nil, nil),
		generator.WithResource("clusterElasticIPAddress", eipDef(namespace), nil, nil),
		generator.WithResource("clusterNATGateway", natGatewayDef(namespace), nil, nil),
		generator.WithResource("clusterNodeRole", nodeRoleDef(namespace), nil, nil),
		generator.WithResource("clusterNodeGroup", nodeGroupDef(namespace), nil, nil),
	)

	instanceGenerator := func(namespace, name, version string) *unstructured.Unstructured {
		return &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "EKSCluster",
				"metadata": map[string]any{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]any{
					"name":    name,
					"version": version,
				},
			},
		}
	}
	return resourcegraphdefinition, instanceGenerator
}

func eksVPCDef(namespace string) map[string]any {
	return map[string]any{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "VPC",
		"metadata": map[string]any{
			"name":      "kro-cluster-vpc",
			"namespace": namespace,
		},
		"spec": map[string]any{
			"cidrBlocks": []any{
				"192.168.0.0/16",
			},
			"enableDNSSupport":   true,
			"enableDNSHostnames": true,
		},
	}
}

func eipDef(namespace string) map[string]any {
	return map[string]any{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "ElasticIPAddress",
		"metadata": map[string]any{
			"name":      "kro-cluster-eip",
			"namespace": namespace,
		},
		"spec": map[string]any{},
	}
}

func igwDef(namespace string) map[string]any {
	return map[string]any{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "InternetGateway",
		"metadata": map[string]any{
			"name":      "kro-cluster-igw",
			"namespace": namespace,
		},
		"spec": map[string]any{
			"vpc": "${clusterVPC.status.vpcID}",
		},
	}
}

func routeTableDef(namespace string) map[string]any {
	return map[string]any{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "RouteTable",
		"metadata": map[string]any{
			"name":      "kro-cluster-public-route-table",
			"namespace": namespace,
		},
		"spec": map[string]any{
			"vpcID": "${clusterVPC.status.vpcID}",
			"routes": []any{
				map[string]any{
					"destinationCIDRBlock": "0.0.0.0/0",
					"gatewayID":            "${clusterInternetGateway.status.internetGatewayID}",
				},
			},
		},
	}
}

func eksSubnetDef(namespace, name, az, cidr string) map[string]any {
	return map[string]any{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "Subnet",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{
			"availabilityZone":    az,
			"cidrBlock":           cidr,
			"vpcID":               "${clusterVPC.status.vpcID}",
			"routeTables":         []any{"${clusterRouteTable.status.routeTableID}"},
			"mapPublicIPOnLaunch": true,
		},
	}
}

func natGatewayDef(namespace string) map[string]any {
	return map[string]any{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "NATGateway",
		"metadata": map[string]any{
			"name":      "kro-cluster-natgateway1",
			"namespace": namespace,
		},
		"spec": map[string]any{
			"subnetID":     "${clusterSubnetB.status.subnetID}",
			"allocationID": "${clusterElasticIPAddress.status.allocationID}",
		},
	}
}

func clusterRoleDef(namespace string) map[string]any {
	return map[string]any{
		"apiVersion": "iam.services.k8s.aws/v1alpha1",
		"kind":       "Role",
		"metadata": map[string]any{
			"name":      "kro-cluster-role",
			"namespace": namespace,
		},
		"spec": map[string]any{
			"name":        "kro-cluster-role",
			"description": "kro created cluster cluster role",
			"policies": []any{
				"arn:aws:iam::aws:policy/AmazonEKSClusterPolicy",
			},
			"assumeRolePolicyDocument": `{
				"Version": "2012-10-17",
				"Statement": [
					{
						"Effect": "Allow",
						"Principal": {
							"Service": "eks.amazonaws.com"
						},
						"Action": "sts:AssumeRole"
					}
				]
			}`,
		},
	}
}

func nodeRoleDef(namespace string) map[string]any {
	return map[string]any{
		"apiVersion": "iam.services.k8s.aws/v1alpha1",
		"kind":       "Role",
		"metadata": map[string]any{
			"name":      "kro-cluster-node-role",
			"namespace": namespace,
		},
		"spec": map[string]any{
			"name":        "kro-cluster-node-role",
			"description": "kro created cluster node role",
			"policies": []any{
				"arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
				"arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly",
				"arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
			},
			"assumeRolePolicyDocument": `{
				"Version": "2012-10-17",
				"Statement": [
					{
						"Effect": "Allow",
						"Principal": {
							"Service": "ec2.amazonaws.com"
						},
						"Action": "sts:AssumeRole"
					}
				]
			}`,
		},
	}
}

func adminRoleDef(namespace string) map[string]any {
	return map[string]any{
		"apiVersion": "iam.services.k8s.aws/v1alpha1",
		"kind":       "Role",
		"metadata": map[string]any{
			"name":      "kro-cluster-pia-role",
			"namespace": namespace,
		},
		"spec": map[string]any{
			"name":        "kro-cluster-pia-role",
			"description": "kro created cluster admin pia role",
			"policies": []any{
				"arn:aws:iam::aws:policy/AdministratorAccess",
			},
			"assumeRolePolicyDocument": `{
				"Version": "2012-10-17",
				"Statement": [
					{
						"Sid": "AllowEksAuthToAssumeRoleForPodIdentity",
						"Effect": "Allow",
						"Principal": {
							"Service": "pods.eks.amazonaws.com"
						},
						"Action": [
							"sts:AssumeRole",
							"sts:TagSession"
						]
					}
				]
			}`,
		},
	}
}

func clusterDef(namespace string) map[string]any {
	return map[string]any{
		"apiVersion": "eks.services.k8s.aws/v1alpha1",
		"kind":       "Cluster",
		"metadata": map[string]any{
			"name":      "${schema.spec.name}",
			"namespace": namespace,
		},
		"spec": map[string]any{
			"name": "${schema.spec.name}",
			"accessConfig": map[string]any{
				"authenticationMode": "API_AND_CONFIG_MAP",
			},
			"roleARN": "${clusterRole.status.ackResourceMetadata.arn}",
			"version": "${schema.spec.version}",
			"resourcesVPCConfig": map[string]any{
				"endpointPrivateAccess": false,
				"endpointPublicAccess":  true,
				"subnetIDs": []any{
					"${clusterSubnetA.status.subnetID}",
					"${clusterSubnetB.status.subnetID}",
				},
			},
		},
	}
}

func nodeGroupDef(namespace string) map[string]any {
	return map[string]any{
		"apiVersion": "eks.services.k8s.aws/v1alpha1",
		"kind":       "Nodegroup",
		"metadata": map[string]any{
			"name":      "kro-cluster-nodegroup",
			"namespace": namespace,
		},
		"spec": map[string]any{
			"name":        "kro-cluster-ng",
			"diskSize":    100,
			"clusterName": "${cluster.spec.name}",
			"subnets": []any{
				"${clusterSubnetA.status.subnetID}",
				"${clusterSubnetB.status.subnetID}",
			},
			"nodeRole": "${clusterNodeRole.status.ackResourceMetadata.arn}",
			"updateConfig": map[string]any{
				"maxUnavailable": 1,
			},
			"scalingConfig": map[string]any{
				"minSize":     1,
				"maxSize":     1,
				"desiredSize": 1,
			},
		},
	}
}
