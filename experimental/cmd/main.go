// Binary entrypoint for the experimental Graph controller.
//
// Run with --bootstrap to automatically install CRDs:
//
//	go run ./experimental/cmd/ --bootstrap
//
// Or install manifests manually first:
//
//	kubectl apply -f experimental/deploy/
//	go run ./experimental/cmd/
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on DefaultServeMux
	"os"
	"runtime/debug"
	"time"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	graphcontroller "github.com/kubernetes-sigs/kro/experimental/controller"
	"github.com/kubernetes-sigs/kro/experimental/stdlib"
)

func main() {
	// Pin the controller-runtime logger before any other package can
	// touch it. The delegating sink warns after 30s if SetLogger is
	// called late, and leaves earlier calls buffered — both hurt
	// observability.
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("main")
	log.Info("graph-controller starting")

	var (
		bootstrapFlag          bool
		healthProbeBindAddress string
		metricsBindAddress     string
		pprofBindAddress       string
		maxWorkers             int
		driftInterval          time.Duration
	)

	flag.BoolVar(&bootstrapFlag, "bootstrap", false, "Install CRDs before starting the controller")
	flag.StringVar(&healthProbeBindAddress, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to. Use :0 for a random port.")
	flag.StringVar(&metricsBindAddress, "metrics-bind-address", "0", "The address the metrics endpoint binds to. Use 0 to disable.")
	flag.StringVar(&pprofBindAddress, "pprof-bind-address", "", "The address the pprof endpoint binds to. Empty to disable.")
	flag.IntVar(&maxWorkers, "max-workers", 0, "Maximum concurrent reconcile workers. 0 uses the default.")
	flag.DurationVar(&driftInterval, "drift-interval", 0, "Per-node drift timer interval. 0 uses the default (30m).")
	flag.Parse()

	if pprofBindAddress != "" {
		// Register pprof handlers — will start after SetupWithManager.
		http.HandleFunc("/debug/freeosmemory", func(w http.ResponseWriter, r *http.Request) {
			debug.FreeOSMemory()
			w.Write([]byte("ok\n"))
		})
	}

	cfg := ctrl.GetConfigOrDie()

	if bootstrapFlag {
		if err := bootstrap(context.Background(), cfg); err != nil {
			log.Error(err, "bootstrap failed")
			os.Exit(1)
		}
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme.Scheme,
		HealthProbeBindAddress: healthProbeBindAddress,
		Metrics:                server.Options{BindAddress: metricsBindAddress},
		Controller: config.Controller{
			SkipNameValidation: ptr.To(true),
		},
	})
	if err != nil {
		log.Error(err, "creating manager")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "setting up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "setting up readyz check")
		os.Exit(1)
	}

	shutdown, caches, err := graphcontroller.SetupWithManager(mgr, cfg, maxWorkers, driftInterval)
	if err != nil {
		log.Error(err, "setting up controller")
		os.Exit(1)
	}

	// Apply stdlib after the controller starts. The Runnable runs after
	// caches sync. Resources with unknown CRDs fail and are retried.
	if bootstrapFlag {
		if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			return stdlib.Apply(ctx, ctrl.Log.WithName("stdlib"), cfg)
		})); err != nil {
			log.Error(err, "registering stdlib")
			os.Exit(1)
		}
	}

	if pprofBindAddress != "" {
		http.HandleFunc("/debug/cachestats", func(w http.ResponseWriter, r *http.Request) {
			compiled, instances := caches.CacheSizes()
			fmt.Fprintf(w, "compiled=%d instances=%d\n", compiled, instances)
		})
		go func() {
			log.Info("starting pprof server", "addr", pprofBindAddress)
			if err := http.ListenAndServe(pprofBindAddress, nil); err != nil {
				log.Error(err, "pprof server failed")
			}
		}()
	}

	log.Info("starting graph controller")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "running manager")
	}
	// Cancel dynamic watch informers after the manager has fully stopped.
	// All controllers have drained by the time mgr.Start returns, so no
	// reconcile can race with informer cancellation.
	shutdown()
}
