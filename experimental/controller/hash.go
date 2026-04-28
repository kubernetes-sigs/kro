// hash.go implements content-addressed hashing for apply gating and
// evaluation skip detection.
//
// Three hash layers (per 005-reconciliation.md § Hash Mechanics):
//
//   - hashDesiredState: content hash of an evaluated template map. Used to
//     gate SSA Patch — if the desired state matches the annotation, skip.
//   - hashNodeInputs: field-path-scoped hash of a node's dependency inputs.
//     Used to skip template evaluation when inputs haven't changed.
//   - hashSelfPaths: field-path-scoped hash of a node's own observed resource.
//     Used for output propagation change detection.
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

	"github.com/kubernetes-sigs/kro/experimental/controller/graph"
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

// volatileMetadataFields are metadata keys excluded from field-path hashing.
// These change on every write without conveying semantic meaning — including them
// would defeat the evaluation hash (same problem as full-object hashing).
var volatileMetadataFields = map[string]bool{
	"resourceVersion":            true,
	"managedFields":              true,
	"creationTimestamp":          true,
	"uid":                        true,
	"selfLink":                   true,
	"generation":                 true,
	"deletionGracePeriodSeconds": true,
}

// hashNodeInputs computes a field-path-scoped hash of a node's dependency inputs.
// Only the specific field paths that the node's CEL expressions reference are
// included (from node.DepPaths). For metadata paths, volatile fields are excluded.
//
// Field paths are extracted at compile time (see 004-compilation.md § Algorithm,
// Phase 4 step 5). Hashing uses them at reconcile time per
// 005-reconciliation.md § Hash Mechanics.
//
// Returns "" if the node has no dependency paths to hash.
func hashNodeInputs(node *graph.Node, scope map[string]any) (string, error) {
	if len(node.DepPaths) == 0 {
		return "", nil
	}

	hs := hashStatePool.Get().(*hashState)
	hs.buf = hs.buf[:0]

	// Process dependencies in sorted order for deterministic hashing.
	// Lazy dep paths are excluded — their availability in the coordinator's
	// scope is timing-dependent (they may or may not have been processed
	// in this walk). Changes to lazy deps propagate through the output-hash
	// chain: the dep's self-paths include the referenced fields, so when
	// the dep's data changes, its output-hash changes, triggering the
	// consumer via propagation.
	hasHardDeps := false
	depIDs := sortedMapKeys(node.DepPaths)
	for _, depID := range depIDs {
		if node.Dependencies[depID] == graph.DepLazy {
			continue
		}
		hasHardDeps = true
		paths := node.DepPaths[depID]
		depData, ok := scope[depID]
		if !ok {
			hashStatePool.Put(hs)
			return "", fmt.Errorf("dependency %q not in scope", depID)
		}
		depMap, ok := depData.(map[string]any)
		if !ok {
			// Non-map dependency (e.g., Watch array) — hash the whole thing.
			hs.buf = append(hs.buf, depID...)
			appendValue(hs, depData)
			continue
		}

		for _, fp := range paths {
			hashFieldPath(hs, depID, fp, depMap)
		}
	}

	// If all dep paths were lazy (skipped), there's nothing to hash.
	// Return "" to disable the eval-hash skip check — lazy dep changes
	// propagate through the output-hash chain, not the input-hash.
	if !hasHardDeps {
		hashStatePool.Put(hs)
		return "", nil
	}

	h := fnv.New64a()
	h.Write(hs.buf)
	result := fmt.Sprintf("%016x", h.Sum64())
	hashStatePool.Put(hs)
	return result, nil
}

