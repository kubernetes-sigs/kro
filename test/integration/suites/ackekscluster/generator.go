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
package ackekscluster_test

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	krov1alpha1 "github.com/awslabs/kro/api/v1alpha1"
	"github.com/awslabs/kro/internal/testutil/generator"
)

func eksCluster(
	namespace, name string,
) (
	*krov1alpha1.ResourceGroup,
	func(namespace, name, version string) *unstructured.Unstructured,
) {
	resourcegroup := generator.NewResourceGroup(name,
		generator.WithNamespace(namespace),
		generator.WithSchema(
			"EKSCluster", "v1alpha1",
			map[string]interface{}{
				"name":    "string",
				"version": "string",
			},
			map[string]interface{}{
				"networkingInfo": map[string]interface{}{
					"vpcID":     "${clusterVPC.status.vpcID}",
					"subnetAZA": "${clusterSubnetA.status.subnetID}",
					"subnetAZB": "${clusterSubnetB.status.subnetID}",
				},
				"clusterARN": "${cluster.status.ackResourceMetadata.arn}",
			},
		),
		generator.WithResource("clusterVPC", vpcDef(namespace), nil, nil),
		generator.WithResource("clusterElasticIPAddress", eipDef(namespace), nil, nil),
		generator.WithResource("clusterInternetGateway", igwDef(namespace), nil, nil),
		generator.WithResource("clusterRouteTable", routeTableDef(namespace), nil, nil),
		generator.WithResource(
			"clusterSubnetA",
			subnetDef(namespace, "kro-cluster-public-subnet1", "us-west-2a", "192.168.0.0/18"), nil, nil,
		),
		generator.WithResource(
			"clusterSubnetB",
			subnetDef(namespace, "kro-cluster-public-subnet2", "us-west-2b", "192.168.64.0/18"), nil, nil,
		),
		generator.WithResource("clusterNATGateway", natGatewayDef(namespace), nil, nil),
		generator.WithResource("clusterRole", clusterRoleDef(namespace), nil, nil),
		generator.WithResource("clusterNodeRole", nodeRoleDef(namespace), nil, nil),
		generator.WithResource("clusterAdminRole", adminRoleDef(namespace), nil, nil),
		generator.WithResource("cluster", clusterDef(namespace), nil, nil),
		generator.WithResource("clusterNodeGroup", nodeGroupDef(namespace), nil, nil),
	)

	instanceGenerator := func(namespace, name, version string) *unstructured.Unstructured {
		return &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KroDomainName, "v1alpha1"),
				"kind":       "EKSCluster",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":    name,
					"version": version,
				},
			},
		}
	}
	return resourcegroup, instanceGenerator
}

func vpcDef(namespace string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "VPC",
		"metadata": map[string]interface{}{
			"name":      "kro-cluster-vpc",
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"cidrBlocks": []interface{}{
				"192.168.0.0/16",
			},
			"enableDNSSupport":   true,
			"enableDNSHostnames": true,
		},
	}
}

func eipDef(namespace string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "ElasticIPAddress",
		"metadata": map[string]interface{}{
			"name":      "kro-cluster-eip",
			"namespace": namespace,
		},
		"spec": map[string]interface{}{},
	}
}

func igwDef(namespace string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "InternetGateway",
		"metadata": map[string]interface{}{
			"name":      "kro-cluster-igw",
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"vpc": "${clusterVPC.status.vpcID}",
		},
	}
}

func routeTableDef(namespace string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "RouteTable",
		"metadata": map[string]interface{}{
			"name":      "kro-cluster-public-route-table",
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"vpcID": "${clusterVPC.status.vpcID}",
			"routes": []interface{}{
				map[string]interface{}{
					"destinationCIDRBlock": "0.0.0.0/0",
					"gatewayID":            "${clusterInternetGateway.status.internetGatewayID}",
				},
			},
		},
	}
}

func subnetDef(namespace, name, az, cidr string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "Subnet",
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"availabilityZone":    az,
			"cidrBlock":           cidr,
			"vpcID":               "${clusterVPC.status.vpcID}",
			"routeTables":         []interface{}{"${clusterRouteTable.status.routeTableID}"},
			"mapPublicIPOnLaunch": true,
		},
	}
}

func natGatewayDef(namespace string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "NATGateway",
		"metadata": map[string]interface{}{
			"name":      "kro-cluster-natgateway1",
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"subnetID":     "${clusterSubnetB.status.subnetID}",
			"allocationID": "${clusterElasticIPAddress.status.allocationID}",
		},
	}
}

func clusterRoleDef(namespace string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "iam.services.k8s.aws/v1alpha1",
		"kind":       "Role",
		"metadata": map[string]interface{}{
			"name":      "kro-cluster-role",
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"name":        "kro-cluster-role",
			"description": "KRO created cluster cluster role",
			"policies": []interface{}{
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

func nodeRoleDef(namespace string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "iam.services.k8s.aws/v1alpha1",
		"kind":       "Role",
		"metadata": map[string]interface{}{
			"name":      "kro-cluster-node-role",
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"name":        "kro-cluster-node-role",
			"description": "KRO created cluster node role",
			"policies": []interface{}{
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

func adminRoleDef(namespace string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "iam.services.k8s.aws/v1alpha1",
		"kind":       "Role",
		"metadata": map[string]interface{}{
			"name":      "kro-cluster-pia-role",
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"name":        "kro-cluster-pia-role",
			"description": "KRO created cluster admin pia role",
			"policies": []interface{}{
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

func clusterDef(namespace string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "eks.services.k8s.aws/v1alpha1",
		"kind":       "Cluster",
		"metadata": map[string]interface{}{
			"name":      "${spec.name}",
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"name": "${spec.name}",
			"accessConfig": map[string]interface{}{
				"authenticationMode": "API_AND_CONFIG_MAP",
			},
			"roleARN": "${clusterRole.status.ackResourceMetadata.arn}",
			"version": "${spec.version}",
			"resourcesVPCConfig": map[string]interface{}{
				"endpointPrivateAccess": false,
				"endpointPublicAccess":  true,
				"subnetIDs": []interface{}{
					"${clusterSubnetA.status.subnetID}",
					"${clusterSubnetB.status.subnetID}",
				},
			},
		},
	}
}

func nodeGroupDef(namespace string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "eks.services.k8s.aws/v1alpha1",
		"kind":       "Nodegroup",
		"metadata": map[string]interface{}{
			"name":      "kro-cluster-nodegroup",
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"name":        "kro-cluster-ng",
			"diskSize":    100,
			"clusterName": "${cluster.spec.name}",
			"subnets": []interface{}{
				"${clusterSubnetA.status.subnetID}",
				"${clusterSubnetB.status.subnetID}",
			},
			"nodeRole": "${clusterNodeRole.status.ackResourceMetadata.arn}",
			"updateConfig": map[string]interface{}{
				"maxUnavailable": 1,
			},
			"scalingConfig": map[string]interface{}{
				"minSize":     1,
				"maxSize":     1,
				"desiredSize": 1,
			},
		},
	}
}
