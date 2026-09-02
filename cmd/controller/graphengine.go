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

package main

import (
	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ctrlgraph "github.com/kubernetes-sigs/kro/pkg/controller/graph"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/schemawatcher"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// The Graph kind (kro.run/v1alpha1) is registered in the scheme by
// api/v1alpha1's SchemeBuilder, which main.go already adds — no separate
// registration is needed here.

// setupGraphController wires the Graph controller into the
// manager alongside the ResourceGraphDefinition stack. It builds the compile
// cache, the resource-drift watch router, and the CRD schema watcher, then
// registers the reconciler. The router and schema watcher are added as manager
// Runnables so their event channels feed the same work queue as Graph spec
// changes.
func setupGraphController(
	mgr ctrl.Manager,
	cmp *compiler.Compiler,
	metaClient metadata.Interface,
	logger logr.Logger,
	concurrentReconciles int,
	maxCollectionSize int,
	applyConcurrency int,
	controllerServiceAccount string,
) error {
	router := watchrouter.NewRouter(logger.WithName("graph-watch-router"), watchrouter.Config{}, metaClient)
	if err := mgr.Add(router); err != nil {
		return fmt.Errorf("add graph watch router: %w", err)
	}

	reg := registry.New()
	sw := schemawatcher.New(logger.WithName("graph-schema-watcher"), schemawatcher.Config{
		Cache:   mgr.GetCache(),
		Graphs:  reg,
		Schemas: cmp,
	})
	if err := mgr.Add(sw); err != nil {
		return fmt.Errorf("add graph schema watcher: %w", err)
	}

	exec := executor.NewSimple(mgr.GetClient())
	exec.ApplyConcurrency = applyConcurrency
	// Standalone Graph objects carry no ApplySet part-of ownership label, so
	// per-Graph field-manager conflict detection is what keeps two Graphs that
	// template the same object from flip-flopping its fields. The RGD/instance
	// path leaves this off and relies on its ApplySet part-of guard instead.
	exec.ConflictDetection = true

	// A namespaced Graph applies its resources while impersonating a
	// ServiceAccount in the Graph's namespace (default, or spec.serviceAccountName).
	// Build impersonated controller-runtime clients from the manager's REST
	// config; they share the manager's REST mapper so discovery is not repeated
	// per ServiceAccount. The kro controller SA needs the "impersonate" verb on
	// serviceaccounts for this to take effect.
	baseCfg := mgr.GetConfig()
	mapper := mgr.GetRESTMapper()
	impersonation := ctrlgraph.NewImpersonation(exec, func(user string) (client.Client, error) {
		cfg := rest.CopyConfig(baseCfg)
		cfg.Impersonate = rest.ImpersonationConfig{UserName: user}
		return client.New(cfg, client.Options{Mapper: mapper})
	}, func(user string) (authorizationv1client.AuthorizationV1Interface, error) {
		// Same impersonated config drives the SelfSubjectAccessReview gate, so
		// "self" is the Graph's ServiceAccount. A client-go Clientset is used
		// (rather than the controller-runtime client) to avoid scheme wiring for
		// the SSAR type.
		cfg := rest.CopyConfig(baseCfg)
		cfg.Impersonate = rest.ImpersonationConfig{UserName: user}
		cs, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return nil, err
		}
		return cs.AuthorizationV1(), nil
	})

	reconciler := &ctrlgraph.Reconciler{
		Client:                   mgr.GetClient(),
		Compiler:                 cmp,
		Registry:                 reg,
		Executor:                 exec,
		Router:                   router,
		SchemaWatcher:            sw,
		MaxConcurrentReconciles:  concurrentReconciles,
		MaxCollectionSize:        maxCollectionSize,
		Impersonation:            impersonation,
		RequireImpersonation:     true,
		ControllerServiceAccount: controllerServiceAccount,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup graph reconciler: %w", err)
	}
	return nil
}
