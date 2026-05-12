// Binary entrypoint for the experimental Graph controller.
//
// CRDs are installed by the Helm chart (experimental/charts/controller/).
// Stdlib Graphs are installed by the Helm chart (experimental/charts/stdlib/).
//
// For local development, apply CRDs manually first:
//
//	kubectl apply -f experimental/charts/crds/templates/
//	go run ./experimental/cmd/
package main

import (
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof" // Registers pprof handlers; server started conditionally via pprofBindAddress flag
	"os"
	"runtime/debug"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	graphcontroller "github.com/ellistarn/kro/experimental/controller"
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
		healthProbeBindAddress string
		metricsBindAddress     string
		pprofBindAddress       string
		maxWorkers             int
	)

	flag.StringVar(&healthProbeBindAddress, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to. Use :0 for a random port.")
	flag.StringVar(&metricsBindAddress, "metrics-bind-address", "0", "The address the metrics endpoint binds to. Use 0 to disable.")
	flag.StringVar(&pprofBindAddress, "pprof-bind-address", "", "The address the pprof endpoint binds to. Empty to disable.")
	flag.IntVar(&maxWorkers, "max-workers", 0, "Maximum concurrent reconcile workers (default: 16).")
	flag.Parse()

	if pprofBindAddress != "" {
		// Register pprof handlers — will start after SetupWithManager.
		http.HandleFunc("/debug/freeosmemory", func(w http.ResponseWriter, r *http.Request) {
			debug.FreeOSMemory()
			w.Write([]byte("ok\n"))
		})
	}

	cfg := ctrl.GetConfigOrDie()

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

	shutdown, caches, err := graphcontroller.SetupWithManager(mgr, cfg, maxWorkers)
	if err != nil {
		log.Error(err, "setting up controller")
		os.Exit(1)
	}

	if pprofBindAddress != "" {
		http.HandleFunc("/debug/cachestats", func(w http.ResponseWriter, r *http.Request) {
			instances := caches.CacheSizes()
			fmt.Fprintf(w, "instances=%d\n", instances)
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
