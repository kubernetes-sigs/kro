// Copyright 2025 The Kube Resource Orchestrator Authors
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

package client

import (
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrlrtconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/release-utils/version"
)

// Set provides a unified interface for different Kubernetes clients
type Set struct {
	config          *rest.Config
	kubernetes      *kubernetes.Clientset
	dynamic         *dynamic.DynamicClient
	apiExtensionsV1 *apiextensionsv1.ApiextensionsV1Client
}

// Config holds configuration for client creation
type Config struct {
	RestConfig      *rest.Config
	ImpersonateUser string
	QPS             float32
	Burst           int
}

// NewSet creates a new client Set with the given config
func NewSet(cfg Config) (*Set, error) {
	var err error
	config := cfg.RestConfig

	if config == nil {
		config, err = ctrlrtconfig.GetConfig()
		if err != nil {
			return nil, err
		}
	}

	if cfg.ImpersonateUser != "" {
		config = rest.CopyConfig(config)
		config.Impersonate = rest.ImpersonationConfig{
			UserName: cfg.ImpersonateUser,
		}
	}

	// Set default QPS and burst
	if config.QPS == 0 {
		config.QPS = cfg.QPS
	}
	if config.Burst == 0 {
		config.Burst = cfg.Burst
	}
	config.UserAgent = fmt.Sprintf("kro/%s", version.GetVersionInfo().GitVersion)

	c := &Set{config: config}
	if err := c.init(); err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Set) init() error {
	var err error

	// share http client between all k8s clients
	sharedHttpClient, err := rest.HTTPClientFor(c.config)
	if err != nil {
		return fmt.Errorf("failed to create HTTP client: %w", err)
	}

	c.kubernetes, err = kubernetes.NewForConfigAndClient(c.config, sharedHttpClient)
	if err != nil {
		return err
	}

	c.dynamic, err = dynamic.NewForConfigAndClient(c.config, sharedHttpClient)
	if err != nil {
		return err
	}

	c.apiExtensionsV1, err = apiextensionsv1.NewForConfigAndClient(c.config, sharedHttpClient)
	if err != nil {
		return err
	}

	return nil
}

// Kubernetes returns the standard Kubernetes clientset
func (c *Set) Kubernetes() *kubernetes.Clientset {
	return c.kubernetes
}

// Dynamic returns the dynamic client
func (c *Set) Dynamic() *dynamic.DynamicClient {
	return c.dynamic
}

// APIExtensionsV1 returns the API extensions client
func (c *Set) APIExtensionsV1() *apiextensionsv1.ApiextensionsV1Client {
	return c.apiExtensionsV1
}

// RESTConfig returns a copy of the underlying REST config
func (c *Set) RESTConfig() *rest.Config {
	return rest.CopyConfig(c.config)
}

// CRD returns a new CRDWrapper instance
func (c *Set) CRD(cfg CRDWrapperConfig) *CRDWrapper {
	if cfg.Client == nil {
		cfg.Client = c.apiExtensionsV1
	}

	return newCRDWrapper(cfg)
}

// WithImpersonation returns a new client that impersonates the given user
func (c *Set) WithImpersonation(user string) (*Set, error) {
	return NewSet(Config{
		RestConfig:      c.config,
		ImpersonateUser: user,
	})
}
