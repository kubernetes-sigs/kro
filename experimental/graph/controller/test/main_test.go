package graphcontroller_test

import (
	"context"
	"testing"

	"k8s.io/client-go/kubernetes/scheme"
	k8smetadata "k8s.io/client-go/metadata"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	graphcontroller "github.com/kubernetes-sigs/kro/docs/design/graph/controller"
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

	graphCRD := buildGraphCRD()
	if err := k8sClient.Create(ctx, graphCRD); err != nil {
		panic("creating Graph CRD: " + err.Error())
	}

	if err := waitForCRD(ctx, k8sClient, "graphs.kro.run"); err != nil {
		panic("waiting for Graph CRD: " + err.Error())
	}

	metadataClient, err := k8smetadata.NewForConfig(cfg)
	if err != nil {
		panic("creating metadata client: " + err.Error())
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

	testSetup := graphcontroller.SetupWithManagerForTest(mgr, metadataClient)

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

	testSetup.Shutdown()
	cancel()
	if err := testEnv.Stop(); err != nil {
		panic("stopping envtest: " + err.Error())
	}

	if code != 0 {
		panic("tests failed")
	}
}
