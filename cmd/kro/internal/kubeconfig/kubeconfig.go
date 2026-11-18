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

// Package kubeconfig selects the cluster for the kro CLI commands that talk to one.
package kubeconfig

import (
	"flag"

	"github.com/spf13/pflag"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	kroclient "github.com/kubernetes-sigs/kro/pkg/client"
)

var contextName string

// AddFlags adds --kubeconfig and --context to fs. --kubeconfig is controller-runtime's
// own flag, so NewClientSet loads the config the way controller-runtime's GetConfig does.
func AddFlags(fs *pflag.FlagSet) {
	goFlags := flag.NewFlagSet("kubeconfig", flag.ContinueOnError)
	ctrlconfig.RegisterFlags(goFlags)
	fs.AddGoFlagSet(goFlags)
	fs.StringVar(&contextName, "context", "", "Kubernetes context to use")
}

// NewClientSet returns a client set for the cluster that the flags select: the
// --kubeconfig file, or else the in-cluster config when $KUBECONFIG is unset, or
// else $KUBECONFIG or ~/.kube/config. --context replaces the current context of
// the kubeconfig file.
func NewClientSet() (*kroclient.Set, error) {
	restConfig, err := ctrlconfig.GetConfigWithContext(contextName)
	if err != nil {
		return nil, err
	}
	log := ctrllog.Log.WithName("kubeconfig")
	if contextName != "" {
		log = log.WithValues("context", contextName)
	}
	log.V(1).Info("Using cluster", "host", restConfig.Host)
	return kroclient.NewSet(kroclient.Config{RestConfig: restConfig})
}
