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

// Package registry caches compiled Programs by Graph identity. Entries are
// invalidated when the Graph's normalized spec hash changes.
package registry

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"slices"

	"k8s.io/apimachinery/pkg/runtime"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// marshalFunc is the JSON marshaling surface used during hashing. Production
// callers always use encoding/json.Marshal; tests inject a stub at call time
// to cover error paths without touching package-level state.
type marshalFunc func(any) ([]byte, error)

// HashSpec computes a deterministic FNV-64a hash for a Graph spec.
//
// This is an in-memory dedup mechanism only — not persisted on a CRD field,
// not part of any API contract. The normalization rules can evolve freely;
// any change causes one extra compile per Graph on the first reconcile after
// upgrade, then the cache stabilizes.
//
// Normalization:
//   - Nodes are sorted by ID; user-authored YAML ordering doesn't affect the hash.
//   - nil/missing collections collapse to empty so absent ↔ explicit-empty hash equally.
//   - Template / Def RawExtension payloads are roundtripped through a
//     canonical JSON form so equivalent objects with different key ordering
//     hash the same.
//   - ForEach order is preserved (cartesian-product order is semantic).
func HashSpec(spec expv1alpha1.GraphSpec) (string, error) {
	return hashSpecWith(spec, json.Marshal)
}

func hashSpecWith(spec expv1alpha1.GraphSpec, marshal marshalFunc) (string, error) {
	normalized, err := normalizeSpec(spec, marshal)
	if err != nil {
		return "", err
	}
	data, err := marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("marshal normalized spec: %w", err)
	}
	h := fnv.New64a()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func normalizeSpec(spec expv1alpha1.GraphSpec, marshal marshalFunc) (expv1alpha1.GraphSpec, error) {
	normalized := *spec.DeepCopy()
	if normalized.Nodes == nil {
		normalized.Nodes = []expv1alpha1.Node{}
	}

	slices.SortFunc(normalized.Nodes, func(a, b expv1alpha1.Node) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})

	for i := range normalized.Nodes {
		n := &normalized.Nodes[i]
		for _, slot := range []**runtime.RawExtension{&n.Template, &n.Def} {
			if *slot == nil {
				continue
			}
			canon, err := normalizeRawExtension(**slot, marshal)
			if err != nil {
				return expv1alpha1.GraphSpec{}, fmt.Errorf("node[%d]: %w", i, err)
			}
			*slot = &canon
		}
		if n.ForEach == nil {
			n.ForEach = []expv1alpha1.ForEachDimension{}
		}
	}
	return normalized, nil
}

func normalizeRawExtension(ext runtime.RawExtension, marshal marshalFunc) (runtime.RawExtension, error) {
	source := ext.Raw
	if len(bytes.TrimSpace(source)) == 0 && ext.Object != nil {
		var err error
		source, err = marshal(ext.Object)
		if err != nil {
			return runtime.RawExtension{}, fmt.Errorf("marshal raw extension object: %w", err)
		}
	}
	if len(bytes.TrimSpace(source)) == 0 {
		return runtime.RawExtension{}, nil
	}
	var canonical any
	if err := json.Unmarshal(source, &canonical); err != nil {
		return runtime.RawExtension{}, fmt.Errorf("parse raw extension payload: %w", err)
	}
	canonicalJSON, err := marshal(canonical)
	if err != nil {
		return runtime.RawExtension{}, fmt.Errorf("marshal canonical raw extension payload: %w", err)
	}
	return runtime.RawExtension{Raw: canonicalJSON}, nil
}
