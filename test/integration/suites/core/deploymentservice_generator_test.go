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

// deploymentService creates a ResourceGraphDefinition for testing deployment+service combinations
func deploymentService(
	name string,
) (
	*krov1alpha1.ResourceGraphDefinition,
	func(namespace, name string, port int) *unstructured.Unstructured,
) {
	resourcegraphdefinition := generator.NewResourceGraphDefinition(name,
		generator.WithSchema(
			"DeploymentService", "v1alpha1",
			map[string]any{
				"name":     "string",
				"port":     "integer | default=80",
				"replicas": "integer | default=1",
			},
			map[string]any{
				"deploymentConditions": "${deployment.status.conditions}",
				"availableReplicas":    "${deployment.status.availableReplicas}",

				// These fields are not strictly needed for our deployment but are used for optionals
				// TODO(jakobmoellerdev): Decide if we want a completely separate integration test for this.
				"available": "${deployment.status.?conditions[0].status}",
				// unavailable is a placeholder for a condition that is never present
				"unavailable": "${deployment.status.?conditions[10]}",
			},
		),
		generator.WithResource("deployment", deploymentDef(), nil, nil),
		generator.WithResource("service", serviceDef(), nil, nil),
	)
	instanceGenerator := func(namespace, name string, port int) *unstructured.Unstructured {
		return &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "DeploymentService",
				"metadata": map[string]any{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]any{
					"name": name,
					"port": port,
				},
			},
		}
	}
	return resourcegraphdefinition, instanceGenerator
}

func deploymentDef() map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name": "${schema.spec.name}",
		},
		"spec": map[string]any{
			"replicas": "${schema.spec.replicas}",
			"selector": map[string]any{
				"matchLabels": map[string]any{
					"app": "deployment",
				},
			},
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{
						"app": "deployment",
					},
				},
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"name":  "${schema.spec.name}-deployment",
							"image": "nginx",
							"ports": []any{
								map[string]any{
									"containerPort": "${schema.spec.port}",
								},
							},
						},
					},
				},
			},
		},
	}
}

func serviceDef() map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]any{
			"name": "${schema.spec.name}",
		},
		"spec": map[string]any{
			"selector": map[string]any{
				"app": "deployment",
			},
			"ports": []any{
				map[string]any{
					"port":       "${schema.spec.port}",
					"targetPort": "${schema.spec.port}",
				},
			},
		},
	}
}
