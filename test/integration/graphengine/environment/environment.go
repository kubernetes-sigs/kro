// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package environment owns the envtest harness shared by every
// integration suite. Each test file under test/integration/... uses
// Start(t) to spin up a control plane with the Graph CRD installed,
// the controller wired through the dynamic controller, and returns a
// fully wired Env value that supports K8s client access, Graph CRUD
// helpers, and an Eventually poller.
package environment

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	graphcontroller "github.com/kubernetes-sigs/kro/pkg/graphengine/controller/graph"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/schemawatcher"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// Env bundles every handle a suite needs: the envtest control plane,
// the controller-runtime manager (already running), and a typed client.
// Stop is registered automatically as a t.Cleanup at Start time so suites
// don't have to manage lifecycle themselves.
type Env struct {
	Ctx           context.Context
	Cancel        context.CancelFunc
	Cfg           *rest.Config
	TestEnv       *envtest.Environment
	Mgr           ctrl.Manager
	Client        client.Client
	Meta          metadata.Interface
	Router        *watchrouter.Router
	SchemaWatcher *schemawatcher.SchemaWatcher

	cancelMgr context.CancelFunc
	mgrDone   chan struct{}
}

// Shared returns the package-singleton Env, booting envtest on the
// first call. Cleanup is the suite's TestMain responsibility — call
// StopShared after RunSpecs/m.Run to tear down. Repeated calls return
// the cached env; the *testing.T parameter is only used to surface
// boot errors.
func Shared(t *testing.T) *Env {
	t.Helper()
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if shared != nil {
		return shared
	}
	if sharedErr != nil {
		t.Fatalf("shared env previously failed to boot: %v", sharedErr)
	}
	e, err := boot()
	if err != nil {
		sharedErr = err
		t.Fatalf("boot shared env: %v", err)
	}
	shared = e
	return shared
}

// StopShared tears down the singleton Env. Suites call this from
// TestMain after m.Run() so the control plane goes away exactly once.
// Safe to call when no env was started.
func StopShared() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if shared != nil {
		shared.Stop()
		shared = nil
	}
}

var (
	sharedMu  sync.Mutex
	shared    *Env
	sharedErr error
)

// Start brings up envtest, installs the Graph CRD, builds the manager,
// wires the reconciler + dynamic controller, and starts the manager in a
// background goroutine. Returns once the manager has begun.
//
// On t.Cleanup the manager is canceled and envtest is torn down. Use
// only when isolation from other tests matters; otherwise prefer
// Shared.
func Start(t *testing.T) *Env {
	t.Helper()
	e, err := boot()
	if err != nil {
		t.Fatalf("boot env: %v", err)
	}
	t.Cleanup(e.Stop)
	return e
}

