// cache.go implements content-addressed apply gating and metadata-driven read
// elision for the Graph controller.
//
// The resource cache stores the full object from the last successful read/apply
// alongside its resourceVersion and apply hash. On reconcile, the controller
// checks the metadata informer for the current resourceVersion and compares
// the apply hash to decide whether to skip the Patch, skip the GET, or both.
package graphcontroller

import (
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
)

const applyHashAnnotation = "internal.kro.run/template-hash"

// hashDesiredState computes a content hash of an evaluated template map.
// Uses FNV-64a over Go's json.Marshal output. json.Marshal produces
// deterministic output for map[string]any (keys sorted at every level),
// so the hash is stable within the same Go runtime.
func hashDesiredState(evalMap map[string]any) (string, error) {
	data, err := json.Marshal(evalMap)
	if err != nil {
		return "", fmt.Errorf("hashing desired state: %w", err)
	}
	h := fnv.New64a()
	h.Write(data)
	return fmt.Sprintf("%016x", h.Sum64()), nil
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
// Per 004-graph-execution.md § Change detection: "At graph compilation, the
// controller walks each compiled expression's AST to extract reference chains."
//
// Returns "" if the node has no dependency paths to hash.
func hashNodeInputs(node *Node, scope map[string]any) (string, error) {
	if len(node.DepPaths) == 0 {
		return "", nil
	}

	h := fnv.New64a()
	// Process dependencies in sorted order for deterministic hashing.
	depIDs := sortedMapKeys(node.DepPaths)
	for _, depID := range depIDs {
		paths := node.DepPaths[depID]
		depData, ok := scope[depID]
		if !ok {
			return "", fmt.Errorf("dependency %q not in scope", depID)
		}
		depMap, ok := depData.(map[string]any)
		if !ok {
			// Non-map dependency (e.g., WatchKind array) — hash the whole thing.
			data, err := json.Marshal(depData)
			if err != nil {
				return "", fmt.Errorf("hashing dependency %q: %w", depID, err)
			}
			h.Write([]byte(depID))
			h.Write(data)
			continue
		}

		for _, fp := range paths {
			hashFieldPath(h, depID, fp, depMap)
		}
	}

	return fmt.Sprintf("%016x", h.Sum64()), nil
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

	h := fnv.New64a()
	for _, fp := range node.SelfPaths {
		hashFieldPath(h, "", fp, observedMap)
	}

	return fmt.Sprintf("%016x", h.Sum64()), nil
}

// hashFieldPath walks into an object at a field path and hashes the value found.
// If the path is nil (bare reference), the entire object is hashed.
// If the path leads to an absent value, a sentinel is hashed.
// Volatile metadata fields are excluded when the path enters "metadata".
func hashFieldPath(h hash.Hash64, prefix string, fp FieldPath, obj map[string]any) {
	// Write the prefix (dependency ID) and path for domain separation.
	h.Write([]byte(prefix))
	for _, seg := range fp {
		h.Write([]byte(seg))
	}

	if len(fp) == 0 {
		// Bare reference — hash the entire object.
		data, err := json.Marshal(obj)
		if err != nil {
			h.Write([]byte("\x00__error__\x00"))
			return
		}
		h.Write(data)
		return
	}

	// Walk the path into the object.
	var current any = obj
	for i, segment := range fp {
		m, ok := current.(map[string]any)
		if !ok {
			// Path expects a map but found something else — treat as absent.
			h.Write([]byte("\x00__absent__\x00"))
			return
		}
		val, exists := m[segment]
		if !exists {
			// Per 004-graph-execution.md: "Absent paths hash to a fixed sentinel."
			h.Write([]byte("\x00__absent__\x00"))
			return
		}
		// At the first segment, check for metadata volatile field filtering.
		if i == 0 && segment == "metadata" {
			val = filterVolatileMetadata(val)
		}
		if i == len(fp)-1 {
			// Leaf — hash the value.
			data, err := json.Marshal(val)
			if err != nil {
				h.Write([]byte("\x00__error__\x00"))
				return
			}
			h.Write(data)
			return
		}
		current = val
	}
}

// filterVolatileMetadata returns a copy of metadata with volatile fields removed.
func filterVolatileMetadata(metadata any) any {
	md, ok := metadata.(map[string]any)
	if !ok {
		return metadata
	}
	filtered := make(map[string]any, len(md))
	for k, v := range md {
		if !volatileMetadataFields[k] {
			filtered[k] = v
		}
	}
	return filtered
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
