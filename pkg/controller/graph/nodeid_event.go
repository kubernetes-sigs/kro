// Copyright 2026 The Kubernetes Authors.
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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/validate/content"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"

	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// warnOnEncodedNodeIDs emits one event per template node whose qualified path
// is too long for kro.run/node-id label. Called every reconcile, not just on a
// fresh compile, so the event does not age out of the apiserver's event-ttl
// while the label is still hashed.
func (r *Reconciler) warnOnEncodedNodeIDs(g *expv1alpha1.Graph, prog *compiler.Program) {
	if r.Recorder == nil {
		return
	}
	for _, path := range encodedNodePaths(prog, "") {
		r.Recorder.Eventf(g, corev1.EventTypeWarning, "NodeIDEncoded",
			"node %q exceeds the %d character label value limit once qualified; %s is set to %q "+
				"on its managed resources, and selectors must use that value; the full path is "+
				"preserved in the %s annotation",
			path, content.LabelValueMaxLength, metadata.NodeIDLabel,
			metadata.NodeIDToken(path), metadata.NodePathAnnotation)
	}
}

// encodedNodePaths walks one compiled frame and returns the qualified paths
// whose node-id token has to be hashed, recursing into subgraphs with the
// frame prefix the executor's applySubgraph uses.
func encodedNodePaths(prog *compiler.Program, prefix string) []string {
	if prog == nil {
		return nil
	}
	var out []string
	for _, id := range prog.TopologicalOrder {
		n := prog.Nodes[id]
		if n == nil {
			continue
		}
		qualified := prefix + id
		if n.Kind == compiler.NodeKindGraph {
			out = append(out, encodedNodePaths(n.SubProgram, qualified+"/")...)
			continue
		}
		if n.Kind != compiler.NodeKindTemplate {
			continue
		}
		if metadata.NodeIDTokenIsHashed(qualified) {
			out = append(out, qualified)
		}
	}
	return out
}
