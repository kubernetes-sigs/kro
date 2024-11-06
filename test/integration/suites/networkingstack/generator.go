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
package networkingstack_test

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	symphonyv1alpha1 "github.com/awslabs/symphony/api/v1alpha1"
	"github.com/awslabs/symphony/internal/testutil/generator"
)

func networkingStack(
	name, namespace string,
) (
	*symphonyv1alpha1.ResourceGroup,
	func(namespace, name string) *unstructured.Unstructured,
) {
	resourcegroup := generator.NewResourceGroup(name,
		generator.WithNamespace(namespace),
		generator.WithKind("NetworkingStack", "v1alpha1"),
		generator.WithDefinition(
			map[string]interface{}{
				"name": "string",
			},
			map[string]interface{}{
				"networkingInfo": map[string]interface{}{
					"vpcID":         "${vpc.status.vpcID}",
					"subnetAZA":     "${subnetAZA.status.subnetID}",
					"subnetAZB":     "${subnetAZB.status.subnetID}",
					"subnetAZC":     "${subnetAZC.status.subnetID}",
					"securityGroup": "${securityGroup.status.id}",
				},
			},
		),
		generator.WithResource("vpc", vpcDef(), nil, nil),
		generator.WithResource("subnetAZA", subnetDef("a", "us-west-2a", "192.168.0.0/18"), nil, nil),
		generator.WithResource("subnetAZB", subnetDef("b", "us-west-2b", "192.168.64.0/18"), nil, nil),
		generator.WithResource("subnetAZC", subnetDef("c", "us-west-2c", "192.168.128.0/18"), nil, nil),
		generator.WithResource("securityGroup", securityGroupDef(), nil, nil),
	)

	instanceGenerator := func(namespace, name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("x.%s/%s", symphonyv1alpha1.SymphonyDomainName, "v1alpha1"),
				"kind":       "NetworkingStack",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": name,
				},
			},
		}
	}
	return resourcegroup, instanceGenerator
}

func vpcDef() map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "VPC",
		"metadata": map[string]interface{}{
			"name": "vpc-${spec.name}",
		},
		"spec": map[string]interface{}{
			"cidrBlocks": []interface{}{
				"192.168.0.0/16",
			},
			"enableDNSHostnames": false,
			"enableDNSSupport":   true,
		},
	}
}

func subnetDef(suffix, az, cidr string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "Subnet",
		"metadata": map[string]interface{}{
			"name": "subnet-" + suffix + "-${spec.name}",
		},
		"spec": map[string]interface{}{
			"availabilityZone": az,
			"cidrBlock":        cidr,
			"vpcID":            "${vpc.status.vpcID}",
		},
	}
}

func securityGroupDef() map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "SecurityGroup",
		"metadata": map[string]interface{}{
			"name": "security-group-${spec.name}",
		},
		"spec": map[string]interface{}{
			"vpcID":       "${vpc.status.vpcID}",
			"name":        "my-sg-${spec.name}",
			"description": "something something",
		},
	}
}
