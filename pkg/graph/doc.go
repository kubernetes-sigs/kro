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

// Package graph builds an executable resource graph from a
// v1alpha1.ResourceGraphDefinition.
//
// The GraphCompiler runs a fixed multi-stage pipeline:
//
//	Parse -> Validate -> Resolve -> Link -> TypeCheck -> Compile -> Assemble
//
// Each stage produces a more explicit representation:
//
//   - Parse: v1alpha1.ResourceGraphDefinition -> ParsedRGD
//   - Resolve: ParsedRGD -> ResolvedRGD
//   - Link: ResolvedRGD -> LinkedRGD
//   - TypeCheck: LinkedRGD -> TypeCheckResult (analysis sidecar)
//   - Compile: LinkedRGD + TypeCheckResult -> CompiledRGD
//   - Assemble: CompiledRGD -> Graph (final output)
//
// Stage contract:
//   - Stage inputs are treated as immutable.
//   - Each stage returns a distinct output type consumed by the next stage.
//
// Phase responsibilities:
//
//   - Parse:
//     Decodes RawExtension payloads, extracts CEL strings schemalessly, parses
//     conditions and forEach, and creates normalized node/instance metadata.
//
//   - Validate:
//     Enforces static invariants (naming, reserved words, template/externalRef
//     rules, iterator constraints, object structure, CRD expression constraints).
//
//   - Resolve:
//     Resolves node identity against the cluster (GVK/GVR/scope/OpenAPI schema).
//     It also performs scope checks and validates resource templates against the
//     resolved OpenAPI schema. This is the only stage that depends on API server
//     discovery/schema state.
//
//   - Link:
//     Inspects CEL references, validates expression scope, classifies field kinds
//     (static/dynamic/iteration), and builds the dependency DAG + topo order.
//
//   - TypeCheck:
//     Type-checks expressions in typed CEL environments, validates expected field
//     types, derives the instance CEL "schema" view (excluding status), infers
//     iterator element types, and infers instance status CEL types.
//
//   - Compile:
//     Converts type-checked CEL ASTs into reusable cel.Program values.
//
//   - Assemble:
//     Converts status CEL types to OpenAPI, synthesizes the CRD, creates the
//     instance node, and emits the final Graph.
//
// Error model:
//
//   - TerminalError: user/actionable spec issue (do not retry until spec changes).
//   - RetriableError: transient external issue (safe to retry, e.g. discovery/schema
//     resolution timing).
//
// Helpers IsTerminal and IsRetriable can be used by callers/controllers to decide
// reconciliation behavior.
package graph
