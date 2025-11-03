// Copyright 2023 The Kubernetes Authors.
// Copyright 2025 The Kube Resource Orchestrator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package applyset

// Label and annotation keys from the ApplySet specification.
// https://git.k8s.io/enhancements/keps/sig-cli/3659-kubectl-apply-prune#design-details-applyset-specification
const (
	// ApplySetToolingAnnotation is the key of the label that indicates which tool is used to manage this ApplySet.
	// Tooling should refuse to mutate ApplySets belonging to other tools.
	// The value must be in the format <toolname>/<semver>.
	// Example value: "kubectl/v1.27" or "helm/v3" or "kpt/v1.0.0"
	ApplySetToolingAnnotation = "applyset.kubernetes.io/tooling"

	// ApplySetAdditionalNamespacesAnnotation annotation extends the scope of the ApplySet beyond the parent
	// object's own namespace (if any) to include the listed namespaces. The value is a comma-separated
	// list of the names of namespaces other than the parent's namespace in which objects are found
	// Example value: "kube-system,ns1,ns2".
	ApplySetAdditionalNamespacesAnnotation = "applyset.kubernetes.io/additional-namespaces"

	// ApplySetGKsAnnotation is a list of group-kinds used to optimize listing of ApplySet member objects.
	// It is optional in the ApplySet specification, as tools can perform discovery or use a different optimization.
	// However, it is currently required in kubectl.
	// When present, the value of this annotation must be a comma separated list of the group-kinds,
	// in the fully-qualified name format, i.e. <kind>.<group>.
	// Example value: "Certificate.cert-manager.io,ConfigMap,deployments.apps,Secret,Service"
	ApplySetGKsAnnotation = "applyset.kubernetes.io/contains-group-kinds"

	// ApplySetParentIDLabel is the key of the label that makes object an ApplySet parent object.
	// Its value MUST use the format specified in V1ApplySetIdFormat below
	ApplySetParentIDLabel = "applyset.kubernetes.io/id"

	// V1ApplySetIdFormat is the format required for the value of ApplySetParentIDLabel (and ApplysetPartOfLabel).
	// The %s segment is the unique ID of the object itself, which MUST be the base64 encoding
	// (using the URL safe encoding of RFC4648) of the hash of the GKNN of the object it is on, in the form:
	// base64(sha256(<name>.<namespace>.<kind>.<group>)).
	V1ApplySetIdFormat = "applyset-%s-v1"

	// ApplysetPartOfLabel is the key of the label which indicates that the object is a member of an ApplySet.
	// The value of the label MUST match the value of ApplySetParentIDLabel on the parent object.
	ApplysetPartOfLabel = "applyset.kubernetes.io/part-of"

	// ApplysetParentCRDLabel is the key of the label that can be set on a CRD to identify
	// the custom resource type it defines (not the CRD itself) as an allowed parent for an ApplySet.
	ApplysetParentCRDLabel = "applyset.kubernetes.io/is-parent-type"

	// applySetIDPartDelimiter is the delimiter used to separate the parts of the ApplySet ID.
	ApplySetIDPartDelimiter = "."
)
