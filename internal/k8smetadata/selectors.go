// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package k8smetadata

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Sometimes we need to search for resources that belong to given instance, resource group or node.
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

func NewResourceGroupSelector(resourceGroup metav1.Object) metav1.LabelSelector {
	return metav1.LabelSelector{
		MatchLabels: map[string]string{
			ResourceGroupIDLabel: string(resourceGroup.GetUID()),
			// ResourceGroupNameLabel:      resourceGroup.GetName(),
			// ResourceGroupNamespaceLabel: resourceGroup.GetNamespace(),
		},
	}
}

func NewInstanceAndResourceGroupSelector(instance metav1.Object, resourceGroup metav1.Object) metav1.LabelSelector {
	return metav1.LabelSelector{
		MatchLabels: map[string]string{
			InstanceIDLabel:      string(instance.GetUID()),
			ResourceGroupIDLabel: string(resourceGroup.GetUID()),
		},
	}
}

func NewNodeAndInstanceAndResourceGroupSelector(node metav1.Object, instance metav1.Object, resourceGroup metav1.Object) metav1.LabelSelector {
	return metav1.LabelSelector{
		MatchLabels: map[string]string{
			NodeIDLabel:          node.GetName(),
			InstanceIDLabel:      string(instance.GetUID()),
			ResourceGroupIDLabel: string(resourceGroup.GetUID()),
		},
	}
}
