// Copyright 2025 The Kubernetes Authors.
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

package environment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
	ctrlgraph "github.com/kubernetes-sigs/kro/pkg/controller/graph"
	ctrlgraphrevision "github.com/kubernetes-sigs/kro/pkg/controller/graphrevision"
	ctrlinstance "github.com/kubernetes-sigs/kro/pkg/controller/instance"
	ctrlresourcegraphdefinition "github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/schemawatcher"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

type Environment struct {
	context context.Context
	cancel  context.CancelFunc

	ControllerConfig ControllerConfig
	Client           client.Client
	TestEnv          *envtest.Environment
	CtrlManager      ctrl.Manager
	ClientSet        *kroclient.Set
	CRDManager       kroclient.CRDClient
	GraphBuilder     *graph.Builder
	Router           *watchrouter.Router
	SchemaWatcher    *schemawatcher.SchemaWatcher
	managerReady     <-chan struct{}
	managerDone      chan struct{}
	managerErrMu     sync.RWMutex
	managerErr       error
}

type ControllerConfig struct {
	AllowCRDDeletion  bool
	ReconcileConfig   ctrlinstance.ReconcileConfig
	MaxGraphRevisions int
	LogWriter         io.Writer
}

// init installs a no-op apiserver warning handler for every client-go client
// built in this test process, silencing benign warning headers (e.g. "child
// pods are preserved by default when jobs are deleted; set
// propagationPolicy=Background ...") that otherwise clutter spec output. It
// affects only the integration test binary, never production clients.
func init() {
	rest.SetDefaultWarningHandler(rest.NoWarnings{})
}

func New(ctx context.Context, controllerConfig ControllerConfig) (_ *Environment, retErr error) {
	env := &Environment{
		ControllerConfig: controllerConfig,
	}
	defer func() {
		if retErr == nil {
			return
		}
		if cleanupErr := env.Stop(); cleanupErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()

	if env.ControllerConfig.LogWriter == nil {
		env.ControllerConfig.LogWriter = io.Discard
	}

	// Setup logging
	logf.SetLogger(zap.New(zap.WriteTo(env.ControllerConfig.LogWriter), zap.UseDevMode(true)))
	env.context, env.cancel = context.WithCancel(ctx)

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if out, err := exec.Command("setup-envtest", "use", "-p", "path").Output(); err == nil && len(bytes.TrimSpace(out)) > 0 {
			_ = os.Setenv("KUBEBUILDER_ASSETS", string(bytes.TrimSpace(out)))
		} else if out, err := exec.Command(filepath.Join("..", "..", "..", "..", "bin", "setup-envtest"), "use", "-p", "path").Output(); err == nil && len(bytes.TrimSpace(out)) > 0 {
			_ = os.Setenv("KUBEBUILDER_ASSETS", string(bytes.TrimSpace(out)))
		}
	}

	env.TestEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			// resourcegraphdefinition CRD
			filepath.Join("../../../..", "helm", "crds"),
			// ACK ec2 CRDs
			filepath.Join("../..", "crds", "ack-ec2-controller"),
			// ACK iam CRDs
			filepath.Join("../..", "crds", "ack-iam-controller"),
			// ACK eks CRDs
			filepath.Join("../..", "crds", "ack-eks-controller"),
		},
		ErrorIfCRDPathMissing:   true,
		ControlPlaneStopTimeout: 2 * time.Minute,
	}

	apiServer := env.TestEnv.ControlPlane.GetAPIServer().Configure()
	apiServer.Append("enable-admission-plugins", "ValidatingAdmissionPolicy")
	// The integration suites drive a single shared apiserver from many parallel
	// Ginkgo processes. Disable API Priority & Fairness and raise the in-flight
	// request ceilings so bursts of watches/lists are not throttled into 429s,
	// which otherwise surface as flaky Eventually timeouts under load.
	apiServer.Set("enable-priority-and-fairness", "false")
	apiServer.Set("max-requests-inflight", "800")
	apiServer.Set("max-mutating-requests-inflight", "400")

	// Start the test environment
	cfg, err := env.TestEnv.Start()
	if err != nil {
		retErr = fmt.Errorf("starting test environment: %w", err)
		return nil, retErr
	}

	clientSet, err := kroclient.NewSet(kroclient.Config{
		RestConfig: cfg,
	})
	if err != nil {
		retErr = fmt.Errorf("creating client set: %w", err)
		return nil, retErr
	}
	env.ClientSet = clientSet

	// Setup scheme
	if err := registerSchemes(); err != nil {
		retErr = err
		return nil, retErr
	}

	// Initialize clients
	if err := env.initializeClients(); err != nil {
		retErr = fmt.Errorf("initializing clients: %w", err)
		return nil, retErr
	}

	// Setup and start controller
	if err := env.grantImpersonatedServiceAccounts(); err != nil {
		retErr = fmt.Errorf("granting impersonated service accounts: %w", err)
		return nil, retErr
	}
	if err := env.setupController(); err != nil {
		retErr = fmt.Errorf("setting up controller: %w", err)
		return nil, retErr
	}

	if err := env.waitForManagerReady(); err != nil {
		retErr = fmt.Errorf("waiting for manager readiness: %w", err)
		return nil, retErr
	}
	return env, nil
}

