// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package crd

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/go-logr/logr"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var (
	defaultOwnerReference = metav1.OwnerReference{
		Name:       "symphony-controller",
		Kind:       "ResourceGroup",
		APIVersion: "x.symphony.k8s.aws/v1alpha1",
		Controller: &[]bool{false}[0],
		UID:        "00000000-0000-0000-0000-000000000000",
	}
)

// Manager is an object that allows for the management of CRDs
// It is mainly responsible for creating and deleting CRDs
type Manager struct {
	Client *apiextensionsv1.ApiextensionsV1Client
	log    logr.Logger
}

func NewManager(Client *apiextensionsv1.ApiextensionsV1Client, log logr.Logger) *Manager {
	crdLogger := log.WithName("crd-manager")

	return &Manager{
		log:    crdLogger,
		Client: Client,
	}
}

func (m *Manager) Create(ctx context.Context, crd v1.CustomResourceDefinition) error {
	crd.OwnerReferences = []metav1.OwnerReference{defaultOwnerReference}

	m.log.V(1).Info("Creating CRD", "name", crd.Name)
	_, err := m.Client.CustomResourceDefinitions().Create(
		ctx,
		&crd,
		metav1.CreateOptions{},
	)
	return err
}

func (m *Manager) Update(ctx context.Context, crd v1.CustomResourceDefinition) error {
	m.log.V(1).Info("Updating CRD", "name", crd.Name)
	_, err := m.Client.CustomResourceDefinitions().Update(
		ctx,
		&crd,
		metav1.UpdateOptions{},
	)
	return err
}

func (m *Manager) Ensure(ctx context.Context, crd v1.CustomResourceDefinition) error {
	m.log.V(1).Info("Ensuring CRD exists", "name", crd.Name)
	_, err := m.Describe(ctx, crd.Name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return m.Create(ctx, crd)
		}
		return err
	}

	// patch the CRD
	return m.Patch(ctx, crd)
}

func (m *Manager) Describe(ctx context.Context, name string) (*v1.CustomResourceDefinition, error) {
	m.log.V(1).Info("Describing CRD", "name", name)
	return m.Client.CustomResourceDefinitions().Get(
		ctx,
		name,
		metav1.GetOptions{},
	)
}

func (m *Manager) Delete(ctx context.Context, name string) error {
	m.log.V(1).Info("Deleting CRD", "name", name)
	err := m.Client.CustomResourceDefinitions().Delete(
		ctx,
		name,
		metav1.DeleteOptions{},
	)
	if err != nil {
		// if the CRD is not found, we can ignore the error
		if strings.Contains(err.Error(), "not found") {
			return nil
		}
	}
	return err
}

func (m *Manager) Patch(ctx context.Context, newCRD v1.CustomResourceDefinition) error {
	m.log.V(1).Info("Patching CRD", "name", newCRD.Name)
	b, err := json.Marshal(newCRD)
	if err != nil {
		return err
	}

	_, err = m.Client.CustomResourceDefinitions().Patch(
		ctx,
		newCRD.Name,
		types.MergePatchType,
		b,
		metav1.PatchOptions{},
	)
	return err
}
