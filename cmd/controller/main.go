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

package main

import (
	"flag"
	"os"
	"time"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	xv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
	resourcegraphdefinitionctrl "github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(xv1alpha1.AddToScheme(scheme))
	utilruntime.Must(extv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var (
		metricsAddr                                 string
		enableLeaderElection                        bool
		leaderElectionNamespace                     string
		probeAddr                                   string
		allowCRDDeletion                            bool
		resourceGraphDefinitionConcurrentReconciles int
		dynamicControllerConcurrentReconciles       int
		// dynamic controller rate limiter parameters
		minRetryDelay time.Duration
		maxRetryDelay time.Duration
		rateLimit     int
		burstLimit    int
		// reconciler parameters
		resyncPeriod            int
		queueMaxRetries         int
		gracefulShutdownTimeout time.Duration
		// var dynamicControllerDefaultResyncPeriod int
		qps   float64
		burst int
		// kubeconfig paths for multi-cluster support
		rgdKubeconfig      string
		workloadKubeconfig string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8078", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8079", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&leaderElectionNamespace, "leader-election-namespace", "",
		"Specific namespace that the controller will utilize to manage the coordination.k8s.io/lease object for"+
			"leader election. By default it will try to use the namespace of the service account mounted"+
			" to the controller pod.")
	flag.BoolVar(&allowCRDDeletion, "allow-crd-deletion", false, "allow kro to delete CRDs")
	flag.DurationVar(&gracefulShutdownTimeout, "graceful-shutdown-timeout", 60*time.Second,
		"maximum duration to wait for the controller manager to gracefully shutdown")
	flag.IntVar(&resourceGraphDefinitionConcurrentReconciles,
		"resource-graph-definition-concurrent-reconciles", 1,
		"The number of resource graph definition reconciles to run in parallel",
	)
	flag.IntVar(&dynamicControllerConcurrentReconciles,
		"dynamic-controller-concurrent-reconciles", 1,
		"The number of dynamic controller reconciles to run in parallel",
	)

	// rate limiter parameters
	flag.DurationVar(&minRetryDelay, "dynamic-controller-rate-limiter-min-delay", 200*time.Millisecond,
		"Minimum delay for the dynamic controller rate limiter, in milliseconds.")
	flag.DurationVar(&maxRetryDelay, "dynamic-controller-rate-limiter-max-delay", 1000*time.Second,
		"Maximum delay for the dynamic controller rate limiter, in seconds.")
	flag.IntVar(&rateLimit, "dynamic-controller-rate-limiter-rate-limit", 10,
		"Rate limit to control how frequently events are allowed to happen for the dynamic controller.")
	flag.IntVar(&burstLimit, "dynamic-controller-rate-limiter-burst-limit", 100,
		"Burst size of events for the dynamic controller rate limiter.")

	// reconciler parameters
	flag.IntVar(&resyncPeriod, "dynamic-controller-default-resync-period", 36000,
		"interval at which the controller will re list resources even with no changes, in seconds.")
	flag.IntVar(&queueMaxRetries, "dynamic-controller-default-queue-max-retries", 20,
		"maximum number of retries for an item in the queue will be retried before being dropped")
	// qps and burst
	flag.Float64Var(&qps, "client-qps", 100, "The number of queries per second to allow")
	flag.IntVar(&burst, "client-burst", 150,
		"The number of requests that can be stored for processing before the server starts enforcing the QPS limit")

	// kubeconfig paths for multi-cluster support
	flag.StringVar(&rgdKubeconfig, "rgd-kubeconfig", "",
		"Path to kubeconfig for RGD resources. Uses in-cluster config if not specified.")
	flag.StringVar(&workloadKubeconfig, "workload-kubeconfig", "",
		"Path to kubeconfig for CRDs and CR instances. Uses in-cluster config if not specified.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)

	flag.Parse()

	rootLogger := zap.New(zap.UseFlagOptions(&opts))
	ctrl.SetLogger(rootLogger)

	// Manager uses in-cluster config for leader election and health probes
	set, err := kroclient.NewSet(kroclient.Config{
		QPS:   float32(qps),
		Burst: burst,
	})
	if err != nil {
		setupLog.Error(err, "unable to create client set")
		os.Exit(1)
	}
	restConfig := set.RESTConfig()

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		Client: client.Options{
			HTTPClient: set.HTTPClient(),
		},
		GracefulShutdownTimeout: &gracefulShutdownTimeout,
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        "controller.kro.run",
		LeaderElectionNamespace: leaderElectionNamespace,
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		LeaderElectionReleaseOnCancel: false,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// RGD cluster: use separate cluster if rgdKubeconfig is specified, otherwise reuse manager
	var rgdCluster cluster.Cluster
	if rgdKubeconfig != "" {
		rgdSet, err := kroclient.NewSet(kroclient.Config{
			KubeconfigPath: rgdKubeconfig,
			QPS:            float32(qps),
			Burst:          burst,
		})
		if err != nil {
			setupLog.Error(err, "unable to create RGD client set")
			os.Exit(1)
		}
		rgdCluster, err = cluster.New(rgdSet.RESTConfig(), func(o *cluster.Options) {
			o.Scheme = scheme
			o.HTTPClient = rgdSet.HTTPClient()
		})
		if err != nil {
			setupLog.Error(err, "unable to create RGD cluster")
			os.Exit(1)
		}
		if err := mgr.Add(rgdCluster); err != nil {
			setupLog.Error(err, "unable to add RGD cluster to manager")
			os.Exit(1)
		}
	} else {
		rgdCluster = mgr
	}

	// Workload cluster and client set: use separate if workloadKubeconfig is specified
	var workloadCluster cluster.Cluster
	var workloadSet kroclient.SetInterface
	if workloadKubeconfig != "" {
		workloadSet, err = kroclient.NewSet(kroclient.Config{
			KubeconfigPath: workloadKubeconfig,
			QPS:            float32(qps),
			Burst:          burst,
		})
		if err != nil {
			setupLog.Error(err, "unable to create workload client set")
			os.Exit(1)
		}
		workloadCluster, err = cluster.New(workloadSet.RESTConfig(), func(o *cluster.Options) {
			o.Scheme = scheme
			o.HTTPClient = workloadSet.HTTPClient()
		})
		if err != nil {
			setupLog.Error(err, "unable to create workload cluster")
			os.Exit(1)
		}
		if err := mgr.Add(workloadCluster); err != nil {
			setupLog.Error(err, "unable to add workload cluster to manager")
			os.Exit(1)
		}
	} else {
		workloadCluster = mgr
		workloadSet = set
	}

	// Dynamic controller uses workload client set for watching CR instances
	dc := dynamiccontroller.NewDynamicController(rootLogger, dynamiccontroller.Config{
		Workers:         dynamicControllerConcurrentReconciles,
		ResyncPeriod:    time.Duration(resyncPeriod) * time.Second,
		QueueMaxRetries: queueMaxRetries,
		MinRetryDelay:   minRetryDelay,
		MaxRetryDelay:   maxRetryDelay,
		RateLimit:       rateLimit,
		BurstLimit:      burstLimit,
	}, workloadSet.Metadata(), workloadSet.RESTMapper())

	// Graph builder uses workload client set to discover API resources in workload cluster
	resourceGraphDefinitionGraphBuilder, err := graph.NewBuilder(workloadSet.RESTConfig(), workloadSet.HTTPClient())
	if err != nil {
		setupLog.Error(err, "unable to create resource graph definition graph builder")
		os.Exit(1)
	}

	rgd := resourcegraphdefinitionctrl.NewResourceGraphDefinitionReconciler(
		workloadSet,
		rgdCluster,
		workloadCluster,
		allowCRDDeletion,
		dc,
		resourceGraphDefinitionGraphBuilder,
		resourceGraphDefinitionConcurrentReconciles,
	)
	if err := rgd.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ResourceGraphDefinition")
		os.Exit(1)
	}

	if err := mgr.Add(dc); err != nil {
		setupLog.Error(err, "unable to add dynamic controller to manager")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}

	if err = mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
