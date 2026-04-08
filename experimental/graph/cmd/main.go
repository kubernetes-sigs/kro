// Binary entrypoint for the experimental Graph controller.
//
// Prerequisites:
//
//	kubectl apply -f experimental/graph/crds/
//
// Run:
//
//	go run ./experimental/graph/cmd/
package main

import (
	"os"

	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	graphcontroller "github.com/kubernetes-sigs/kro/experimental/graph/controller"
)

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("main")

	cfg := ctrl.GetConfigOrDie()

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
