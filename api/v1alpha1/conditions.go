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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionType is a type of condition for a resource.
type ConditionType string

const (
	// ResourceGroupConditionTypeGraphSynced indicates the state of the directed
	// acyclic graph (DAG) that kro uses to manage the resources in a
	// ResourceGroup.
	ResourceGroupConditionTypeGraphVerified ConditionType = "GraphVerified"
	// ResourceGroupConditionTypeCustomResourceDefinitionSynced indicates the state of the
	// CustomResourceDefinition (CRD) that kro uses to manage the resources in a
	// ResourceGroup.
	ResourceGroupConditionTypeCustomResourceDefinitionSynced ConditionType = "CustomResourceDefinitionSynced"
	// ResourceGroupConditionTypeReconcilerReady indicates the state of the reconciler.
	// Whenever an ResourceGroup resource is created, kro will spin up a
	// reconciler for that resource. This condition indicates the state of the
	// reconciler.
	ResourceGroupConditionTypeReconcilerReady ConditionType = "ReconcilerReady"
)

const (
	InstanceConditionTypeReady ConditionType = "Ready"

	// Creating Deleting Migrating
	InstanceConditionTypeProgressing ConditionType = "Progressing"

	// Unexpected situation, behaviour, need human intervention
	InstanceConditionTypeDegraded ConditionType = "Degraded"

	// Something is wrong but i'm gonna try again
	InstanceConditionTypeError ConditionType = "Error"
)

// Condition is the common struct used by all CRDs managed by ACK service
// controllers to indicate terminal states  of the CR and its backend AWS
// service API resource
type Condition struct {
	// Type is the type of the Condition
	Type ConditionType `json:"type"`
	// Status of the condition, one of True, False, Unknown.
	Status metav1.ConditionStatus `json:"status"`
	// Last time the condition transitioned from one status to another.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	// The reason for the condition's last transition.
	// +optional
	Reason *string `json:"reason,omitempty"`
	// A human readable message indicating details about the transition.
	// +optional
	Message *string `json:"message,omitempty"`
	// observedGeneration represents the .metadata.generation that the condition was set based upon.
	// For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date
	// with respect to the current state of the instance.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// NewCondition returns a new Condition instance.
func NewCondition(t ConditionType, status metav1.ConditionStatus, reason, message string) Condition {
	return Condition{
		Type:               t,
		Status:             status,
		LastTransitionTime: &metav1.Time{Time: metav1.Now().Time},
		Reason:             &reason,
		Message:            &message,
	}
}

func GetCondition(conditions []Condition, t ConditionType) *Condition {
	for _, c := range conditions {
		if c.Type == t {
			return &c
		}
	}
	return nil
}

func SetCondition(conditions []Condition, condition Condition) []Condition {
	for i, c := range conditions {
		if c.Type == condition.Type {
			conditions[i] = condition
			return conditions
		}
	}
	return append(conditions, condition)
}

func HasCondition(conditions []Condition, t ConditionType) bool {
	return GetCondition(conditions, t) != nil
}
