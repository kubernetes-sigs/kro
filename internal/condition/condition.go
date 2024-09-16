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

package condition

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/aws-controllers-k8s/symphony/api/v1alpha1"
)

// NewCondition returns a new Condition instance.
func NewCondition(t v1alpha1.ConditionType, status corev1.ConditionStatus, reason, message string) v1alpha1.Condition {
	return v1alpha1.Condition{
		Type:               t,
		Status:             status,
		LastTransitionTime: &metav1.Time{Time: metav1.Now().Time},
		Reason:             &reason,
		Message:            &message,
	}
}

func GetCondition(conditions []v1alpha1.Condition, t v1alpha1.ConditionType) *v1alpha1.Condition {
	for _, c := range conditions {
		if c.Type == t {
			return &c
		}
	}
	return nil
}

func SetCondition(conditions []v1alpha1.Condition, condition v1alpha1.Condition) []v1alpha1.Condition {
	for i, c := range conditions {
		if c.Type == condition.Type {
			conditions[i] = condition
			return conditions
		}
	}
	return append(conditions, condition)
}

func HasCondition(conditions []v1alpha1.Condition, t v1alpha1.ConditionType) bool {
	return GetCondition(conditions, t) != nil
}
