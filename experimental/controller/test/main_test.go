package graphcontroller_test

import (
	"context"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	graphcontroller "github.com/kubernetes-sigs/kro/experimental/controller"
)

var (
	testEnv   *envtest.Environment
	k8sClient client.Client
	ctx       context.Context
	cancel    context.CancelFunc
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(true)))
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

	testEnv = &envtest.Environment{}

	cfg, err := testEnv.Start()
	if err != nil {
		panic("starting envtest: " + err.Error())
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		panic("creating client: " + err.Error())
	}

	// Pre-install all CRDs upfront. This eliminates per-test CRD creation
	// latency — CRD establishment takes 3-5s each, and several tests need
	// the same CRDs. Install them once at startup.
	crds := []struct {
		name    string
		builder func() *apiextensionsv1.CustomResourceDefinition
	}{
		{"graphs.kro.run", buildGraphCRD},
		{"graphrevisions.internal.kro.run", buildGraphRevisionCRD},
		{"resourcegraphdefinitions.test.kro.run", buildRGDCRD},
		{"simpleapps.test.kro.run", buildSimpleAppCRD},
	}
	for _, crd := range crds {
		if err := k8sClient.Create(ctx, crd.builder()); err != nil {
			panic("creating CRD " + crd.name + ": " + err.Error())
		}
	}
	for _, crd := range crds {
		if err := waitForCRD(ctx, k8sClient, crd.name); err != nil {
			panic("waiting for CRD " + crd.name + ": " + err.Error())
		}
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
		Controller: config.Controller{
			SkipNameValidation: ptr(true),
		},
		Metrics: server.Options{BindAddress: "0"},
	})
	if err != nil {
		panic("creating manager: " + err.Error())
	}

	// Use the same SetupWithManager that production uses.
	// Multiple workers prevent watch event starvation under parallel test load.
	shutdown, err := graphcontroller.SetupWithManager(mgr, cfg, 4)
	if err != nil {
		panic("setting up controller: " + err.Error())
	}

	k8sClient = mgr.GetClient()

	go func() {
		if err := mgr.Start(ctx); err != nil {
			panic("starting manager: " + err.Error())
		}
	}()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		panic("waiting for cache sync")
	}

	code := m.Run()

	shutdown()
	cancel()
	if err := testEnv.Stop(); err != nil {
		panic("stopping envtest: " + err.Error())
	}

	if code != 0 {
		panic("tests failed")
	}
}
