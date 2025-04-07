// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	xv1alpha1 "github.com/kro-run/kro/api/v1alpha1"
	kroclient "github.com/kro-run/kro/pkg/client"
	resourcegraphdefinitionctrl "github.com/kro-run/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kro-run/kro/pkg/dynamiccontroller"
	"github.com/kro-run/kro/pkg/graph"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(xv1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

type customLevelEnabler struct {
	level int
}

func (c customLevelEnabler) Enabled(lvl zapcore.Level) bool {
	return -int(lvl) <= c.level
}

func main() {
	var (
		metricsAddr                                 string
		enableLeaderElection                        bool
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
		resyncPeriod    int
		queueMaxRetries int
		shutdownTimeout int
		// var dynamicControllerDefaultResyncPeriod int
		logLevel int
		qps      float64
		burst    int
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8078", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8079", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&allowCRDDeletion, "allow-crd-deletion", false, "allow kro to delete CRDs")
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
	flag.IntVar(&resyncPeriod, "dynamic-controller-default-resync-period", 10,
		"interval at which the controller will re list resources even with no changes, in hours")
	flag.IntVar(&queueMaxRetries, "dynamic-controller-default-queue-max-retries", 20,
		"maximum number of retries for an item in the queue will be retried before being dropped")
	flag.IntVar(&shutdownTimeout, "dynamic-controller-default-shutdown-timeout", 60,
		"maximum duration to wait for the controller to gracefully shutdown, in seconds")
	// log level flags
	flag.IntVar(&logLevel, "log-level", 10, "The log level verbosity. 0 is the least verbose, 5 is the most verbose.")
	// qps and burst
	flag.Float64Var(&qps, "client-qps", 100, "The number of queries per second to allow")
	flag.IntVar(&burst, "client-burst", 150,
		"The number of requests that can be stored for processing before the server starts enforcing the QPS limit")

	flag.Parse()

	opts := zap.Options{
		Development: true,
		Level:       customLevelEnabler{level: logLevel},
		TimeEncoder: zapcore.ISO8601TimeEncoder,
	}
	rootLogger := zap.New(zap.UseFlagOptions(&opts))

	ctrl.SetLogger(rootLogger)

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
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "6f0f64a5.kro.run",
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
		// LeaderElectionReleaseOnCancel: true,
		Logger: rootLogger,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	dc := dynamiccontroller.NewDynamicController(rootLogger, dynamiccontroller.Config{
		Workers: dynamicControllerConcurrentReconciles,
		// TODO(a-hilaly): expose these as flags
		ShutdownTimeout: time.Duration(shutdownTimeout) * time.Second,
		ResyncPeriod:    time.Duration(resyncPeriod) * time.Hour,
		QueueMaxRetries: queueMaxRetries,
		MinRetryDelay:   minRetryDelay,
		MaxRetryDelay:   maxRetryDelay,
		RateLimit:       rateLimit,
		BurstLimit:      burstLimit,
	}, set.Dynamic())

	resourceGraphDefinitionGraphBuilder, err := graph.NewBuilder(
		restConfig,
	)
	if err != nil {
		setupLog.Error(err, "unable to create resource graph definition graph builder")
		os.Exit(1)
	}

	rgd := resourcegraphdefinitionctrl.NewResourceGraphDefinitionReconciler(
		set,
		allowCRDDeletion,
		dc,
		resourceGraphDefinitionGraphBuilder,
		resourceGraphDefinitionConcurrentReconciles,
	)
	if err := rgd.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ResourceGraphDefinition")
		os.Exit(1)
	}

	go func() {
		err := dc.Run(context.Background())
		if err != nil {
			setupLog.Error(err, "dynamic controller failed to run")
		}
	}()

	//+kubebuilder:scaffold:builder

	if err = mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}

	if err = mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()

	go func() {
		if err := mgr.Start(ctx); err != nil {
			setupLog.Error(err, "problem running manager")
			os.Exit(1)
		}
	}()

	<-ctx.Done()

}

// getExpectedResources returns a list of resources that should exist based on templates and conditions
func (r *ResourceGraphDefinitionReconciler) getExpectedResources(ctx context.Context, resourceGraphDef *xv1alpha1.ResourceGraphDefinition) ([]xv1alpha1.ManagedResource, error) {
	var expectedResources []xv1alpha1.ManagedResource
	
	// Iterate through the resources in the ResourceGraphDefinition
	for _, resource := range resourceGraphDef.Spec.Resources {
		// Check if the resource should be included based on conditions
		shouldInclude, err := r.evaluateIncludeWhen(ctx, resourceGraphDef, resource)
		if err != nil {
			return nil, err
		}
		
		if !shouldInclude {
			continue
		}
		
		// Extract resource information from the template
		managedResource, err := r.extractResourceInfo(ctx, resourceGraphDef, resource)
		if err != nil {
			return nil, err
		}
		
		expectedResources = append(expectedResources, managedResource)
	}
	
	return expectedResources, nil
}

