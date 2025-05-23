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

	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cmd"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// DefaultKindClusterName is the default name for the kind cluster
	DefaultKindClusterName = "kro-e2e"
	// DefaultKindWaitTimeout is the default timeout for waiting for the kind cluster
	DefaultKindWaitTimeout = 5 * time.Minute
)

// KindClusterProvider manages a kind cluster for e2e testing
type KindClusterProvider struct {
	// Name is the name of the kind cluster
	Name string
	// WaitTimeout is the timeout for waiting for the kind cluster
	WaitTimeout time.Duration
	// provider is the kind cluster provider
	provider *cluster.Provider
	// kubeconfig is the path to the kubeconfig file
	kubeconfig string
	// restConfig is the kubernetes rest config
	restConfig *rest.Config
}

// NewKindClusterProvider creates a new kind cluster provider
func NewKindClusterProvider(opts ...KindClusterOption) (*KindClusterProvider, error) {
	p := &KindClusterProvider{
		Name:        DefaultKindClusterName,
		WaitTimeout: DefaultKindWaitTimeout,
	}

	for _, opt := range opts {
		opt(p)
	}

	provider := cluster.NewProvider(
		cluster.ProviderWithLogger(cmd.NewLogger()),
	)
	p.provider = provider

	return p, nil
}

// KindClusterOption is a function that configures a KindClusterProvider
type KindClusterOption func(*KindClusterProvider)

// WithName sets the name of the kind cluster
func WithName(name string) KindClusterOption {
	return func(p *KindClusterProvider) {
		p.Name = name
	}
}

// WithWaitTimeout sets the timeout for waiting for the kind cluster
func WithWaitTimeout(timeout time.Duration) KindClusterOption {
	return func(p *KindClusterProvider) {
		p.WaitTimeout = timeout
	}
}

// Create creates a new kind cluster
func (p *KindClusterProvider) Create(ctx context.Context) error {
	// Create the kind cluster
	if err := p.provider.Create(
		p.Name,
		cluster.CreateWithWaitForReady(p.WaitTimeout),
		cluster.CreateWithKubeconfigPath(p.getKubeconfigPath()),
	); err != nil {
		return fmt.Errorf("failed to create kind cluster: %w", err)
	}

	// Get the kubeconfig
	kubeconfig, err := p.provider.KubeConfig(p.Name, false)
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	// Write the kubeconfig to a temporary file
	if err := os.WriteFile(p.getKubeconfigPath(), []byte(kubeconfig), 0644); err != nil {
		return fmt.Errorf("failed to write kubeconfig: %w", err)
	}

	// Create the rest config
	config, err := clientcmd.BuildConfigFromFlags("", p.getKubeconfigPath())
	if err != nil {
		return fmt.Errorf("failed to build config from flags: %w", err)
	}
	p.restConfig = config

	return nil
}

// Delete deletes the kind cluster
func (p *KindClusterProvider) Delete(ctx context.Context) error {
	if err := p.provider.Delete(p.Name, p.getKubeconfigPath()); err != nil {
		return fmt.Errorf("failed to delete kind cluster: %w", err)
	}
	return nil
}

// GetKubeconfig returns the path to the kubeconfig file
func (p *KindClusterProvider) GetKubeconfig() string {
	return p.getKubeconfigPath()
}

// GetRESTConfig returns the kubernetes rest config
func (p *KindClusterProvider) GetRESTConfig() *rest.Config {
	return p.restConfig
}

// getKubeconfigPath returns the path to the kubeconfig file
func (p *KindClusterProvider) getKubeconfigPath() string {
	if p.kubeconfig != "" {
		return p.kubeconfig
	}
	p.kubeconfig = filepath.Join(os.TempDir(), fmt.Sprintf("kubeconfig-%s", p.Name))
	return p.kubeconfig
}
