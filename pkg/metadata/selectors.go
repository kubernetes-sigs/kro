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

package metadata

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Sometimes we need to search for resources that belong to given instance, resource graph definition or node.
// This is helpful to for garbage collection of resources that are no longer needed, or got
// orphaned due to graph evolutions.

func NewInstanceSelector(instance metav1.Object) metav1.LabelSelector {
	return metav1.LabelSelector{
		MatchLabels: map[string]string{
			InstanceIDLabel: string(instance.GetUID()),
			// InstanceNameLabel:      instance.GetName(),
			// InstanceNamespaceLabel: instance.GetNamespace(),
		},
	}
}

func NewResourceGraphDefinitionSelector(resourceGraphDefinition metav1.Object) metav1.LabelSelector {
	return metav1.LabelSelector{
		MatchLabels: map[string]string{
			ResourceGraphDefinitionIDLabel: string(resourceGraphDefinition.GetUID()),
			// ResourceGraphDefinitionNameLabel: resourceGraphDefinition.GetName(),
			// ResourceGraphDefinitionNamespaceLabel: resourceGraphDefinition.GetNamespace(),
		},
	}
}

func NewInstanceAndResourceGraphDefinitionSelector(instance metav1.Object, resourceGraphDefinition metav1.Object) metav1.LabelSelector {
	return metav1.LabelSelector{
		MatchLabels: map[string]string{
			InstanceIDLabel:                string(instance.GetUID()),
			ResourceGraphDefinitionIDLabel: string(resourceGraphDefinition.GetUID()),
		},
	}
}

func NewNodeAndInstanceAndResourceGraphDefinitionSelector(node metav1.Object, instance metav1.Object, resourceGraphDefinition metav1.Object) metav1.LabelSelector {
	return metav1.LabelSelector{
		MatchLabels: map[string]string{
			NodeIDLabel:                    node.GetName(),
			InstanceIDLabel:                string(instance.GetUID()),
			ResourceGraphDefinitionIDLabel: string(resourceGraphDefinition.GetUID()),
		},
	}
}
