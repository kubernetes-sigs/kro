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
	"os"

	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	graphcontroller "github.com/kubernetes-sigs/kro/experimental/controller"
)

func main() {
	var (
		bootstrapFlag          bool
		healthProbeBindAddress string
		metricsBindAddress     string
		maxWorkers             int
	)

	flag.BoolVar(&bootstrapFlag, "bootstrap", false, "Install CRDs before starting the controller")
	flag.StringVar(&healthProbeBindAddress, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to. Use :0 for a random port.")
	flag.StringVar(&metricsBindAddress, "metrics-bind-address", "0", "The address the metrics endpoint binds to. Use 0 to disable.")
	flag.IntVar(&maxWorkers, "max-workers", 0, "Maximum concurrent reconcile workers. 0 uses the default.")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("main")

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
			SkipNameValidation: ptr(true),
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

	shutdown, err := graphcontroller.SetupWithManager(mgr, cfg, maxWorkers)
	if err != nil {
		log.Error(err, "setting up controller")
		os.Exit(1)
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
