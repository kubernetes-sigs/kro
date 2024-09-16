//go:build dev

// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package kubernetes

import (
	"flag"
	"path/filepath"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var (
	kubeconfig *string
)

func init() {
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
}

func NewRestConfig() (*rest.Config, error) {
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return nil, err
	}
	return config, nil
}

func NewDynamicClient() (*dynamic.DynamicClient, error) {

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return nil, err
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func NewAPIExtensionsClientSet() (*apiextensionsv1.ApiextensionsV1Client, error) {

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return nil, err

	}
	clientset, err := apiextensionsv1.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return clientset, nil
}

func NewClientSet() (*kubernetes.Clientset, error) {
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return clientset, nil
}
