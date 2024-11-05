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
	v1alpha1 "github.com/aws-controllers-k8s/symphony/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func NewInstanceConditionReady(status metav1.ConditionStatus, reason, message string) v1alpha1.Condition {
	return NewCondition(v1alpha1.InstanceConditionTypeReady, status, reason, message)
}

func NewInstanceConditionProgressing(status metav1.ConditionStatus, reason, message string) v1alpha1.Condition {
	return NewCondition(v1alpha1.InstanceConditionTypeProgressing, status, reason, message)
}

func NewInstanceConditionDegraded(status metav1.ConditionStatus, reason, message string) v1alpha1.Condition {
	return NewCondition(v1alpha1.InstanceConditionTypeDegraded, status, reason, message)
}

func NewInstanceConditionErrored(status metav1.ConditionStatus, reason, message string) v1alpha1.Condition {
	return NewCondition(v1alpha1.InstanceConditionTypeError, status, reason, message)
}