// NewShared builds a thin Environment that connects to an already-running
// control plane (started by another process, e.g. Ginkgo parallel process #1).
//
// Unlike New, it does NOT start an envtest control plane or a controller
// manager: the process that called New owns those. NewShared only wires up the
// client set, typed client, CRD manager and graph builder against the shared
// rest.Config so that specs running on secondary processes can create and
// observe objects on the single shared apiserver.
func NewShared(ctx context.Context, cfg *rest.Config) (_ *Environment, retErr error) {
	env := &Environment{}
	defer func() {
		if retErr == nil {
			return
		}
		if cleanupErr := env.Stop(); cleanupErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()

	env.ControllerConfig.LogWriter = io.Discard
	env.context, env.cancel = context.WithCancel(ctx)

	clientSet, err := kroclient.NewSet(kroclient.Config{RestConfig: cfg})
	if err != nil {
		retErr = fmt.Errorf("creating client set: %w", err)
		return nil, retErr
	}
	env.ClientSet = clientSet

	if err := registerSchemes(); err != nil {
		retErr = err
		return nil, retErr
	}

	if err := env.initializeClients(); err != nil {
		retErr = fmt.Errorf("initializing clients: %w", err)
		return nil, retErr
	}

	return env, nil
}

// registerSchemes adds the kro API types to the global scheme. It is safe to
// call more than once; AddToScheme is idempotent.
func registerSchemes() error {
	if err := internalv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("adding internal kro scheme: %w", err)
	}
	if err := krov1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("adding kro scheme: %w", err)
	}
	return nil
}

func (e *Environment) initializeClients() error {
	var err error

	e.Client, err = client.New(e.ClientSet.RESTConfig(), client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("creating client: %w", err)
	}

	e.CRDManager = e.ClientSet.CRD(kroclient.CRDWrapperConfig{})

	restConfig := e.ClientSet.RESTConfig()
	e.GraphBuilder, err = graph.NewBuilder(restConfig, e.ClientSet.HTTPClient())
	if err != nil {
		return fmt.Errorf("creating graph builder: %w", err)
	}

	return nil
}

