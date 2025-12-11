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

package client

import (
	"fmt"
	"net/http"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	ctrlrtconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/release-utils/version"
)

// SetInterface provides a unified interface for different Kubernetes clients
type SetInterface interface {
	HTTPClient() *http.Client

	// Kubernetes returns the standard Kubernetes clientset
	Kubernetes() kubernetes.Interface

	// Dynamic returns the dynamic client
	Dynamic() dynamic.Interface

	// Metadata returns the metadata client
	Metadata() metadata.Interface

	// APIExtensionsV1 returns the API extensions client
	APIExtensionsV1() apiextensionsv1.ApiextensionsV1Interface

	// RESTConfig returns a copy of the underlying REST config
	RESTConfig() *rest.Config

	// CRD returns a new CRDInterface instance
	CRD(cfg CRDWrapperConfig) CRDInterface

	// WithImpersonation returns a new client that impersonates the given user
	WithImpersonation(user string) (SetInterface, error)

	RESTMapper() meta.RESTMapper
	SetRESTMapper(restMapper meta.RESTMapper)
}

// Set provides a unified interface for different Kubernetes clients
type Set struct {
	config          *rest.Config
	kubernetes      *kubernetes.Clientset
	dynamic         *dynamic.DynamicClient
	metadata        metadata.Interface
	apiExtensionsV1 *apiextensionsv1.ApiextensionsV1Client
	// restMapper is a REST mapper for the Kubernetes API server
	restMapper meta.RESTMapper
	httpClient *http.Client
}

var _ SetInterface = (*Set)(nil)

// BuildRestConfig builds a REST config from a kubeconfig path.
// If kubeconfigPath is empty, it falls back to in-cluster config or the default
// kubeconfig loading rules (KUBECONFIG env var, ~/.kube/config).
func BuildRestConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}
	// Use controller-runtime's config loader which handles in-cluster config
	// and KUBECONFIG env var properly
	return ctrlrtconfig.GetConfig()
}

// Config holds configuration for client creation
type Config struct {
	// RestConfig is an optional pre-built REST config. If provided, it takes precedence
	// over KubeconfigPath.
	RestConfig *rest.Config
	// KubeconfigPath is the path to a kubeconfig file. If empty and RestConfig is nil,
	// in-cluster config will be used.
	KubeconfigPath  string
	ImpersonateUser string
	QPS             float32
	Burst           int
}

// NewSet creates a new client Set with the given config
func NewSet(cfg Config) (*Set, error) {
	var err error
	config := cfg.RestConfig

	if config == nil {
		config, err = BuildRestConfig(cfg.KubeconfigPath)
		if err != nil {
			return nil, fmt.Errorf("failed to build REST config: %w", err)
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
	c.httpClient, err = rest.HTTPClientFor(c.config)
	if err != nil {
		return fmt.Errorf("failed to create HTTP client: %w", err)
	}

	c.kubernetes, err = kubernetes.NewForConfigAndClient(c.config, c.httpClient)
	if err != nil {
		return err
	}

	c.metadata, err = metadata.NewForConfigAndClient(c.config, c.httpClient)
	if err != nil {
		return err
	}

	c.dynamic, err = dynamic.NewForConfigAndClient(c.config, c.httpClient)
	if err != nil {
		return err
	}

	c.apiExtensionsV1, err = apiextensionsv1.NewForConfigAndClient(c.config, c.httpClient)
	if err != nil {
		return err
	}

	c.restMapper, err = apiutil.NewDynamicRESTMapper(c.config, c.httpClient)
	if err != nil {
		return err
	}

	return nil
}

func (c *Set) HTTPClient() *http.Client {
	return c.httpClient
}

// Kubernetes returns the standard Kubernetes clientset
func (c *Set) Kubernetes() kubernetes.Interface {
	return c.kubernetes
}

// Metadata returns the metadata client
func (c *Set) Metadata() metadata.Interface {
	return c.metadata
}

// Dynamic returns the dynamic client
func (c *Set) Dynamic() dynamic.Interface {
	return c.dynamic
}

// APIExtensionsV1 returns the API extensions client
func (c *Set) APIExtensionsV1() apiextensionsv1.ApiextensionsV1Interface {
	return c.apiExtensionsV1
}

// RESTConfig returns a copy of the underlying REST config
func (c *Set) RESTConfig() *rest.Config {
	return rest.CopyConfig(c.config)
}

// RESTMapper returns the REST mapper
func (c *Set) RESTMapper() meta.RESTMapper {
	return c.restMapper
}

// CRD returns a new CRDInterface instance
func (c *Set) CRD(cfg CRDWrapperConfig) CRDInterface {
	if cfg.Client == nil {
		cfg.Client = c.apiExtensionsV1
	}

	return newCRDWrapper(cfg)
}

// WithImpersonation returns a new client that impersonates the given user
func (c *Set) WithImpersonation(user string) (SetInterface, error) {
	return NewSet(Config{
		RestConfig:      c.config,
		ImpersonateUser: user,
	})
}

// SetRESTMapper sets the REST mapper for the client
func (c *Set) SetRESTMapper(restMapper meta.RESTMapper) {
	c.restMapper = restMapper
}
