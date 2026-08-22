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

package graph

import (
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// Source is the schema-agnostic input to Builder.CompileSource. Any graph
// consumer projects its own API shape into a Source so the compile pipeline
// never sees a concrete API type.
//
// SchemaVarSchema is the value bound to the `schema` CEL variable: the instance
// spec plus ObjectMeta, with status excluded. Callers that synthesize a CRD
// derive it from that CRD; others build it directly.
type Source interface {
	// Resources are the graph's resource nodes in source order.
	Resources() []ResourceSpec
	// InstanceGVR is the GroupVersionResource of the owning instance.
	InstanceGVR() k8sschema.GroupVersionResource
	// InstanceNamespaced reports whether the instance is namespace-scoped.
	InstanceNamespaced() bool
	// SchemaVarSchema is the schema bound to the `schema` CEL variable.
	SchemaVarSchema() *spec.Schema
	// StatusRaw is the raw status block (SimpleSchema + CEL) to infer from.
	StatusRaw() []byte
}
