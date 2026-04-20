// cache.go implements content-addressed apply gating and metadata-driven read
// elision for the Graph controller.
//
// The resource cache stores the full object from the last successful read/apply
// alongside its resourceVersion and apply hash. On reconcile, the controller
// checks the metadata informer for the current resourceVersion and compares
// the apply hash to decide whether to skip the Patch, skip the GET, or both.
package graphcontroller

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"
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

// ---------------------------------------------------------------------------
// Resource cache
// ---------------------------------------------------------------------------

// cachedObject holds the last known state of a managed resource.
type cachedObject struct {
	resourceVersion string         // from the API server
	applyHash       string         // hash of the desired state we last applied
	object          map[string]any // full object from last GET/Patch response
}

// resourceCache is a concurrent-safe cache of full objects keyed by
// GVR/namespace/name. Used to elide GETs when metadata indicates the
// object hasn't changed.
type resourceCache struct {
	mu      sync.RWMutex
	objects map[string]*cachedObject
}

func newResourceCache() *resourceCache {
	return &resourceCache{objects: make(map[string]*cachedObject)}
}

func (rc *resourceCache) get(key string) (*cachedObject, bool) {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	obj, ok := rc.objects[key]
	if !ok {
		return nil, false
	}
	// Return a shallow copy with a deep-copied object map. The caller
	// may write to the map (markReady injects __ready), so the cache
	// must not share map references with callers. Without this copy,
	// removeForGraph's label iteration races with worker writes.
	return &cachedObject{
		resourceVersion: obj.resourceVersion,
		applyHash:       obj.applyHash,
		object:          deepCopyMap(obj.object),
	}, true
}

func (rc *resourceCache) set(key string, obj *cachedObject) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	// Normalize numeric types once on store so cached objects are ready for
	// CEL evaluation without per-read conversion.
	obj.object = normalizeTypes(obj.object).(map[string]any)
	rc.objects[key] = obj
}

func (rc *resourceCache) remove(key string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	delete(rc.objects, key)
}

// removeForGraph removes cached objects that belong to the specified Graph.
// Checks for identity labels using the DNS subdomain scheme.
// Scoped to a single Graph — does not affect cache entries from other Graphs.
//
// This is safe because only applySSA calls set(),
// and both stamp identity labels on every applied object.
// Watch-read objects are stored in the evaluator scope, not in this cache.
func (rc *resourceCache) removeForGraph(graphName, graphNamespace string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	suffix := graphLabelSuffix(graphName, graphNamespace)
	for key, obj := range rc.objects {
		if obj.object == nil {
			continue
		}
		md, _ := obj.object["metadata"].(map[string]any)
		if md == nil {
			continue
		}
		lbls, _ := md["labels"].(map[string]any)
		if lbls == nil {
			continue
		}
		for labelKey := range lbls {
			if strings.HasSuffix(labelKey, suffix) {
				delete(rc.objects, key)
				break
			}
		}
	}
}

// resourceCacheKey builds a cache key for a resource from its identifying fields.
func resourceCacheKey(apiVersion, kind, namespace, name string) string {
	return apiVersion + "/" + kind + "/" + namespace + "/" + name
}