// grantImpersonatedServiceAccounts binds cluster-admin to every ServiceAccount
// (the system:serviceaccounts group) on the shared envtest control plane. That
// control plane runs with authorization-mode=RBAC (the controller-runtime
// default), so without this every Graph applied under its impersonated
// ServiceAccount would be denied and every existing Graph suite would break.
// This grant makes each impersonated SA effectively-allow — reproducing the
// permissive behavior the suites assume WITHOUT changing the apiserver's
// authorization mode — so the real impersonated apply/watch/teardown path is
// exercised while Graph behavior stays unchanged. RBAC-confinement proof lives
// in its own dedicated envtest, not here.
func (e *Environment) grantImpersonatedServiceAccounts() error {
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "kro-integration-impersonated-sa-admin"},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     "cluster-admin",
		},
		Subjects: []rbacv1.Subject{{
			Kind:     rbacv1.GroupKind,
			APIGroup: rbacv1.GroupName,
			Name:     "system:serviceaccounts",
		}},
	}
	if err := e.Client.Create(e.context, crb); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (e *Environment) setupController() error {
	var err error
	rgdConfig := graph.Config{
		MaxCollectionSize:          1000,
		MaxCollectionDimensionSize: 10,
	}
	maxGraphRevisions := e.ControllerConfig.MaxGraphRevisions
	if maxGraphRevisions <= 0 {
		maxGraphRevisions = 20
	}

	e.CtrlManager, err = ctrl.NewManager(e.ClientSet.RESTConfig(), ctrl.Options{
		Scheme: scheme.Scheme,
		Controller: config.Controller{
			SkipNameValidation: new(true),
		},
		Metrics: server.Options{
			// Disable the metrics server
			BindAddress: "0",
		},
		GracefulShutdownTimeout: new(30 * time.Second),
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}
	e.ClientSet.SetRESTMapper(e.CtrlManager.GetRESTMapper())

	dc := dynamiccontroller.NewDynamicController(
		zap.New(zap.WriteTo(e.ControllerConfig.LogWriter), zap.UseDevMode(true)),
		dynamiccontroller.Config{
			Workers:         40,
			ResyncPeriod:    0, // disabled resync
			QueueMaxRetries: 20,
			MinRetryDelay:   200 * time.Millisecond,
			MaxRetryDelay:   1000 * time.Second,
			RateLimit:       10,
			BurstLimit:      100,
		},
		e.ClientSet.Metadata(), e.ClientSet.RESTMapper())

	graphRevisionRegistry := revisions.NewRegistry()
	rgReconciler := ctrlresourcegraphdefinition.NewResourceGraphDefinitionReconciler(
		e.ClientSet,
		dc,
		e.GraphBuilder,
		graphRevisionRegistry,
		ctrlresourcegraphdefinition.Config{
			AllowCRDDeletion:        e.ControllerConfig.AllowCRDDeletion,
			InstanceRequeueInterval: e.ControllerConfig.ReconcileConfig.DefaultRequeueDuration,
			ProgressRequeueDelay:    1 * time.Second,
			MaxConcurrentReconciles: 40,
			MaxGraphRevisions:       maxGraphRevisions,
			RGDConfig:               rgdConfig,
			ApplyConcurrency:        e.ControllerConfig.ReconcileConfig.ApplyConcurrency,
		},
	)
	gvReconciler := ctrlgraphrevision.NewGraphRevisionReconciler(
		e.GraphBuilder,
		graphRevisionRegistry,
		10,
		rgdConfig,
	)

	if err := e.CtrlManager.Add(dc); err != nil {
		return fmt.Errorf("adding dynamic controller to manager: %w", err)
	}

	if err = rgReconciler.SetupWithManager(e.CtrlManager); err != nil {
		return fmt.Errorf("setting up reconciler: %w", err)
	}
	// Inject the graph-engine compiler so that micro-controllers route
	// instance reconciliation through the Graph engine.
	geCmp, err := compiler.NewCompiler(e.ClientSet.RESTConfig(), e.ClientSet.HTTPClient())
	if err != nil {
		return fmt.Errorf("building graph-engine compiler: %w", err)
	}
	rgReconciler.WithGraphEngineCompiler(geCmp)
	if err = gvReconciler.SetupWithManager(e.CtrlManager); err != nil {
		return fmt.Errorf("setting up graph revision reconciler: %w", err)
	}

	if features.FeatureGate.Enabled(features.GraphKind) {
		router := watchrouter.NewRouter(
			zap.New(zap.WriteTo(e.ControllerConfig.LogWriter), zap.UseDevMode(true)).WithName("graph-watch-router"),
			watchrouter.Config{},
			e.ClientSet.Metadata(),
		)
		if err := e.CtrlManager.Add(router); err != nil {
			return fmt.Errorf("adding graph watch router to manager: %w", err)
		}
		e.Router = router

		reg := registry.New()
		sw := schemawatcher.New(
			zap.New(zap.WriteTo(e.ControllerConfig.LogWriter), zap.UseDevMode(true)).WithName("graph-schema-watcher"),
			schemawatcher.Config{
				Cache:   e.CtrlManager.GetCache(),
				Graphs:  reg,
				Schemas: geCmp,
			},
		)
		if err := e.CtrlManager.Add(sw); err != nil {
			return fmt.Errorf("adding graph schema watcher to manager: %w", err)
		}
		e.SchemaWatcher = sw

		exec := executor.NewSimple(e.CtrlManager.GetClient())
		exec.ApplyConcurrency = e.ControllerConfig.ReconcileConfig.ApplyConcurrency
		// Match production wiring (cmd/controller/graphengine.go): standalone
		// Graph objects carry no ApplySet part-of label, so per-Graph
		// field-manager conflict detection is what stops two Graphs that template
		// the same object from flip-flopping its fields.
		exec.ConflictDetection = true

		// Mirror production (cmd/controller/graphengine.go): a namespaced Graph
		// applies its resources while impersonating a ServiceAccount in the
		// Graph's namespace. Wiring this here exercises the real impersonated
		// client construction, the SelfSubjectAccessReview CanWatch gate, per-SA
		// executor caching, and teardown-under-impersonation. The shared envtest
		// apiserver enforces RBAC (controller-runtime's default), so the
		// impersonated SAs are granted cluster-admin up front (see
		// grantImpersonatedServiceAccounts) — that keeps Graph behavior unchanged
		// (the SA can do everything) while still running the impersonated path.
		// RBAC-confinement is proven separately in a dedicated envtest.
		baseCfg := e.CtrlManager.GetConfig()
		mapper := e.CtrlManager.GetRESTMapper()
		impersonation := ctrlgraph.NewImpersonation(exec, func(user string) (client.Client, error) {
			cfg := rest.CopyConfig(baseCfg)
			cfg.Impersonate = rest.ImpersonationConfig{UserName: user}
			return client.New(cfg, client.Options{Mapper: mapper})
		}, func(user string) (authorizationv1client.AuthorizationV1Interface, error) {
			cfg := rest.CopyConfig(baseCfg)
			cfg.Impersonate = rest.ImpersonationConfig{UserName: user}
			cs, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				return nil, err
			}
			return cs.AuthorizationV1(), nil
		})

		graphReconciler := &ctrlgraph.Reconciler{
			Client:                  e.CtrlManager.GetClient(),
			Compiler:                geCmp,
			Registry:                reg,
			Executor:                exec,
			Router:                  router,
			SchemaWatcher:           sw,
			MaxConcurrentReconciles: 40,
			MaxCollectionSize:       1000,
			Impersonation:           impersonation,
			RequireImpersonation:    true,
		}
		if err := graphReconciler.SetupWithManager(e.CtrlManager); err != nil {
			return fmt.Errorf("setting up graph reconciler: %w", err)
		}
	}

	e.managerReady = e.CtrlManager.Elected()
	e.managerErrMu.Lock()
	e.managerErr = nil
	e.managerErrMu.Unlock()
	e.managerDone = make(chan struct{})
	go func() {
		err := e.CtrlManager.Start(e.context)
		e.managerErrMu.Lock()
		e.managerErr = err
		e.managerErrMu.Unlock()
		close(e.managerDone)
	}()

	return nil
}