// hashSelfPaths computes a hash of the referenced field paths of a node's own
// observed resource. Used to detect self-state changes that require gate
// re-evaluation without template re-evaluation, and for propagation change detection.
//
// Returns "" if the node has no self-paths.
func hashSelfPaths(node *graph.Node, observed any) (string, error) {
	if len(node.SelfPaths) == 0 {
		return "", nil
	}

	observedMap, ok := observed.(map[string]any)
	if !ok {
		return "", nil
	}

	hs := hashStatePool.Get().(*hashState)
	hs.buf = hs.buf[:0]

	for _, fp := range node.SelfPaths {
		hashFieldPath(hs, "", fp, observedMap)
	}

	h := fnv.New64a()
	h.Write(hs.buf)
	result := fmt.Sprintf("%016x", h.Sum64())
	hashStatePool.Put(hs)
	return result, nil
}

// hashFieldPath walks into an object at a field path and appends the value
// to the hashState buffer. If the path is nil (bare reference), the entire
// object is appended. If the path leads to an absent value, a sentinel is
// appended. Volatile metadata fields are excluded when the path enters
// "metadata". Uses the same zero-alloc appendValue path as hashDesiredState.
func hashFieldPath(hs *hashState, prefix string, fp graph.FieldPath, obj map[string]any) {
	// Write the prefix (dependency ID) and path for domain separation.
	hs.buf = append(hs.buf, prefix...)
	for _, seg := range fp {
		hs.buf = append(hs.buf, seg...)
	}

	if len(fp) == 0 {
		// Bare reference — append the entire object.
		appendMap(hs, obj)
		return
	}

	// Walk the path into the object.
	var current any = obj
	for i, segment := range fp {
		m, ok := current.(map[string]any)
		if !ok {
			// Path expects a map but found something else — treat as absent.
			hs.buf = append(hs.buf, "\x00__absent__\x00"...)
			return
		}
		val, exists := m[segment]
		if !exists {
			// Per 005-reconciliation.md: "Absent paths hash to a fixed sentinel."
			hs.buf = append(hs.buf, "\x00__absent__\x00"...)
			return
		}
		// At the first segment, if entering metadata, hash non-volatile keys
		// directly — no intermediate map copy needed.
		if i == 0 && segment == "metadata" {
			if md, ok := val.(map[string]any); ok && i == len(fp)-1 {
				hashMetadataFiltered(hs, md)
				return
			}
			// Non-leaf: path continues to a sub-field (e.g., metadata.name)
			// which has no volatile keys — walk deeper without filtering.
		}
		if i == len(fp)-1 {
			// Leaf — append the value using the zero-alloc path.
			appendValue(hs, val)
			return
		}
		current = val
	}
}

// hashMetadataFiltered appends metadata fields to the buffer, skipping volatile
// fields. Avoids creating an intermediate filtered map — iterates in sorted key
// order and skips volatile keys inline.
func hashMetadataFiltered(hs *hashState, md map[string]any) {
	keys := make([]string, 0, len(md))
	for k := range md {
		if !volatileMetadataFields[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	hs.buf = append(hs.buf, '{')
	for i, k := range keys {
		if i > 0 {
			hs.buf = append(hs.buf, ',')
		}
		appendQuotedString(hs, k)
		hs.buf = append(hs.buf, ':')
		appendValue(hs, md[k])
	}
	hs.buf = append(hs.buf, '}')
}

// hashForEachContext computes a hash of the shared dependency context for a
// forEach node. Computed once per forEach node per reconcile, then included
// in each per-item cached hash as a prefix. When context changes but
// collection items are stable, the prefix differs and all cached hashes
// become stale — forcing re-evaluation.
func hashForEachContext(scope map[string]any, deps map[string]graph.DepKind) string {
	h := fnv.New64a()
	depIDs := make([]string, 0, len(deps))
	for id := range deps {
		depIDs = append(depIDs, id)
	}
	sort.Strings(depIDs)
	hs := hashStatePool.Get().(*hashState)
	for _, depID := range depIDs {
		hs.buf = hs.buf[:0]
		h.Write([]byte(depID))
		if v, ok := scope[depID]; ok {
			appendValue(hs, v)
			h.Write(hs.buf)
		}
	}
	hashStatePool.Put(hs)
	return fmt.Sprintf("%016x", h.Sum64())
}

// sortedMapKeys returns the keys of any map[string]V in sorted order.
func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
