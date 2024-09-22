//go:build !dev

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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func NewClients() (*rest.Config, *kubernetes.Clientset, *dynamic.DynamicClient, *apiextensionsv1.ApiextensionsV1Client, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	apiExtensionsClient, err := apiextensionsv1.NewForConfig(config)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return config, clientset, dynamicClient, apiExtensionsClient, nil
}
