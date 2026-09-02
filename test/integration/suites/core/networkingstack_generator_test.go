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

func networkingStack(
	name string,
) (
	*krov1alpha1.ResourceGraphDefinition,
	func(namespace, name string) *unstructured.Unstructured,
) {
	resourcegraphdefinition := generator.NewResourceGraphDefinition(name,
		generator.WithSchema(
			"NetworkingStack", "v1alpha1",
			map[string]any{
				"name": "string",
			},
			map[string]any{
				"networkingInfo": map[string]any{
					"vpcID":         "${vpc.status.vpcID}",
					"subnetAZA":     "${subnetAZA.status.subnetID}",
					"subnetAZB":     "${subnetAZB.status.subnetID}",
					"subnetAZC":     "${subnetAZC.status.subnetID}",
					"securityGroup": "${securityGroup.status.id}",
				},
			},
		),
		generator.WithResource("vpc", nsVPCDef(), nil, nil),
		generator.WithResource("securityGroup", securityGroupDef(), nil, nil),
		generator.WithResource("subnetAZA", nsSubnetDef("a", "us-west-2a", "192.168.0.0/18"), nil, nil),
		generator.WithResource("subnetAZB", nsSubnetDef("b", "us-west-2b", "192.168.64.0/18"), nil, nil),
		generator.WithResource("subnetAZC", nsSubnetDef("c", "us-west-2c", "192.168.128.0/18"), nil, nil),
	)

	instanceGenerator := func(namespace, name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "NetworkingStack",
				"metadata": map[string]any{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]any{
					"name": name,
				},
			},
		}
	}
	return resourcegraphdefinition, instanceGenerator
}

func nsVPCDef() map[string]any {
	return map[string]any{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "VPC",
		"metadata": map[string]any{
			"name": "vpc-${schema.spec.name}",
		},
		"spec": map[string]any{
			"cidrBlocks": []any{
				"192.168.0.0/16",
			},
			"enableDNSHostnames": false,
			"enableDNSSupport":   true,
		},
	}
}

func nsSubnetDef(suffix, az, cidr string) map[string]any {
	return map[string]any{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "Subnet",
		"metadata": map[string]any{
			"name": "subnet-" + suffix + "-${schema.spec.name}",
		},
		"spec": map[string]any{
			"availabilityZone": az,
			"cidrBlock":        cidr,
			"vpcID":            "${vpc.status.vpcID}",
		},
	}
}

func securityGroupDef() map[string]any {
	return map[string]any{
		"apiVersion": "ec2.services.k8s.aws/v1alpha1",
		"kind":       "SecurityGroup",
		"metadata": map[string]any{
			"name": "security-group-${schema.spec.name}",
		},
		"spec": map[string]any{
			"vpcID":       "${vpc.status.vpcID}",
			"name":        "my-sg-${schema.spec.name}",
			"description": "something something",
		},
	}
}