// evaluateIncludeWhen evaluates the IncludeWhen conditions for a resource
func (r *ResourceGraphDefinitionReconciler) evaluateIncludeWhen(ctx context.Context, resourceGraphDef *xv1alpha1.ResourceGraphDefinition, resource *xv1alpha1.Resource) (bool, error) {
	if len(resource.IncludeWhen) == 0 {
		return true, nil
	}
	
	// Evaluate each condition
	for _, condition := range resource.IncludeWhen {
		// This is a simplified implementation. You'll need to implement the actual condition evaluation logic
		// based on your specific requirements.
		result, err := r.evaluateCondition(ctx, resourceGraphDef, condition)
		if err != nil {
			return false, err
		}
		
		if !result {
			return false, nil
		}
	}
	
	return true, nil
}

// extractResourceInfo extracts resource information from the template
func (r *ResourceGraphDefinitionReconciler) extractResourceInfo(ctx context.Context, resourceGraphDef *xv1alpha1.ResourceGraphDefinition, resource *xv1alpha1.Resource) (xv1alpha1.ManagedResource, error) {
	// This is a simplified implementation. You'll need to implement the actual logic to extract
	// resource information from the template based on your specific requirements.
	
	// For example, you might need to parse the template and extract the kind, name, and namespace
	var managedResource xv1alpha1.ManagedResource
	
	// Extract information from the template
	// This is just a placeholder. You'll need to implement the actual logic.
	managedResource.Kind = "Deployment" // Example
	managedResource.Name = "example-deployment" // Example
	managedResource.Namespace = "default" // Example
	managedResource.ResourceID = resource.ID
	
	return managedResource, nil
}

// findOrphanedResources finds resources that are in currentManagedResources but not in expectedResources
func (r *ResourceGraphDefinitionReconciler) findOrphanedResources(currentManagedResources, expectedResources []xv1alpha1.ManagedResource) []xv1alpha1.ManagedResource {
	var orphanedResources []xv1alpha1.ManagedResource
	
	// Create a map of expected resources for quick lookup
	expectedMap := make(map[string]xv1alpha1.ManagedResource)
	for _, resource := range expectedResources {
		key := r.resourceKey(resource)
		expectedMap[key] = resource
	}
	
	// Check each current managed resource
	for _, resource := range currentManagedResources {
		key := r.resourceKey(resource)
		if _, exists := expectedMap[key]; !exists {
			orphanedResources = append(orphanedResources, resource)
		}
	}
	
	return orphanedResources
}

// resourceKey generates a unique key for a resource
func (r *ResourceGraphDefinitionReconciler) resourceKey(resource xv1alpha1.ManagedResource) string {
	return fmt.Sprintf("%s/%s/%s", resource.Kind, resource.Namespace, resource.Name)
}

// deleteResource deletes a resource from the cluster
func (r *ResourceGraphDefinitionReconciler) deleteResource(ctx context.Context, resource xv1alpha1.ManagedResource) error {
	// This is a simplified implementation. You'll need to implement the actual logic to delete
	// the resource from the cluster based on your specific requirements.
	
	// For example, you might need to use the dynamic client to delete the resource
	// This is just a placeholder. You'll need to implement the actual logic.
	
	// Example:
	// gvr, err := r.getGroupVersionResource(resource.Kind)
	// if err != nil {
	//     return err
	// }
	// 
	// return r.DynamicClient.Resource(gvr).Namespace(resource.Namespace).Delete(ctx, resource.Name, metav1.DeleteOptions{})
	
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceGraphDefinitionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&xv1alpha1.ResourceGraphDefinition{}).
		Complete(r)
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ResourceGraphDefinitionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	
	// Fetch the ResourceGraphDefinition instance
	resourceGraphDef := &xv1alpha1.ResourceGraphDefinition{}
	if err := r.Get(ctx, req.NamespacedName, resourceGraphDef); err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Return and don't requeue
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}
	
	// Get the current list of resources that should exist based on templates and conditions
	expectedResources, err := r.getExpectedResources(ctx, resourceGraphDef)
	if err != nil {
		log.Error(err, "Failed to get expected resources")
		return ctrl.Result{}, err
	}
	
	// Get the current list of managed resources from the status
	currentManagedResources := resourceGraphDef.Status.ManagedResources
	
	// Find orphaned resources (resources that are in currentManagedResources but not in expectedResources)
	orphanedResources := r.findOrphanedResources(currentManagedResources, expectedResources)
	
	// Delete orphaned resources
	for _, orphanedResource := range orphanedResources {
		if err := r.deleteResource(ctx, orphanedResource); err != nil {
			log.Error(err, "Failed to delete orphaned resource", 
				"kind", orphanedResource.Kind, 
				"name", orphanedResource.Name, 
				"namespace", orphanedResource.Namespace)
			// Continue with other resources even if one fails
		} else {
			log.Info("Deleted orphaned resource", 
				"kind", orphanedResource.Kind, 
				"name", orphanedResource.Name, 
				"namespace", orphanedResource.Namespace)
		}
	}
	
	// Update the managedResources field in the status
	resourceGraphDef.Status.ManagedResources = expectedResources
	
	// Update the status
	if err := r.Status().Update(ctx, resourceGraphDef); err != nil {
		log.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}
	
	return ctrl.Result{}, nil
}
