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

package metadata

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kro-run/kro/api/v1alpha1"
)

var (
	KRORGOwnerReferenceKind       = "ResourceGraphDefinition"
	KRORGOwnerReferenceAPIVersion = v1alpha1.GroupVersion.String()
)

// stamped on the CRD and RGIs
func NewResourceGraphDefinitionOwnerReference(name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{
		Name:       name,
		Kind:       KRORGOwnerReferenceKind,
		APIVersion: KRORGOwnerReferenceAPIVersion,
		Controller: &[]bool{false}[0],
		UID:        uid,
	}
}

// stamped on the RGI child resources
func NewInstanceOwnerReference(gvk schema.GroupVersionKind, name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{
		Name:       name,
		Kind:       gvk.Kind,
		APIVersion: gvk.GroupVersion().String(),
		Controller: &[]bool{true}[0],
		UID:        uid,
	}
}
