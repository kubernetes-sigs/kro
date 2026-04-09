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
	return obj, ok
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
// Checks the graph-name and graph-namespace labels on each cached object.
// Scoped to a single Graph — does not affect cache entries from other Graphs.
//
// This is safe because only applyResource and applyContribution call set(),
// and both stamp LabelGraphName/LabelGraphNamespace on every applied object.
// Watch-read objects are stored in the evaluator scope, not in this cache.
func (rc *resourceCache) removeForGraph(graphName, graphNamespace string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for key, obj := range rc.objects {
		if obj.object == nil {
			continue
		}
		md, _ := obj.object["metadata"].(map[string]any)
		if md == nil {
			continue
		}
		labels, _ := md["labels"].(map[string]any)
		if labels == nil {
			continue
		}
		if labels[LabelGraphName] == graphName && labels[LabelGraphNamespace] == graphNamespace {
			delete(rc.objects, key)
		}
	}
}

// resourceCacheKey builds a cache key for a resource from its identifying fields.
func resourceCacheKey(apiVersion, kind, namespace, name string) string {
	return apiVersion + "/" + kind + "/" + namespace + "/" + name
}
