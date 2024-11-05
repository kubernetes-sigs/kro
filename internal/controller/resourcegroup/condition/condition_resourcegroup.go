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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/aws-controllers-k8s/symphony/api/v1alpha1"
)

func NewReconcilerReadyCondition(status metav1.ConditionStatus, reason, message string) v1alpha1.Condition {
	return NewCondition(v1alpha1.ResourceGroupConditionTypeReconcilerReady, status, reason, message)
}

func NewGraphVerifiedCondition(status metav1.ConditionStatus, reason, message string) v1alpha1.Condition {
	return NewCondition(v1alpha1.ResourceGroupConditionTypeGraphVerified, status, reason, message)
}

func NewCustomResourceDefinitionSyncedCondition(status metav1.ConditionStatus, reason, message string) v1alpha1.Condition {
	return NewCondition(v1alpha1.ResourceGroupConditionTypeCustomResourceDefinitionSynced, status, reason, message)
}
