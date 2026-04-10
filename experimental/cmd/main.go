// Binary entrypoint for the experimental Graph controller.
//
// Run with --bootstrap to automatically install CRDs:
//
//	go run ./experimental/cmd/ --bootstrap
//
// Or install CRDs manually first:
//
//	kubectl apply -f experimental/crds/
//	go run ./experimental/cmd/
package main

import (
	"context"
	"flag"
	"os"

	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	graphcontroller "github.com/kubernetes-sigs/kro/experimental/controller"
)

func main() {
	bootstrapFlag := flag.Bool("bootstrap", false, "Install CRDs before starting the controller")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("main")

	cfg := ctrl.GetConfigOrDie()

	if *bootstrapFlag {
		if err := bootstrap(context.Background(), cfg); err != nil {
			log.Error(err, "bootstrap failed")
			os.Exit(1)
		}
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
	})
	if err != nil {
		log.Error(err, "creating manager")
		os.Exit(1)
	}

	shutdown, err := graphcontroller.SetupWithManager(mgr, cfg)
	if err != nil {
		log.Error(err, "setting up controller")
		os.Exit(1)
	}
	defer shutdown()

	log.Info("starting graph controller")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "running manager")
		os.Exit(1)
	}
}
