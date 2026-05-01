// setup.go initializes the Graph controller — watch infrastructure, schema
// resolution, reconciler struct, and controller-runtime registration.
// Called once from cmd/main.go at startup. Separated from controller.go
// because initialization and reconciliation are different lifecycles.
package graphcontroller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/ellistarn/kro/experimental/controller/compiler"
	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
	"github.com/ellistarn/kro/experimental/controller/watches"
	schemaresolver "github.com/kubernetes-sigs/kro/pkg/graph/schema/resolver"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// hydrateWatchCachesFromRevisions lists all existing GraphRevisions and starts
// a watch informer for every GVR referenced in their node templates.
//
// This is the replacement for the per-revision finalizer. After a controller
// restart, the three sources for allPreviousKeys in the prune phase are:
//
//	(1) deriveAppliedSet — watch cache, requires an active informer for the GVR
//	(2) state.previousAppliedKeys — in-memory, lost on restart
//	(3) superseded revision static keys — extracted from revision objects
//
// Without hydration, source (1) is empty on startup for cross-GVR transitions:
// if the current revision manages a ConfigMap but a superseded revision managed
// a Deployment, no ConfigMap informer is running for the Deployment GVR.
// Hydration fixes this by starting informers eagerly from all existing
// revisions before the first reconcile fires — using the same graphOwnerID as
// the normal reconcile path so ref-counting works naturally.
//
// Called synchronously in SetupWithManager before the controller is registered,
// so there is no window where a reconcile fires before hydration completes.
func hydrateWatchCachesFromRevisions(restConfig *rest.Config, watchMgr *watches.WatchManager) {
	logger := log.Log.WithName("startup-hydration")

	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		logger.Error(err, "creating dynamic client; skipping startup watch hydration")
		return
	}

	graphRevisionGVR := schema.GroupVersionResource{
		Group:    "experimental.kro.run",
		Version:  "v1alpha1",
		Resource: "graphrevisions",
	}

	list, err := dynClient.Resource(graphRevisionGVR).Namespace("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		// CRD may not exist on first installation. Log and skip — no existing
		// revisions to hydrate from.
		logger.Info("could not list GraphRevisions; skipping startup watch hydration", "err", err)
		return
	}

	// Collect unique (graph, gvr) pairs across all revisions.
	type hydrateKey struct {
		graph watches.GraphKey
		gvr   schema.GroupVersionResource
		kind  string
	}
	toHydrate := make(map[hydrateKey]struct{})

	for i := range list.Items {
		rev := &list.Items[i]
		graphName := rev.GetLabels()[graphpkg.LabelRevisionGraphName]
		if graphName == "" {
			continue
		}
		graph := watches.GraphKey{Name: graphName, Namespace: rev.GetNamespace()}

		spec, err := extractRevisionSpec(rev)
		if err != nil {
			logger.V(1).Info("skipping revision during hydration", "revision", rev.GetName(), "err", err)
			continue
		}
		for _, node := range spec.Nodes {
			if node.Finalizes != "" {
				continue // finalizer node — dormant during normal operation
			}
			if node.HasDynamicGVR() {
				continue // GVR contains CEL — resolved at reconcile time, not startup
			}
			id := node.Identity()
			apiVersion, _ := id["apiVersion"].(string)
			kind, _ := id["kind"].(string)
			if apiVersion == "" || kind == "" {
				continue
			}
			gv, err := schema.ParseGroupVersion(apiVersion)
			if err != nil {
				continue
			}
			gvr := gvkToGVR(gv.WithKind(kind))
			toHydrate[hydrateKey{graph: graph, gvr: gvr, kind: kind}] = struct{}{}
		}
	}

	if len(toHydrate) == 0 {
		return
	}

	// Start informers in parallel — each ensureWatch blocks until cache sync,
	// so parallel execution reduces startup time from O(n·30s) to O(30s).
	var wg sync.WaitGroup
	for k := range toHydrate {
		wg.Add(1)
		go func(graph watches.GraphKey, gvr schema.GroupVersionResource, kind string) {
			defer wg.Done()
			ownerID := watches.GraphOwnerID(graph)
			if err := watchMgr.EnsureWatch(gvr, kind, ownerID); err != nil {
				logger.Error(err, "failed to hydrate watch", "gvr", gvr, "graph", graph.Name)
			} else {
				logger.V(1).Info("hydrated watch from revision", "gvr", gvr, "graph", graph.Name)
			}
		}(k.graph, k.gvr, k.kind)
	}
	wg.Wait()
	logger.Info("startup watch hydration complete", "watchCount", len(toHydrate))
}

