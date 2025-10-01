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

/*
Package applyset provides a library for managing sets of Kubernetes objects.
The core of this package is the Set interface, which defines the operations
for an apply set. An apply set is a collection of Kubernetes objects that are
managed together as a group. This is useful for managing the resources of a
composite object, like a CRD that creates other resources.

This is implemented as per the KEP:
https://github.com/kubernetes/enhancements/blob/master/keps/sig-cli/3659-kubectl-apply-prune/README.md

Existing implementations of the KEP:

 1. kubectl implements the KEP in its codebase:
    https://github.com/kubernetes/kubectl/blob/master/pkg/cmd/apply/applyset.go
    https://github.com/kubernetes/kubectl/blob/master/pkg/cmd/apply/applyset_pruner.go
    cli focussed, pulls in a lot of dependencies not required for a controller.

 2. kubernetes-declarative-pattern (kdp)
    https://pkg.go.dev/sigs.k8s.io/kubebuilder-declarative-pattern/applylib/applyset
    controller focussed, missing callbacks, lifecycle support etc.

Why Fork:
* The kubectl implementation is built for cli and has many dependencies that we dont need in a controller.
* The kdp implementation is good for controller but lacks some enhancements that are required for it to work with KRO.
* We need the ability to do partial applies of an applyset (without pruning) that is not supported by KDP library.
* Also we need the ability to read-in the result of an apply for use by KRO engine. This requires some callback support.
* We ended up adding call-backs (to populate KRO engine)
* Also we enhanced applylib to support ExternalRef (read from cluster, no changes) - KRO specific for now.
* Planning to add LifeCycle modifiers to support additional behaviours like abandon on delete, no update to a resource after creation etc.

Upstreaming:
The goal is to upstream the changes back to either kubernetes-declarative-pattern or controller-runtime.
We may need to do the enhancements and decide if all of it or some of it can be upstreamed.
We will need to do a few iterations in the KRO repo first before we get an idea of where and how to upstream.
*/
package applyset