// boot does the heavy lifting — envtest, manager, controller. Returns
// without registering any t.Cleanup so callers can pick lifecycle
// policy (per-test vs. shared).
func boot() (*Env, error) {
	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	_, thisFile, _, _ := runtime.Caller(0)
	// thisFile is test/integration/graphengine/environment/environment.go;
	// four levels up is the repo root, where helm/crds holds the generated
	// CRDs (including kro.run_graphs.yaml).
	crdDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", "helm", "crds")

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:       []string{crdDir},
		ErrorIfCRDPathMissing:   true,
		ControlPlaneStopTimeout: 30 * time.Second,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		return nil, fmt.Errorf("start envtest: %w", err)
	}

	scheme := clientgoscheme.Scheme
	utilruntime.Must(expv1alpha1.AddToScheme(scheme))
	// CRD informer (schema watcher) needs apiextensions types registered.
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))

	ctx, cancel := context.WithCancel(context.Background())

	skipName := true
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 server.Options{BindAddress: "0"},
		HealthProbeBindAddress:  "0",
		LeaderElection:          false,
		GracefulShutdownTimeout: ptrDuration(10 * time.Second),
		Controller: config.Controller{
			SkipNameValidation: &skipName,
		},
	})
	if err != nil {
		_ = testEnv.Stop()
		cancel()
		return nil, fmt.Errorf("build manager: %w", err)
	}

	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		_ = testEnv.Stop()
		cancel()
		return nil, fmt.Errorf("build client: %w", err)
	}
	metaClient, err := metadata.NewForConfig(cfg)
	if err != nil {
		_ = testEnv.Stop()
		cancel()
		return nil, fmt.Errorf("build metadata client: %w", err)
	}

	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		_ = testEnv.Stop()
		cancel()
		return nil, fmt.Errorf("build http client: %w", err)
	}
	cmp, err := compiler.NewCompiler(cfg, httpClient)
	if err != nil {
		_ = testEnv.Stop()
		cancel()
		return nil, fmt.Errorf("build compiler: %w", err)
	}

	dc := watchrouter.NewRouter(ctrl.Log.WithName("dc"),
		watchrouter.Config{ResyncPeriod: 0}, metaClient)
	if err := mgr.Add(dc); err != nil {
		_ = testEnv.Stop()
		cancel()
		return nil, fmt.Errorf("add dc to manager: %w", err)
	}

	reg := registry.New()
	sw := schemawatcher.New(ctrl.Log.WithName("sw"), schemawatcher.Config{
		Cache:   mgr.GetCache(),
		Graphs:  reg,
		Schemas: cmp,
	})
	if err := mgr.Add(sw); err != nil {
		_ = testEnv.Stop()
		cancel()
		return nil, fmt.Errorf("add schema watcher to manager: %w", err)
	}

	reconciler := &graphcontroller.Reconciler{
		Client:        mgr.GetClient(),
		Compiler:      cmp,
		Registry:      reg,
		Executor:      executor.NewSimple(mgr.GetClient()),
		Router:        dc,
		SchemaWatcher: sw,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		_ = testEnv.Stop()
		cancel()
		return nil, fmt.Errorf("setup reconciler: %w", err)
	}

	mgrCtx, cancelMgr := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mgr.Start(mgrCtx)
	}()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		cancelMgr()
		<-done
		_ = testEnv.Stop()
		cancel()
		return nil, fmt.Errorf("manager cache sync timed out")
	}

	return &Env{
		Ctx:           ctx,
		Cancel:        cancel,
		Cfg:           cfg,
		TestEnv:       testEnv,
		Mgr:           mgr,
		Client:        cl,
		Meta:          metaClient,
		Router:        dc,
		SchemaWatcher: sw,
		cancelMgr:     cancelMgr,
		mgrDone:       done,
	}, nil
}

// Stop tears the manager down then stops envtest. Safe to call twice.
func (e *Env) Stop() {
	if e.cancelMgr != nil {
		e.cancelMgr()
		<-e.mgrDone
		e.cancelMgr = nil
	}
	if e.Cancel != nil {
		e.Cancel()
		e.Cancel = nil
	}
	if e.TestEnv != nil {
		_ = e.TestEnv.Stop()
		e.TestEnv = nil
	}
}

// Eventually polls fn until it returns nil or the timeout fires. The
// returned error contains the last fn error so failure messages stay
// informative.
func Eventually(t *testing.T, timeout, interval time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for {
		last = fn()
		if last == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Eventually timed out after %s: %v", timeout, last)
		}
		time.Sleep(interval)
	}
}

// Consistently asserts fn keeps returning nil for the duration window.
// Used to verify a state holds (e.g. nothing got recreated) rather than
// converges to it.
func Consistently(t *testing.T, duration, interval time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		if err := fn(); err != nil {
			t.Fatalf("Consistently violation: %v", err)
		}
		time.Sleep(interval)
	}
}

// CreateNamespace creates a fresh namespace named for the test and
// returns its name. The namespace is dropped on cleanup so suites
// don't have to coordinate names.
func (e *Env) CreateNamespace(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("krcdl-%d", time.Now().UnixNano())
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := e.Client.Create(e.Ctx, ns); err != nil {
		t.Fatalf("create namespace %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = e.Client.Delete(context.Background(), ns)
	})
	return name
}

func ptrDuration(d time.Duration) *time.Duration { return &d }
