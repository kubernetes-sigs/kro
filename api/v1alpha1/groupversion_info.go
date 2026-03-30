// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package v1alpha1 contains API Schema definitions for the x v1alpha1 API group
// +kubebuilder:object:generate=true
// +groupName=kro.run
package v1alpha1

import (
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

const (
	KRODomainName = "kro.run"
)

// Annotations for ResourceGraphDefinitions
const (
	// AllowBreakingChangesAnnotation allows RGD updates that would otherwise be
	// blocked due to breaking schema changes. Use with caution - breaking changes
	// can invalidate existing instances.
	AllowBreakingChangesAnnotation = KRODomainName + "/allow-breaking-changes"

	// InstanceReconcileAnnotation controls instance reconciliation. When set to
	// "suspended" (case-insensitive), the instance controller skips resource
	// reconciliation and marks the instance as suspended.
	// "disabled" is accepted as a legacy alias for backward compatibility.
	InstanceReconcileAnnotation = KRODomainName + "/reconcile"

	// ReconcileSuspended is the canonical annotation value to suspend reconciliation.
	ReconcileSuspended = "suspended"
	// ReconcileLegacyDisabled is the legacy annotation value for suspending reconciliation.
	// Kept for backward compatibility; prefer ReconcileSuspended.
	ReconcileLegacyDisabled = "disabled"
)

// IsReconcileSuspended reports whether the given annotation value represents
// a suspended reconciliation state. It accepts "suspended" (canonical) and
// "disabled" (legacy) in a case-insensitive manner.
func IsReconcileSuspended(value string) bool {
	return strings.EqualFold(value, ReconcileSuspended) || strings.EqualFold(value, ReconcileLegacyDisabled)
}

var (
	// GroupVersion is group version used to register these objects
	GroupVersion = schema.GroupVersion{Group: KRODomainName, Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
