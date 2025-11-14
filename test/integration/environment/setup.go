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
	"time"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
	ctrlinstance "github.com/kubernetes-sigs/kro/pkg/controller/instance"
	ctrlresourcegraphdefinition "github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller"
	"github.com/kubernetes-sigs/kro/pkg/graph"
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
	ManagerResult    chan error
}

type ControllerConfig struct {
	AllowCRDDeletion bool
	ReconcileConfig  ctrlinstance.ReconcileConfig
	LogWriter        io.Writer
}

func New(ctx context.Context, controllerConfig ControllerConfig) (*Environment, error) {
	env := &Environment{
		ControllerConfig: controllerConfig,
	}

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

	// Start the test environment
	cfg, err := env.TestEnv.Start()
	if err != nil {
		return nil, fmt.Errorf("starting test environment: %w", err)
	}

	clientSet, err := kroclient.NewSet(kroclient.Config{
		RestConfig: cfg,
	})
	if err != nil {
		return nil, fmt.Errorf("creating client set: %w", err)
	}
	env.ClientSet = clientSet

	// Setup scheme
	if err := krov1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return nil, fmt.Errorf("adding kro scheme: %w", err)
	}

	// Initialize clients
	if err := env.initializeClients(); err != nil {
		return nil, fmt.Errorf("initializing clients: %w", err)
	}

	// Setup and start controller
	if err := env.setupController(); err != nil {
		return nil, fmt.Errorf("setting up controller: %w", err)
	}

	time.Sleep(1 * time.Second)
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
	e.CtrlManager, err = ctrl.NewManager(e.ClientSet.RESTConfig(), ctrl.Options{
		Scheme: scheme.Scheme,
		Metrics: server.Options{
			// Disable the metrics server
			BindAddress: "0",
		},
		GracefulShutdownTimeout: ptr.To(30 * time.Second),
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}
	e.ClientSet.SetRESTMapper(e.CtrlManager.GetRESTMapper())

	dc := dynamiccontroller.NewDynamicController(
		zap.New(zap.WriteTo(e.ControllerConfig.LogWriter), zap.UseDevMode(true)),
		dynamiccontroller.Config{
			Workers:         3,
			ResyncPeriod:    0, // disabled resync
			QueueMaxRetries: 20,
			MinRetryDelay:   200 * time.Millisecond,
			MaxRetryDelay:   1000 * time.Second,
			RateLimit:       10,
			BurstLimit:      100,
		},
		e.ClientSet.Metadata(), e.ClientSet.RESTMapper())

	rgReconciler := ctrlresourcegraphdefinition.NewResourceGraphDefinitionReconciler(
		e.ClientSet,
		e.ControllerConfig.AllowCRDDeletion,
		dc,
		e.GraphBuilder,
		1,
	)

	if err := e.CtrlManager.Add(dc); err != nil {
		return fmt.Errorf("adding dynamic controller to manager: %w", err)
	}

	if err = rgReconciler.SetupWithManager(e.CtrlManager); err != nil {
		return fmt.Errorf("setting up reconciler: %w", err)
	}

	e.ManagerResult = make(chan error, 1)
	go func() {
		e.ManagerResult <- e.CtrlManager.Start(e.context)
	}()

	return nil
}

func (e *Environment) Stop() error {
	e.cancel()
	time.Sleep(1 * time.Second)
	return errors.Join(e.TestEnv.Stop(), <-e.ManagerResult)
}
