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
	"os"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/tliron/glsp/server"
	"go.uber.org/zap"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	lsName = "kro-language-server"
)

// getKubernetesConfig attempts to create a Kubernetes client configuration
// by trying different configuration sources in order of preference.
// Returns nil if no valid configuration can be found.
func getKubernetesConfig(log logr.Logger) *rest.Config {
	// First, try in-cluster configuration (when running inside a Kubernetes pod)
	if config, err := rest.InClusterConfig(); err == nil {
		return config
	}

	// Second, try kubeconfig file from the default location (~/.kube/config)
	kubeconfigPath := clientcmd.RecommendedHomeFile
	if config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath); err == nil {
		return config
	}

	// Return nil if no configuration source is available (offline mode)
	return nil
}

func main() {
	// Initialize structured logging with Zap logger in development mode
	zapLogger, _ := zap.NewDevelopment()
	log := zapr.NewLogger(zapLogger)

	log.Info("Starting server", "name", lsName, "version", getVersion())

	// Attempt to get Kubernetes cluster configuration for online validation
	// If this returns nil, the LSP will operate in offline mode (basic validation only)
	clientConfig := getKubernetesConfig(log)

	// Create the KRO Language Server with:
	// - Logger for structured logging
	// - Kubernetes client config for online CRD and CEL validation (nil = offline mode)
	// - No additional options (nil)
	kroServer := NewKroServer(log, clientConfig, nil)

	// Create the LSP protocol handler with all the language server capabilities
	handler := kroServer.createHandler()

	// Initialize the GLSP server with our handler
	lspServer := server.NewServer(handler, lsName, false) // false = not debug mode

	// Store the server reference in kroServer for lifecycle management
	kroServer.server = lspServer

	log.V(1).Info("LSP server created, starting stdio communication")

	// Start the LSP server using stdio for communication with the language client
	// This is the standard way LSP servers communicate with editors like VS Code
	if err := lspServer.RunStdio(); err != nil {
		log.Error(err, "Error running LSP server")
		os.Exit(1)
	}
}
