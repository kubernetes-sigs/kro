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
	"net/http"

	"github.com/go-logr/logr"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	graphenginectrl "github.com/kubernetes-sigs/kro/pkg/graphengine/controller/graph"
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
	restConfig *rest.Config,
	httpClient *http.Client,
	metaClient metadata.Interface,
	logger logr.Logger,
) error {
	cmp, err := compiler.NewCompiler(restConfig, httpClient)
	if err != nil {
		return fmt.Errorf("build graph compiler: %w", err)
	}

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

	reconciler := &graphenginectrl.Reconciler{
		Client:        mgr.GetClient(),
		Compiler:      cmp,
		Registry:      reg,
		Executor:      executor.NewSimple(mgr.GetClient()),
		Router:        router,
		SchemaWatcher: sw,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup graph reconciler: %w", err)
	}
	return nil
}
