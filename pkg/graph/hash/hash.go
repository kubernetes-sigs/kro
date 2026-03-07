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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// Spec computes a deterministic SHA-256 hash for an RGD spec.
//
// The hash is based on a canonicalized form of the spec:
// - Struct field order is stable via JSON marshaling.
// - Map key order is canonicalized.
// - RawExtension payloads are normalized into canonical JSON bytes.
func Spec(spec v1alpha1.ResourceGraphDefinitionSpec) (string, error) {
	normalized, err := normalizeSpec(spec)
	if err != nil {
		return "", err
	}

	data, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("marshal normalized spec: %w", err)
	}

	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func normalizeSpec(spec v1alpha1.ResourceGraphDefinitionSpec) (v1alpha1.ResourceGraphDefinitionSpec, error) {
	normalized := spec.DeepCopy()
	if normalized == nil {
		return v1alpha1.ResourceGraphDefinitionSpec{}, nil
	}

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

	for i := range normalized.Resources {
		if normalized.Resources[i] == nil {
			continue
		}
		normalizedTemplate, err := normalizeRawExtension(normalized.Resources[i].Template)
		if err != nil {
			return v1alpha1.ResourceGraphDefinitionSpec{}, fmt.Errorf("normalize resources[%d].template: %w", i, err)
		}
		normalized.Resources[i].Template = normalizedTemplate
	}

	return *normalized, nil
}

func normalizeRawExtension(ext runtime.RawExtension) (runtime.RawExtension, error) {
	source := ext.Raw
	if len(bytes.TrimSpace(source)) == 0 && ext.Object != nil {
		var err error
		source, err = json.Marshal(ext.Object)
		if err != nil {
			return runtime.RawExtension{}, fmt.Errorf("marshal raw extension object: %w", err)
		}
	}

	if len(bytes.TrimSpace(source)) == 0 {
		return runtime.RawExtension{}, nil
	}

	var canonical any
	if err := yaml.Unmarshal(source, &canonical); err != nil {
		return runtime.RawExtension{}, fmt.Errorf("parse raw extension payload: %w", err)
	}

	canonicalJSON, err := json.Marshal(canonical)
	if err != nil {
		return runtime.RawExtension{}, fmt.Errorf("marshal canonical raw extension payload: %w", err)
	}

	return runtime.RawExtension{Raw: canonicalJSON}, nil
}
