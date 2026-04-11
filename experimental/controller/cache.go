// cache.go implements content-addressed apply gating and metadata-driven read
// elision for the Graph controller.
//
// The resource cache stores the full object from the last successful read/apply
// alongside its resourceVersion and template hash. On reconcile, the controller
// checks the metadata informer for the current resourceVersion and compares
// the template hash to decide whether to skip the Patch, skip the GET, or both.
package graphcontroller

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
)

const templateHashAnnotation = "internal.kro.run/template-hash"

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
	templateHash    string         // hash of the template we last applied
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
		templateHash:    obj.templateHash,
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
// This is safe because only applyResource and applyContribution call set(),
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
// Section-scoped input hashing
// ---------------------------------------------------------------------------

// volatileMetadataFields are metadata keys excluded from section-scoped hashing.
// These change on every write without conveying semantic meaning — including them
// would defeat the input hash (same problem as full-object hashing).
var volatileMetadataFields = map[string]bool{
	"resourceVersion":            true,
	"managedFields":              true,
	"creationTimestamp":          true,
	"uid":                        true,
	"selfLink":                   true,
	"generation":                 true,
	"deletionGracePeriodSeconds": true,
}

// hashNodeInputs computes a section-scoped hash of a node's dependency inputs.
// Only the top-level sections that the node's CEL expressions reference are
// included. For metadata, volatile fields (resourceVersion, managedFields, etc.)
// are excluded.
//
// Returns "" if the node has no dependency sections to hash (e.g., Watch nodes
// with no upstream dependencies).
func hashNodeInputs(node *Node, scope map[string]any) (string, error) {
	if len(node.DepSections) == 0 {
		return "", nil
	}

	h := fnv.New64a()
	// Process dependencies in sorted order for deterministic hashing.
	depIDs := sortedKeys(node.DepSections)
	for _, depID := range depIDs {
		sections := node.DepSections[depID]
		depData, ok := scope[depID]
		if !ok {
			// Dependency not in scope — this node can't be evaluated yet.
			// Return a sentinel that won't match any previous hash.
			return "", fmt.Errorf("dependency %q not in scope", depID)
		}
		depMap, ok := depData.(map[string]any)
		if !ok {
			// Non-map dependency (e.g., collection watch array) — hash the whole thing.
			data, err := json.Marshal(depData)
			if err != nil {
				return "", fmt.Errorf("hashing dependency %q: %w", depID, err)
			}
			h.Write([]byte(depID))
			h.Write(data)
			continue
		}

		sectionNames := sortedBoolKeys(sections)
		for _, section := range sectionNames {
			sectionData, ok := depMap[section]
			if !ok {
				// Absent section: hash a sentinel so absent→present is a change.
				// Per 004-graph-execution.md: "Absent paths hash to a fixed
				// sentinel value that is not a valid Kubernetes field value."
				h.Write([]byte(depID))
				h.Write([]byte(section))
				h.Write([]byte("\x00__absent__\x00"))
				continue
			}
			// For metadata, exclude volatile fields.
			if section == "metadata" {
				sectionData = filterVolatileMetadata(sectionData)
			}
			data, err := json.Marshal(sectionData)
			if err != nil {
				return "", fmt.Errorf("hashing %s.%s: %w", depID, section, err)
			}
			h.Write([]byte(depID))
			h.Write([]byte(section))
			h.Write(data)
		}
	}

	return fmt.Sprintf("%016x", h.Sum64()), nil
}

// hashSelfSections computes a hash of the referenced sections of a node's own
// observed resource. Used to detect self-state changes (e.g., status updated by
// another controller) that require gate re-evaluation without template re-evaluation.
//
// Returns "" if the node has no self-sections (no readyWhen/propagateWhen
// referencing self fields).
func hashSelfSections(node *Node, observed any) (string, error) {
	if len(node.SelfSections) == 0 {
		return "", nil
	}

	observedMap, ok := observed.(map[string]any)
	if !ok {
		return "", nil
	}

	h := fnv.New64a()
	sectionNames := sortedBoolKeys(node.SelfSections)
	for _, section := range sectionNames {
		sectionData, ok := observedMap[section]
		if !ok {
			// Absent section: hash a sentinel so absent→present is a change.
			// Per 004-graph-execution.md: "Absent paths hash to a fixed
			// sentinel value that is not a valid Kubernetes field value."
			h.Write([]byte(section))
			h.Write([]byte("\x00__absent__\x00"))
			continue
		}
		if section == "metadata" {
			sectionData = filterVolatileMetadata(sectionData)
		}
		data, err := json.Marshal(sectionData)
		if err != nil {
			return "", fmt.Errorf("hashing self.%s: %w", section, err)
		}
		h.Write([]byte(section))
		h.Write(data)
	}

	return fmt.Sprintf("%016x", h.Sum64()), nil
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

// sortedKeys returns the keys of a map[string]map[string]bool in sorted order.
func sortedKeys(m map[string]map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedBoolKeys returns the keys of a map[string]bool in sorted order.
func sortedBoolKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
