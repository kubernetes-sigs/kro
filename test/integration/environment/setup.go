// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.
package environment

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	krov1alpha1 "github.com/kro-run/kro/api/v1alpha1"
	kroclient "github.com/kro-run/kro/pkg/client"
	ctrlinstance "github.com/kro-run/kro/pkg/controller/instance"
	ctrlresourcegraphdefinition "github.com/kro-run/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kro-run/kro/pkg/dynamiccontroller"
	"github.com/kro-run/kro/pkg/graph"
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
}

type ControllerConfig struct {
	AllowCRDDeletion bool
	ReconcileConfig  ctrlinstance.ReconcileConfig
}

func New(controllerConfig ControllerConfig) (*Environment, error) {
	env := &Environment{
		ControllerConfig: controllerConfig,
	}

	// Setup logging
	logf.SetLogger(zap.New(zap.WriteTo(io.Discard), zap.UseDevMode(true)))
	env.context, env.cancel = context.WithCancel(context.Background())

	env.TestEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			// resourcegraphdefinition CRD
			filepath.Join("../../../..", "config", "crd", "bases"),
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

	e.CRDManager = e.ClientSet.CRD(kroclient.CRDWrapperConfig{
		Log: noopLogger(),
	})

	restConfig := e.ClientSet.RESTConfig()
	e.GraphBuilder, err = graph.NewBuilder(restConfig)
	if err != nil {
		return fmt.Errorf("creating graph builder: %w", err)
	}

	return nil
}

func (e *Environment) setupController() error {
	dc := dynamiccontroller.NewDynamicController(
		noopLogger(),
		dynamiccontroller.Config{
			Workers:         3,
			ResyncPeriod:    60 * time.Second,
			QueueMaxRetries: 20,
			ShutdownTimeout: 60 * time.Second,
		},
		e.ClientSet.Dynamic(),
	)
	go func() {
		err := dc.Run(e.context)
		if err != nil {
			panic(fmt.Sprintf("failed to run dynamic controller: %v", err))
		}
	}()

	rgReconciler := ctrlresourcegraphdefinition.NewResourceGraphDefinitionReconciler(
		noopLogger(),
		e.Client,
		e.ClientSet,
		e.ControllerConfig.AllowCRDDeletion,
		dc,
		e.GraphBuilder,
	)

	var err error
	e.CtrlManager, err = ctrl.NewManager(e.ClientSet.RESTConfig(), ctrl.Options{
		Scheme: scheme.Scheme,
		Metrics: server.Options{
			// Disable the metrics server
			BindAddress: "0",
		},
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	if err = rgReconciler.SetupWithManager(e.CtrlManager); err != nil {
		return fmt.Errorf("setting up reconciler: %w", err)
	}

	go func() {
		if err := e.CtrlManager.Start(e.context); err != nil {
			panic(fmt.Sprintf("failed to start manager: %v", err))
		}
	}()

	return nil
}

func (e *Environment) Stop() error {
	e.cancel()
	time.Sleep(1 * time.Second)
	return e.TestEnv.Stop()
}

func noopLogger() logr.Logger {
	// route all logs to a file
	/* fileName := "test-integration.log"
	file, err := os.OpenFile(fileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		panic(fmt.Sprintf("failed to open log file: %v", err))
	} */

	logger := zap.New(zap.UseFlagOptions(&zap.Options{
		DestWriter:  io.Discard,
		Development: true,
	}))

	return logger
}