// ---------------------------------------------------------------------------
// Field-path-scoped evaluation hashing
// ---------------------------------------------------------------------------

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
func hashNodeInputs(node *Node, scope map[string]any) (string, error) {
	if len(node.DepPaths) == 0 {
		return "", nil
	}

	hs := hashStatePool.Get().(*hashState)
	hs.buf = hs.buf[:0]

	// Process dependencies in sorted order for deterministic hashing.
	depIDs := sortedMapKeys(node.DepPaths)
	for _, depID := range depIDs {
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
func hashSelfPaths(node *Node, observed any) (string, error) {
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
func hashFieldPath(hs *hashState, prefix string, fp FieldPath, obj map[string]any) {
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
func hashForEachContext(scope map[string]any, deps map[string]bool) string {
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

// ---------------------------------------------------------------------------
// Spec hashing
// ---------------------------------------------------------------------------

// Hash computes a deterministic content hash of the compilation inputs.
// Two GraphSpecs that produce the same hash will produce identical compiledGraphs
// (same CEL programs, same DAG, same expression set).
//
// The hash covers: node IDs, template structures, forEach definitions,
// includeWhen/readyWhen/propagateWhen conditions — everything that feeds into
// compileGraphSpec. If a new compilation input is added to GraphSpec without
// updating this hash, content-addressed sharing will silently reuse stale
// compiled graphs. The test TestSpecHashCoversCompilationInputs guards against this.
//
// Each field is length-prefixed (binary.LittleEndian int64) before its content
// to prevent delimiter injection / field boundary ambiguity. This is strictly
// correct — no two distinct specs can produce the same hash input sequence.
//
// Collision probability: FNV-64a has a 64-bit output. At 1000 distinct specs
// the collision probability is ~2.7e-14 (birthday bound). At 1M distinct specs
// it's ~2.7e-8. This is accepted as negligible for an in-memory optimization
// cache. A collision would cause two different specs to share a compiled graph,
// producing incorrect CEL evaluation. If this ever matters, upgrade to FNV-128
// or SHA-256 — the hash is not on the hot path.
//
// Implementation notes:
//   - encoding/json sorts map keys deterministically in Go, so json.Marshal
//     of map[string]any produces a canonical byte sequence.
//   - Nodes are processed in declaration order (spec order is stable from
//     Kubernetes API). IncludeWhen, ReadyWhen, PropagateWhen are slices
//     (order-stable from spec parsing). ForEach is a map (sorted explicitly).
func (s *GraphSpec) Hash() string {
	h := fnv.New64a()

	// hashField writes a length-prefixed field to the hash.
	// Length prefix eliminates field boundary ambiguity (the classic delimiter
	// injection problem) without requiring reserved separator bytes.
	hashField := func(data []byte) {
		binary.Write(h, binary.LittleEndian, int64(len(data))) //nolint:errcheck
		h.Write(data)
	}

	// Process nodes in declaration order (spec order is stable).
	for _, node := range s.Nodes {
		// Node ID
		hashField([]byte(node.ID))

		// Body — Payload() covers Template/Patch/Def; Identity() covers
		// Ref/Watch (identity-only, no Payload). Either way the
		// body uniquely determines the node's spec.
		if payload := node.Payload(); payload != nil {
			data, _ := json.Marshal(payload)
			hashField(data)
		} else if identity := node.Identity(); identity != nil {
			data, _ := json.Marshal(identity)
			hashField(data)
		} else if node.TemplateExpr != "" {
			hashField([]byte(node.TemplateExpr))
		} else {
			hashField(nil)
		}

		// Finalizes
		hashField([]byte(node.Finalizes))

		// ForEach (direct field access — single dimension)
		if node.ForEach != nil {
			hashField([]byte(node.ForEach.VarName))
			hashField([]byte(node.ForEach.Expr))
			binary.Write(h, binary.LittleEndian, int64(1)) //nolint:errcheck
		} else {
			binary.Write(h, binary.LittleEndian, int64(0)) //nolint:errcheck
		}

		// Conditions — slices, order-stable from spec parsing.
		for _, c := range node.IncludeWhen {
			hashField([]byte(c))
		}
		binary.Write(h, binary.LittleEndian, int64(len(node.IncludeWhen))) //nolint:errcheck

		for _, c := range node.ReadyWhen {
			hashField([]byte(c))
		}
		binary.Write(h, binary.LittleEndian, int64(len(node.ReadyWhen))) //nolint:errcheck

		for _, c := range node.PropagateWhen {
			hashField([]byte(c))
		}
		binary.Write(h, binary.LittleEndian, int64(len(node.PropagateWhen))) //nolint:errcheck
	}

	return fmt.Sprintf("%016x", h.Sum64())
}

// CompilationKey returns a structural hash of the spec that considers only
// inputs affecting compilation output. Unlike Hash(), which is a full content
// hash, CompilationKey ignores concrete values (string literals without
// expressions, numbers, booleans) that don't influence the compiled CEL
// programs, DAG, or type environment.
//
// Per 004-compilation.md § Structural Compilation Caching: "Two specs with
// the same compilation key produce the same compiled output."
func (s *GraphSpec) CompilationKey() string {
	h := fnv.New64a()

	hashField := func(data []byte) {
		binary.Write(h, binary.LittleEndian, int64(len(data))) //nolint:errcheck
		h.Write(data)
	}

	for _, node := range s.Nodes {
		// Node ID — always compilation-relevant (CEL variable name).
		hashField([]byte(node.ID))

		// Node type — affects how the body is processed.
		binary.Write(h, binary.LittleEndian, int64(node.Type())) //nolint:errcheck

		// Body: hash only compilation-relevant strings (expressions,
		// literal apiVersion/kind for schema resolution). Concrete
		// values without expressions are excluded.
		if node.TemplateExpr != "" {
			hashField([]byte(node.TemplateExpr))
		} else if body := node.Body(); body != nil {
			hashCompilationRelevantFields(h, hashField, body)
		}

		// Finalizes
		hashField([]byte(node.Finalizes))

		// ForEach — compilation-relevant (variable name + collection expression).
		if node.ForEach != nil {
			hashField([]byte(node.ForEach.VarName))
			hashField([]byte(node.ForEach.Expr))
			binary.Write(h, binary.LittleEndian, int64(1)) //nolint:errcheck
		} else {
			binary.Write(h, binary.LittleEndian, int64(0)) //nolint:errcheck
		}

		// Conditions — always compilation-relevant.
		for _, c := range node.IncludeWhen {
			hashField([]byte(c))
		}
		binary.Write(h, binary.LittleEndian, int64(len(node.IncludeWhen))) //nolint:errcheck
		for _, c := range node.ReadyWhen {
			hashField([]byte(c))
		}
		binary.Write(h, binary.LittleEndian, int64(len(node.ReadyWhen))) //nolint:errcheck
		for _, c := range node.PropagateWhen {
			hashField([]byte(c))
		}
		binary.Write(h, binary.LittleEndian, int64(len(node.PropagateWhen))) //nolint:errcheck
	}

	return fmt.Sprintf("%016x", h.Sum64())
}

// compilationKeyWithHints produces the full cache key by combining the
// structural compilation key with resolved dynamic GVK hints. Instances
// with the same structure AND same resolved GVKs share a compiled artifact.
// When hints is nil or empty, returns the structural key unchanged (bootstrap).
func compilationKeyWithHints(structuralKey string, hints map[string]schema.GroupVersionKind) string {
	if len(hints) == 0 {
		return structuralKey
	}
	// Sort node IDs for deterministic hashing.
	nodeIDs := make([]string, 0, len(hints))
	for id := range hints {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)

	h := fnv.New64a()
	h.Write([]byte(structuralKey))
	for _, id := range nodeIDs {
		gvk := hints[id]
		h.Write([]byte(id))
		h.Write([]byte(gvk.Group))
		h.Write([]byte(gvk.Version))
		h.Write([]byte(gvk.Kind))
	}
	return fmt.Sprintf("%s+%016x", structuralKey, h.Sum64())
}

// hashCompilationRelevantFields walks a body map and hashes only the fields
// that affect compilation: expression strings (containing ${...} or $${...}),
// and literal apiVersion/kind values (which affect schema resolution).
func hashCompilationRelevantFields(h hash.Hash64, hashField func([]byte), body map[string]any) {
	// apiVersion and kind affect schema resolution → compilation-relevant.
	if av, ok := body["apiVersion"].(string); ok {
		hashField([]byte("apiVersion"))
		hashField([]byte(av))
	}
	if k, ok := body["kind"].(string); ok {
		hashField([]byte("kind"))
		hashField([]byte(k))
	}

	// Walk the body for expression strings.
	var strs []string
	collectStrings(body, &strs)
	sort.Strings(strs) // deterministic order
	for _, s := range strs {
		if strings.Contains(s, "${") {
			hashField([]byte(s))
		}
	}
}
