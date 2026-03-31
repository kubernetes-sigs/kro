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
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes/scheme"
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
	ctrlgraphrevision "github.com/kubernetes-sigs/kro/pkg/controller/graphrevision"
	ctrlinstance "github.com/kubernetes-sigs/kro/pkg/controller/instance"
	ctrlresourcegraphdefinition "github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
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

	env.TestEnv.ControlPlane.GetAPIServer().Configure().Append("enable-admission-plugins", "ValidatingAdmissionPolicy")

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
	if err := internalv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		retErr = fmt.Errorf("adding internal kro scheme: %w", err)
		return nil, retErr
	}
	if err := krov1alpha1.AddToScheme(scheme.Scheme); err != nil {
		retErr = fmt.Errorf("adding kro scheme: %w", err)
		return nil, retErr
	}

	// Initialize clients
	if err := env.initializeClients(); err != nil {
		retErr = fmt.Errorf("initializing clients: %w", err)
		return nil, retErr
	}

	// Setup and start controller
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

func (e *Environment) setupController() error {
	var err error
	rgdConfig := graph.RGDConfig{
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
			ProgressRequeueDelay:    3 * time.Second,
			MaxConcurrentReconciles: 40,
			MaxGraphRevisions:       maxGraphRevisions,
			RGDConfig:               rgdConfig,
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
	if err = gvReconciler.SetupWithManager(e.CtrlManager); err != nil {
		return fmt.Errorf("setting up graph revision reconciler: %w", err)
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
