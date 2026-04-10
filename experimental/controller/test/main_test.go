package graphcontroller_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

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
	// Parse flags before m.Run() so we can read test.timeout below.
	flag.Parse()
	ctx, cancel = context.WithCancel(context.Background())

	testEnv = &envtest.Environment{}

	cfg, err := testEnv.Start()
	if err != nil {
		panic("starting envtest: " + err.Error())
	}

	// envtest starts etcd and kube-apiserver as child processes. On macOS,
	// if the parent exits without calling testEnv.Stop(), these children
	// get reparented to PID 1 and run forever — there is no equivalent of
	// Linux's PR_SET_PDEATHSIG. The three mechanisms below ensure Stop is
	// called regardless of how the process exits:
	//
	//   1. defer     — panics during setup (the lines below can panic)
	//   2. signal    — SIGINT/SIGTERM (Ctrl+C)
	//   3. timeout   — fires just before Go's hard-kill test timeout
	//
	// On the normal path, Stop is called explicitly before os.Exit.
	// Duplicate Stop calls are safe (envtest checks if already stopped).

	// 1. Defer catches panics from the setup code below.
	defer func() {
		cancel()
		testEnv.Stop() //nolint:errcheck
	}()

	// 2. Signal handler catches Ctrl+C before Go's default handler
	//    (which calls os.Exit, skipping defers).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\nreceived %s, stopping envtest\n", sig)
		cancel()
		testEnv.Stop() //nolint:errcheck
		os.Exit(1)
	}()

	// 3. Go's test timeout (default 10m, -timeout flag) fires a panic from
	//    a background goroutine, which doesn't run TestMain's defers. We
	//    race our own timer to fire 5s earlier, giving us time to call Stop
	//    from a goroutine we control.
	if f := flag.Lookup("test.timeout"); f != nil {
		if d, err := time.ParseDuration(f.Value.String()); err == nil && d > 5*time.Second {
			time.AfterFunc(d-5*time.Second, func() {
				fmt.Fprintln(os.Stderr, "test timeout approaching, stopping envtest")
				cancel()
				testEnv.Stop() //nolint:errcheck
				os.Exit(1)
			})
		}
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
		{"graphs.experimental.kro.run", buildGraphCRD},
		{"graphrevisions.experimental.kro.run", buildGraphRevisionCRD},
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

	// Normal exit: clean up explicitly, then propagate the exit code.
	// os.Exit skips defers, so we must stop envtest before calling it.
	shutdown()
	cancel()
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
	}
	os.Exit(code)
}
