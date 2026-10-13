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

package sentinels

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// StableStampAnnotationPrefix prefixes the per-field annotation that records
// the key tuple observed when a stableString field was last stamped.
const StableStampAnnotationPrefix = "stable.kro.run/"

// StableStamp is the marker value time.now().stableString(trigger) leaves in
// a rendered object. The executor replaces it with an RFC3339 string once it
// has the live object in hand (see ResolveStableStamps).
//
// Keys is the rendered key tuple from whenChanged(...); nil for once().
// Gate is the AND of every onlyWhen(...) clause (true when none).
// Now is the reconcile's fixed clock.
type StableStamp struct {
	Keys []any
	Gate bool
	Now  time.Time
}

// IsStableStamp reports whether v is a StableStamp sentinel.
func IsStableStamp(v any) bool {
	_, ok := v.(StableStamp)
	return ok
}

// keyFingerprint canonically serializes the key tuple.
func (s StableStamp) keyFingerprint() string {
	b, err := json.Marshal(s.Keys)
	if err != nil {
		return fmt.Sprintf("%v", s.Keys)
	}
	return string(b)
}

// StableStampAnnotationKey derives the annotation key for a field path.
// Annotation names are limited to 63 chars, so the path is hashed.
func StableStampAnnotationKey(fieldPath string) string {
	sum := sha256.Sum256([]byte(fieldPath))
	return StableStampAnnotationPrefix + hex.EncodeToString(sum[:8])
}

// ResolveStableStamps walks desired, replacing every StableStamp with its
// resolved RFC3339 value. live is the pre-apply cluster object (nil when
// absent). For each stamp at field path P with annotation key K:
//
//   - If live has P and K, and (Gate is false OR Keys == live[K]): keep
//     live's value at P and copy K forward (frozen / unchanged).
//   - Else if Gate is false: field is omitted and K is not written (never
//     stamp while gated, not even on create).
//   - Else: write Now as RFC3339 at P and write K = Keys (new stamp).
//
// The annotation and the field always move together in the same object.
func ResolveStableStamps(desired, live map[string]any) {
	resolveStableStamps(desired, live, desired, nil)
}

func resolveStableStamps(node any, live, root map[string]any, path []string) {
	switch val := node.(type) {
	case map[string]any:
		for k, child := range val {
			p := append(path, k)
			if s, ok := child.(StableStamp); ok {
				resolved, keep := resolveOne(s, strings.Join(p, "."), live, root)
				if keep {
					val[k] = resolved
				} else {
					delete(val, k)
				}
				continue
			}
			resolveStableStamps(child, live, root, p)
		}
	case []any:
		for i, child := range val {
			p := append(path, fmt.Sprintf("[%d]", i))
			if s, ok := child.(StableStamp); ok {
				resolved, keep := resolveOne(s, strings.Join(p, "."), live, root)
				if keep {
					val[i] = resolved
				} else {
					val[i] = nil
				}
				continue
			}
			resolveStableStamps(child, live, root, p)
		}
	}
}

func resolveOne(s StableStamp, fieldPath string, live, root map[string]any) (string, bool) {
	annoKey := StableStampAnnotationKey(fieldPath)
	fp := s.keyFingerprint()

	prevVal, prevOK := lookupPath(live, fieldPath)
	prevFP, prevAnnoOK := liveAnnotation(live, annoKey)

	// Baseline exists: freeze unless gated-open AND keys changed.
	if prevOK && prevAnnoOK {
		if !s.Gate || prevFP == fp {
			setAnnotation(root, annoKey, prevFP)
			return prevVal, true
		}
	}
	// once(): an existing value with no annotation is respected as the baseline.
	if prevOK && s.Keys == nil {
		setAnnotation(root, annoKey, fp)
		return prevVal, true
	}
	if !s.Gate {
		return "", false
	}
	setAnnotation(root, annoKey, fp)
	return s.Now.UTC().Format(time.RFC3339), true
}

func lookupPath(obj map[string]any, fieldPath string) (string, bool) {
	if obj == nil {
		return "", false
	}
	var cur any = obj
	for _, seg := range strings.Split(fieldPath, ".") {
		if strings.HasPrefix(seg, "[") {
			var idx int
			if _, err := fmt.Sscanf(seg, "[%d]", &idx); err != nil {
				return "", false
			}
			arr, ok := cur.([]any)
			if !ok || idx < 0 || idx >= len(arr) {
				return "", false
			}
			cur = arr[idx]
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[seg]
		if !ok {
			return "", false
		}
	}
	str, ok := cur.(string)
	return str, ok
}

func liveAnnotation(live map[string]any, key string) (string, bool) {
	if live == nil {
		return "", false
	}
	meta, _ := live["metadata"].(map[string]any)
	annos, _ := meta["annotations"].(map[string]any)
	v, ok := annos[key].(string)
	return v, ok
}

func setAnnotation(root map[string]any, key, value string) {
	meta, _ := root["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		root["metadata"] = meta
	}
	annos, _ := meta["annotations"].(map[string]any)
	if annos == nil {
		annos = map[string]any{}
		meta["annotations"] = annos
	}
	annos[key] = value
}

// StableStampAnnotationKeys returns the sorted stable-stamp annotation keys
// present on obj. Useful for tests and debugging.
func StableStampAnnotationKeys(obj map[string]any) []string {
	meta, _ := obj["metadata"].(map[string]any)
	annos, _ := meta["annotations"].(map[string]any)
	var out []string
	for k := range annos {
		if strings.HasPrefix(k, StableStampAnnotationPrefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
