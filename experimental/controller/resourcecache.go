// resourcecache.go implements a concurrent-safe cache of managed resource
// objects, keyed by GVR/namespace/name.
//
// The resource cache stores the full object from the last successful read/apply
// alongside its resourceVersion and apply hash. On reconcile, the controller
// checks the metadata informer for the current resourceVersion and compares
// the apply hash to decide whether to skip the Patch, skip the GET, or both.
package graphcontroller

import (
	"strings"
	"sync"

	"github.com/kubernetes-sigs/kro/experimental/controller/graph"
)

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
	obj.object = graph.NormalizeTypes(obj.object).(map[string]any)
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
	suffix := graph.GraphLabelSuffix(graphName, graphNamespace)
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
