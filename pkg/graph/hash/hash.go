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

package hash

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"slices"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

var jsonMarshal = json.Marshal

// Spec computes a deterministic FNV-128a hash for an RGD spec.
//
// FNV is chosen over a cryptographic hash because this is a content-identity
// fingerprint, not a security boundary. FNV-128a is fast, well-distributed,
// and sufficient for collision resistance at the scale of revision lineages.
//
// The hash is based on a canonicalized form of the spec:
// - Struct field order is stable via JSON marshaling (sorted map keys).
// - RawExtension payloads are normalized into canonical JSON bytes.
func Spec(spec v1alpha1.ResourceGraphDefinitionSpec) (string, error) {
	normalized, err := normalizeSpec(spec)
	if err != nil {
		return "", err
	}

	data, err := jsonMarshal(normalized)
	if err != nil {
		return "", fmt.Errorf("marshal normalized spec: %w", err)
	}

	h := fnv.New128a()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func normalizeSpec(spec v1alpha1.ResourceGraphDefinitionSpec) (v1alpha1.ResourceGraphDefinitionSpec, error) {
	normalized := spec.DeepCopy()

	if normalized.Schema != nil {
		var err error
		normalized.Schema.Spec, err = normalizeRawExtension(normalized.Schema.Spec)
		if err != nil {
			return v1alpha1.ResourceGraphDefinitionSpec{}, fmt.Errorf("normalize schema.spec: %w", err)
		}
		normalized.Schema.Types, err = normalizeRawExtension(normalized.Schema.Types)
		if err != nil {
			return v1alpha1.ResourceGraphDefinitionSpec{}, fmt.Errorf("normalize schema.types: %w", err)
		}
		normalized.Schema.Status, err = normalizeRawExtension(normalized.Schema.Status)
		if err != nil {
			return v1alpha1.ResourceGraphDefinitionSpec{}, fmt.Errorf("normalize schema.status: %w", err)
		}
	}

	slices.SortFunc(normalized.Resources, func(a, b *v1alpha1.Resource) int {
		if a == nil && b == nil {
			return 0
		}
		if a == nil {
			return -1
		}
		if b == nil {
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})

	for i := range normalized.Resources {
		if normalized.Resources[i] == nil {
			continue
		}
		normalizedTemplate, err := normalizeRawExtension(normalized.Resources[i].Template)
		if err != nil {
			return v1alpha1.ResourceGraphDefinitionSpec{}, fmt.Errorf("normalize resources[%d].template: %w", i, err)
		}
		normalized.Resources[i].Template = normalizedTemplate
		slices.Sort(normalized.Resources[i].ReadyWhen)
		slices.Sort(normalized.Resources[i].IncludeWhen)
		slices.SortFunc(normalized.Resources[i].ForEach, func(a, b v1alpha1.ForEachDimension) int {
			for k := range a {
				for k2 := range b {
					if k < k2 {
						return -1
					}
					if k > k2 {
						return 1
					}
					return 0
				}
			}
			return 0
		})
	}

	return *normalized, nil
}

func normalizeRawExtension(ext runtime.RawExtension) (runtime.RawExtension, error) {
	source := ext.Raw
	if len(bytes.TrimSpace(source)) == 0 && ext.Object != nil {
		var err error
		source, err = jsonMarshal(ext.Object)
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

	canonicalJSON, err := jsonMarshal(canonical)
	if err != nil {
		return runtime.RawExtension{}, fmt.Errorf("marshal canonical raw extension payload: %w", err)
	}

	return runtime.RawExtension{Raw: canonicalJSON}, nil
}
