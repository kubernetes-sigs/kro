// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package v1alpha1

import (
	"github.com/kro-run/kro/pkg/apis"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionType is a type of condition for a resource.
type ConditionType string

const (
	// ResourceGraphDefinitionConditionTypeGraphVerified indicates the state of the directed
	// acyclic graph (DAG) that kro uses to manage the resources in a
	// ResourceGraphDefinition.
	ResourceGraphDefinitionConditionTypeGraphVerified ConditionType = "GraphVerified"
	// ResourceGraphDefinitionConditionTypeCustomResourceDefinitionSynced indicates the state of the
	// CustomResourceDefinition (CRD) that kro uses to manage the resources in a
	// ResourceGraphDefinition.
	ResourceGraphDefinitionConditionTypeCustomResourceDefinitionSynced ConditionType = "CustomResourceDefinitionSynced"
	// ResourceGraphDefinitionConditionTypeReconcilerReady indicates the state of the reconciler.
	// Whenever an ResourceGraphDefinition resource is created, kro will spin up a
	// reconciler for that resource. This condition indicates the state of the
	// reconciler.
	ResourceGraphDefinitionConditionTypeReconcilerReady ConditionType = "ReconcilerReady"
)

const (
	InstanceConditionTypeReady ConditionType = "Ready"

	// InstanceConditionTypeProgressing used for Creating Deleting Migrating
	InstanceConditionTypeProgressing ConditionType = "Progressing"

	// InstanceConditionTypeDegraded used in unexpected situation, behaviour, need human intervention
	InstanceConditionTypeDegraded ConditionType = "Degraded"

	// InstanceConditionTypeError used in something is wrong but i'm going to try again
	InstanceConditionTypeError ConditionType = "Error"
)

// Condition aliases the apis.Condition type.
type Condition apis.Condition

// NewCondition returns a new Condition instance.
func NewCondition(t ConditionType, og int64, status metav1.ConditionStatus, reason, message string) Condition {
	return Condition{
		Type:               string(t),
		ObservedGeneration: og,
		Status:             status,
		LastTransitionTime: metav1.Time{Time: metav1.Now().Time},
		Reason:             reason,
		Message:            message,
	}
}