// SetupWithManager registers the Graph controller with a controller-runtime
// manager. This is the single setup path for both production and tests.
// It creates the watch infrastructure internally — callers provide the
// manager and a rest.Config (needed for the metadata client).
//
// maxWorkers controls MaxConcurrentReconciles. Values ≤ 0 default to 4.
// Multiple workers prevent watch event starvation under load — with a
// single worker, dynamic watch events can't be delivered while it's busy
// processing another Graph's reconcile.
//
// Returns a shutdown function that stops the watch manager. The caller
// must invoke this on teardown.
func SetupWithManager(mgr ctrl.Manager, restConfig *rest.Config, maxWorkers int) (shutdown func(), caches *graphCaches, err error) {
	RegisterMetrics(crmetrics.Registry)

	if maxWorkers <= 0 {
		maxWorkers = DefaultMaxConcurrentReconciles
	}

	metadataClient, err := metadata.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("creating metadata client: %w", err)
	}

	watchChan := make(chan event.GenericEvent, 256)

	watchMgr := watches.NewWatchManager(metadataClient, 12*time.Hour, nil, log.Log)
	coordinator := watches.NewWatchCoordinator(watchMgr, func(graph watches.GraphKey) {
		obj := &unstructured.Unstructured{}
		obj.SetName(graph.Name)
		obj.SetNamespace(graph.Namespace)
		watchChan <- event.GenericEvent{Object: obj}
	}, log.Log)
	watchMgr.SetOnEvent(coordinator.RouteEvent)

	// Create schema resolver for compile-time type checking.
	// Core types resolve from compiled-in definitions, CRDs resolve via
	// cached discovery client.
	//
	// The HTTP client must inherit TLS settings from rest.Config (CAData,
	// ServerName, ClientCert). Passing nil causes the discovery client to
	// build an internal HTTP client that doesn't pick up TLS config in all
	// environments (notably envtest with self-signed certs). Mirrors
	// upstream pkg/client/set.go which constructs rest.HTTPClientFor once.
	httpClient, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		log.Log.Error(err, "failed to build HTTP client for schema resolver; compile-time type checking disabled for resource nodes")
		httpClient = nil
	}
	schemaResolver, err := schemaresolver.NewCombinedResolver(restConfig, httpClient)
	if err != nil {
		// Schema resolution is an operational dependency — log the failure.
		// All resource nodes will fall back to dyn (no field-level type checking).
		log.Log.Error(err, "failed to create schema resolver; compile-time type checking disabled for resource nodes")
		schemaResolver = nil
	}

	reconciler := &GraphReconciler{
		Client:         mgr.GetClient(),
		APIReader:      mgr.GetAPIReader(),
		SchemaResolver: schemaResolver,
		SchemaGen:      compiler.NewSchemaGeneration(),
		Watcher:        coordinator,
		Caches:         newGraphCaches(),
		// Scope is used by staticResourceKey to avoid namespacing
		// cluster-scoped resource keys. Without this, prune/teardown
		// silently miss cluster-scoped resources because their keys
		// never match post-apply resourceKey(). Per 003-ownership.md
		// § Priority Resolution.
		Scope: newRESTMapperGVKScopeResolver(mgr.GetRESTMapper()),
	}

	// When the watch infrastructure observes a new type (first informer for a
	// GVR), advance the schema generation so compiled artifacts become stale,
	// then requeue all cached graphs. The next reconcile for each graph will
	// detect staleness via the TypeCacheGen check and recompile. This is a
	// rare event (CRD installation) so iterating all cached instances is fine.
	watchMgr.SetOnNewType(func(gvr schema.GroupVersionResource) {
		if reconciler.SchemaGen != nil {
			reconciler.SchemaGen.AdvanceGeneration()
		}
		keys := reconciler.Caches.instanceKeys()
		if len(keys) == 0 {
			return
		}
		log.Log.Info("new type observed; requeuing all graphs for staleness check",
			"gvr", gvr, "instanceCount", len(keys))
		for _, key := range keys {
			slash := strings.Index(key, "/")
			if slash < 0 {
				continue
			}
			ns := key[:slash]
			revName := key[slash+1:]
			// Revision name format: "graphname-gNNNNN" — extract graph name.
			lastDash := strings.LastIndex(revName, "-g")
			graphName := revName
			if lastDash >= 0 && lastDash+2 < len(revName) {
				graphName = revName[:lastDash]
			}
			obj := &unstructured.Unstructured{}
			obj.SetName(graphName)
			obj.SetNamespace(ns)
			select {
			case watchChan <- event.GenericEvent{Object: obj}:
			default:
			}
		}
	})

	// Watch CRDs for schema changes. Per 004-compilation.md § Compilation Cache:
	// "Any schema change (CRD installed, updated, removed) advances [the
	// generation counter]." The onNewType callback handles CRD install (first
	// informer for a GVR). This watch handles CRD updates and deletions —
	// schema changes to already-watched types.
	crdGVR := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	crdCtx, crdCancel := context.WithCancel(context.Background())
	crdGenericInformer := metadatainformer.NewFilteredMetadataInformer(metadataClient, crdGVR, metav1.NamespaceAll, 0, cache.Indexers{}, nil)
	crdInformer := crdGenericInformer.Informer()
	crdInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{ //nolint:errcheck
		UpdateFunc: func(_, _ any) {
			if reconciler.SchemaGen != nil {
				reconciler.SchemaGen.AdvanceGeneration()
			}
		},
		DeleteFunc: func(_ any) {
			if reconciler.SchemaGen != nil {
				reconciler.SchemaGen.AdvanceGeneration()
			}
		},
	})
	go crdInformer.RunWithContext(crdCtx)
	_ = crdCancel // stopped when the WatchManager shuts down (same process lifecycle)

	// Pre-populate watch informers from existing GraphRevisions before the
	// controller starts. This ensures deriveAppliedSet works for cross-GVR
	// transitions on the first reconcile after a restart — no window where
	// a reconcile fires before the cache is hydrated.
	hydrateWatchCachesFromRevisions(restConfig, watchMgr)

	graphObj := &unstructured.Unstructured{}
	graphObj.SetGroupVersionKind(GraphGVK)

	if err := ctrl.NewControllerManagedBy(mgr).
		For(graphObj).
		Named("graph").
		WithOptions(controller.Options{MaxConcurrentReconciles: maxWorkers}).
		WatchesRawSource(source.Channel(watchChan, &handler.EnqueueRequestForObject{})).
		Complete(reconciler); err != nil {
		return nil, nil, fmt.Errorf("building controller: %w", err)
	}

	return watchMgr.Shutdown, reconciler.Caches, nil
}
