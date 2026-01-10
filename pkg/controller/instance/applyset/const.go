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

package applyset

import (
	"fmt"

	"sigs.k8s.io/release-utils/version"
)

// Internal constants for ApplySet implementation.
const (
	// FieldManager is the field manager name used for server-side apply.
	FieldManager = "kro.run/applyset"
)

// ToolingID returns the tooling identifier in the format "kro/<version>".
func ToolingID() string {
	return fmt.Sprintf("kro/%s", version.GetVersionInfo().GitVersion)
}

// Label and annotation keys from the ApplySet specification.
// https://git.k8s.io/enhancements/keps/sig-cli/3659-kubectl-apply-prune#design-details-applyset-specification
const (
	// ApplySetToolingAnnotation is the key of the label that indicates which tool is used to manage this ApplySet.
	// Tooling should refuse to mutate ApplySets belonging to other tools.
	// The value must be in the format <toolname>/<semver>.
	// Example value: "kubectl/v1.27" or "helm/v3" or "kro/v1.0.0"
	ApplySetToolingAnnotation = "applyset.kubernetes.io/tooling"

	// ApplySetAdditionalNamespacesAnnotation lists namespaces beyond the parent's own namespace.
	// The parent namespace is implicitly included and must NOT be listed here.
	// Value: comma-separated namespace names, or empty if all resources are in parent namespace.
	// Example: "kube-system,ns1,ns2" (parent namespace is NOT listed).
	ApplySetAdditionalNamespacesAnnotation = "applyset.kubernetes.io/additional-namespaces"

	// ApplySetGKsAnnotation is the standard KEP annotation for group-kinds.
	// Format: comma-separated "Kind.group" entries (Kind only for core resources).
	// Example value: "ConfigMap,Deployment.apps,Service"
	ApplySetGKsAnnotation = "applyset.kubernetes.io/contains-group-kinds"

	// ApplySetParentIDLabel is the key of the label that makes object an ApplySet parent object.
	// Its value MUST use the format specified in V1ApplySetIdFormat below.
	ApplySetParentIDLabel = "applyset.kubernetes.io/id"

	// V1ApplySetIdFormat is the format required for the value of ApplySetParentIDLabel (and ApplysetPartOfLabel).
	// The %s segment is the unique ID of the object itself, which MUST be the base64 encoding
	// (using the URL safe encoding of RFC4648) of the hash of the GKNN of the object it is on, in the form:
	// base64(sha256(<name>.<namespace>.<kind>.<group>)).
	V1ApplySetIdFormat = "applyset-%s-v1"

	// ApplySetIDPartDelimiter is the delimiter used to separate the parts of the ApplySet ID.
	ApplySetIDPartDelimiter = "."

	// ApplysetPartOfLabel is the key of the label which indicates that the object is a member of an ApplySet.
	// The value of the label MUST match the value of ApplySetParentIDLabel on the parent object.
	ApplysetPartOfLabel = "applyset.kubernetes.io/part-of"
)
