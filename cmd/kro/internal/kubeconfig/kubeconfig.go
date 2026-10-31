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

// Package kubeconfig holds the cluster connection options shared by the kro
// CLI commands (the global --kubeconfig and --context flags) and turns them
// into a REST config. It is a leaf package so that both cmd/kro and
// cmd/kro/commands/... can import it without an import cycle.
package kubeconfig

import (
	"fmt"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
)

// Options selects the cluster the CLI talks to.
type Options struct {
	// KubeConfigPath is an explicit kubeconfig file. When empty, the standard
	// kubectl loading rules apply: $KUBECONFIG, then ~/.kube/config, then the
	// in-cluster config.
	KubeConfigPath string
	// Context overrides the kubeconfig's current-context when non-empty.
	Context string
}

// Global is bound to the root command's persistent flags.
var Global = &Options{}

// RESTConfig builds a REST config from the options using the same loading
// rules and precedence as kubectl.
func (o *Options) RESTConfig() (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = o.KubeConfigPath

	overrides := &clientcmd.ConfigOverrides{CurrentContext: o.Context}

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}
	return cfg, nil
}

// NewClientSet returns a kro client set for the cluster selected by the options.
func (o *Options) NewClientSet() (*kroclient.Set, error) {
	cfg, err := o.RESTConfig()
	if err != nil {
		return nil, err
	}
	return kroclient.NewSet(kroclient.Config{RestConfig: cfg})
}
