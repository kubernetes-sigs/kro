// watcher.go implements GraphWatcher — a scoped handle for a single Graph's
// watch registrations. Watches are long-lived: registered immediately via the
// coordinator and persist until the graph is deleted. No buffering, no
// per-cycle flush, no lifecycle methods.
package watches

import (
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GraphWatcher is a scoped handle for registering watches on behalf of a
// single Graph. It is a thin wrapper around the coordinator — stateless,
// no mutex, no buffering. Create it, use it, discard it.
type GraphWatcher struct {
	coord *WatchCoordinator
	graph GraphKey
}

func (c *WatchCoordinator) ForGraph(graph GraphKey) *GraphWatcher {
	return &GraphWatcher{coord: c, graph: graph}
}

// WatchScalar registers a named-object watch. Idempotent: same params = no-op,
// different params for the same nodeID = swap.
func (gw *GraphWatcher) WatchScalar(nodeID string, gvr schema.GroupVersionResource, kind string, name, namespace string) {
	if gw == nil {
		return
	}
	gw.coord.ensureNodeWatch(gw.graph, nodeID, gvr, kind, name, namespace, nil)
}

// WatchCollection registers a selector-scoped watch. Idempotent: same params = no-op,
// different params for the same nodeID = swap.
func (gw *GraphWatcher) WatchCollection(nodeID string, gvr schema.GroupVersionResource, kind string, namespace string, sel labels.Selector) {
	if gw == nil {
		return
	}
	gw.coord.ensureNodeWatch(gw.graph, nodeID, gvr, kind, "", namespace, sel)
}

// GetLabels returns the labels from the metadata informer cache for a specific
// object. Returns nil, false if not found or no watch exists.
func (gw *GraphWatcher) GetLabels(gvr schema.GroupVersionResource, namespace, name string) (map[string]string, bool) {
	if gw == nil {
		return nil, false
	}
	return gw.coord.GetLabels(gvr, namespace, name)
}