func (e *Environment) Context() context.Context {
	if e == nil || e.context == nil {
		return context.Background()
	}
	return e.context
}

func (e *Environment) RestartControllers() error {
	e.cancel()
	if err := e.waitForManagerStop(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("stopping manager: %w", err)
	}

	e.context, e.cancel = context.WithCancel(context.Background())
	if err := e.setupController(); err != nil {
		return fmt.Errorf("restarting controller: %w", err)
	}

	return e.waitForManagerReady()
}

// waitForManagerReady blocks until the controller-runtime manager has synced
// its caches and is ready to serve. Replaces hard time.Sleep with the
// structural signal provided by Manager.Elected().
func (e *Environment) waitForManagerReady() error {
	select {
	case <-e.managerReady:
		return nil
	case <-e.managerDone:
		err := e.currentManagerErr()
		return fmt.Errorf("manager exited before becoming ready: %w", err)
	case <-time.After(30 * time.Second):
		return fmt.Errorf("timed out waiting for manager readiness")
	}
}

func (e *Environment) Stop() error {
	if e == nil {
		return nil
	}
	if e.cancel != nil {
		e.cancel()
	}

	var stopErr error
	if e.TestEnv != nil {
		stopErr = e.TestEnv.Stop()
	}

	managerErr := e.waitForManagerStop()
	if errors.Is(managerErr, context.Canceled) {
		managerErr = nil
	}

	return errors.Join(stopErr, managerErr)
}

func (e *Environment) waitForManagerStop() error {
	if e == nil || e.managerDone == nil {
		return nil
	}
	<-e.managerDone
	return e.currentManagerErr()
}

func (e *Environment) currentManagerErr() error {
	e.managerErrMu.RLock()
	defer e.managerErrMu.RUnlock()
	return e.managerErr
}
