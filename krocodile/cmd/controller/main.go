// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package main

import (
	"flag"
	"net/http"
	"os"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/metadata"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	"sigs.k8s.io/krocodile/pkg/compiler"
	graphcontroller "sigs.k8s.io/krocodile/pkg/controller/graph"
	"sigs.k8s.io/krocodile/pkg/executor"
	"sigs.k8s.io/krocodile/pkg/registry"
	"sigs.k8s.io/krocodile/pkg/schemawatcher"
	"sigs.k8s.io/krocodile/pkg/watchrouter"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(expv1alpha1.AddToScheme(scheme))
	// CRD informer needs the apiextensions types registered.
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr      string
		probeAddr        string
		leaderElect      bool
		leaderElectionID string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for the controller manager.")
	flag.StringVar(&leaderElectionID, "leader-election-id", "krocodile.experimental.kro.run", "Leader election lock name.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       leaderElectionID,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	cmp, err := compiler.NewCompiler(ctrl.GetConfigOrDie(), nil)
	if err != nil {
		setupLog.Error(err, "unable to build compiler")
		os.Exit(1)
	}

	metaClient, err := metadata.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to build metadata client")
		os.Exit(1)
	}
	dc := watchrouter.NewRouter(
		ctrl.Log.WithName("dynamic-controller"),
		watchrouter.Config{},
		metaClient,
	)
	if err := mgr.Add(dc); err != nil {
		setupLog.Error(err, "unable to add dynamic controller to manager")
		os.Exit(1)
	}

	reg := registry.New()
	sw := schemawatcher.New(ctrl.Log.WithName("schema-watcher"), schemawatcher.Config{
		Cache:   mgr.GetCache(),
		Graphs:  reg,
		Schemas: cmp,
	})
	if err := mgr.Add(sw); err != nil {
		setupLog.Error(err, "unable to add schema watcher to manager")
		os.Exit(1)
	}

	if err := (&graphcontroller.Reconciler{
		Client:        mgr.GetClient(),
		Compiler:      cmp,
		Registry:      reg,
		Executor:      executor.NewSimple(mgr.GetClient()),
		Router:        dc,
		SchemaWatcher: sw,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up Graph controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", func(_ *http.Request) error { return nil }); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", func(_ *http.Request) error { return nil }); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
