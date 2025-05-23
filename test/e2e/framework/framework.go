// Copyright 2025 The Kube Resource Orchestrator Authors.
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

package framework

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultWaitTimeout is the default timeout for waiting operations
	DefaultWaitTimeout = 2 * time.Minute
	// DefaultWaitInterval is the default interval for waiting operations
	DefaultWaitInterval = 5 * time.Second
)

// Framework provides utilities for e2e testing
type Framework struct {
	// KindCluster is the kind cluster provider (optional)
	KindCluster *KindClusterProvider
	// Client is the controller-runtime client
	Client client.Client
	// RESTConfig is the kubernetes rest config
	RESTConfig *rest.Config
	// Scheme is the runtime scheme
	Scheme *runtime.Scheme
}

// Option is a function that configures a Framework
type Option func(*Framework) error

// WithKindCluster configures the framework to use a kind cluster
func WithKindCluster(opts ...KindClusterOption) Option {
	return func(f *Framework) error {
		kindCluster, err := NewKindClusterProvider(opts...)
		if err != nil {
			return fmt.Errorf("failed to create kind cluster provider: %w", err)
		}
		f.KindCluster = kindCluster
		return nil
	}
}

// WithKubeconfig configures the framework to use an existing kubeconfig
func WithKubeconfig(kubeconfigPath string) Option {
	return func(f *Framework) error {
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			return fmt.Errorf("failed to build config from kubeconfig: %w", err)
		}
		f.RESTConfig = config
		return nil
	}
}

// New creates a new test framework
func New(opts ...Option) (*Framework, error) {
	f := &Framework{}

	// Apply options
	for _, opt := range opts {
		if err := opt(f); err != nil {
			return nil, err
		}
	}

	// If no cluster provider or kubeconfig is specified, try to use default kubeconfig
	if f.KindCluster == nil && f.RESTConfig == nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
		}
		if err := WithKubeconfig(kubeconfig)(f); err != nil {
			return nil, fmt.Errorf("failed to use default kubeconfig: %w", err)
		}
	}

	return f, nil
}

// Setup sets up the test framework
func (f *Framework) Setup(ctx context.Context) error {
	// Create kind cluster if configured
	if f.KindCluster != nil {
		if err := f.KindCluster.Create(ctx); err != nil {
			return fmt.Errorf("failed to create kind cluster: %w", err)
		}
		f.RESTConfig = f.KindCluster.GetRESTConfig()
	}

	// Create client
	client, err := client.New(f.RESTConfig, client.Options{})
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}
	f.Client = client

	return nil
}

// Teardown tears down the test framework
func (f *Framework) Teardown(ctx context.Context) error {
	// Delete kind cluster if it was created
	if f.KindCluster != nil {
		if err := f.KindCluster.Delete(ctx); err != nil {
			return fmt.Errorf("failed to delete kind cluster: %w", err)
		}
	}
	return nil
}

// LoadCRDs loads CRDs from the specified directory
func (f *Framework) LoadCRDs(ctx context.Context, crdDir string) error {
	// Get CRD paths
	crdPaths := []string{
		// kro CRDs
		filepath.Join(crdDir, "config", "crd", "bases"),
	}

	// Apply CRDs
	for _, crdPath := range crdPaths {
		if err := ApplyManifests(ctx, f.RESTConfig, crdPath); err != nil {
			return fmt.Errorf("failed to apply CRDs from %s: %w", crdPath, err)
		}
	}

	return nil
}

// DeployController deploys the kro controller
func (f *Framework) DeployController(ctx context.Context, manifestDir string) error {
	// Apply controller manifests
	if err := ApplyManifests(ctx, f.RESTConfig, manifestDir); err != nil {
		return fmt.Errorf("failed to apply controller manifests: %w", err)
	}

	return nil
}
