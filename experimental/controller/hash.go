// hash.go implements content-addressed hashing for SSA apply gating.
//
// hashDesiredState computes a content hash of an evaluated template map. Used
// to gate SSA Patch — if the desired state matches the annotation, skip.
//
// All hashing uses FNV-64a over a zero-alloc canonical byte representation.
// Map keys are sorted at every level for determinism. A pooled scratch buffer
// eliminates per-call allocations.
package graphcontroller

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"sync"
)

const applyHashAnnotation = "internal.kro.run/template-hash"

// hashState pools the scratch buffer used by hashDesiredState.
// One allocation per goroutine on first use; reused thereafter. The pool
// prevents cross-goroutine contention (controller workers run in parallel)
// while eliminating per-call allocations for the buffer.
var hashStatePool = sync.Pool{
	New: func() any {
		return &hashState{
			buf: make([]byte, 0, 1024),
		}
	},
}

type hashState struct {
	buf []byte // accumulates the canonical representation, then hashed once
}

// hashDesiredState computes a content hash of an evaluated template map.
// Walks the map tree directly, accumulating a canonical byte representation
// in a pooled buffer, then hashes the buffer with FNV-64a. This avoids the
// reflect allocations that encoding/json.Marshal produces (~24 allocs per
// small map) while producing a deterministic hash.
//
// Determinism: map keys are sorted at every level. Values use a canonical
// representation (strings quoted, numbers as decimal, bools as true/false,
// nil as null). The hash is stable within the same Go runtime — it is
// persisted in annotations (applyHashAnnotation) but both sides are
// computed by this function, so the first reconcile after an algorithm
// change re-applies once (SSA idempotent, no harm).
func hashDesiredState(evalMap map[string]any) (string, error) {
	hs := hashStatePool.Get().(*hashState)
	hs.buf = hs.buf[:0]
	appendMap(hs, evalMap)
	h := fnv.New64a()
	h.Write(hs.buf)
	result := fmt.Sprintf("%016x", h.Sum64())
	hashStatePool.Put(hs)
	return result, nil
}

// appendMap writes a sorted, deterministic map representation to the buffer.
func appendMap(hs *hashState, m map[string]any) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	hs.buf = append(hs.buf, '{')
	for i, k := range keys {
		if i > 0 {
			hs.buf = append(hs.buf, ',')
		}
		appendQuotedString(hs, k)
		hs.buf = append(hs.buf, ':')
		appendValue(hs, m[k])
	}
	hs.buf = append(hs.buf, '}')
}

// appendQuotedString writes a quoted, escaped string to the buffer.
// Escapes " and \ to prevent structural ambiguity — a string like
// `a","b":"c` must not produce the same bytes as a key-value boundary.
// Full JSON escaping is unnecessary — only characters that break the
// framing need escaping.
func appendQuotedString(hs *hashState, s string) {
	hs.buf = append(hs.buf, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\\' {
			hs.buf = append(hs.buf, '\\')
		}
		hs.buf = append(hs.buf, c)
	}
	hs.buf = append(hs.buf, '"')
}

// appendValue writes a deterministic value representation to the buffer.
func appendValue(hs *hashState, v any) {
	switch val := v.(type) {
	case nil:
		hs.buf = append(hs.buf, "null"...)
	case bool:
		if val {
			hs.buf = append(hs.buf, "true"...)
		} else {
			hs.buf = append(hs.buf, "false"...)
		}
	case string:
		appendQuotedString(hs, val)
	case int64:
		hs.buf = strconv.AppendInt(hs.buf, val, 10)
	case float64:
		hs.buf = strconv.AppendFloat(hs.buf, val, 'f', -1, 64)
	case int:
		hs.buf = strconv.AppendInt(hs.buf, int64(val), 10)
	case map[string]any:
		appendMap(hs, val)
	case []any:
		hs.buf = append(hs.buf, '[')
		for i, item := range val {
			if i > 0 {
				hs.buf = append(hs.buf, ',')
			}
			appendValue(hs, item)
		}
		hs.buf = append(hs.buf, ']')
	default:
		// Unrecognized type — should not fire for normalized k8s objects.
		// Append a type-tagged sentinel so distinct unexpected types don't
		// collide with each other or with handled types.
		hs.buf = append(hs.buf, "<unknown:"...)
		hs.buf = fmt.Appendf(hs.buf, "%T:%v", val, val)
		hs.buf = append(hs.buf, '>')
	}
}

