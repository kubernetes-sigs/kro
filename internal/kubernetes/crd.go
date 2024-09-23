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

package kubernetes

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/go-logr/logr"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type CRDManager interface {
	Create(ctx context.Context, crd v1.CustomResourceDefinition) error
	Update(ctx context.Context, crd v1.CustomResourceDefinition) error
	Ensure(ctx context.Context, crd v1.CustomResourceDefinition) error
	Describe(ctx context.Context, name string) (*v1.CustomResourceDefinition, error)
	Delete(ctx context.Context, name string) error
	Patch(ctx context.Context, newCRD v1.CustomResourceDefinition) error
	WaitUntilReady(ctx context.Context, name string, delay time.Duration, maxAttempts int) error
}

var _ CRDManager = &CRDClient{}

// CRDClient is an object that allows for the management of CRDs
// It is mainly responsible for creating and deleting CRDs
type CRDClient struct {
	Client *apiextensionsv1.ApiextensionsV1Client
	log    logr.Logger
}

func NewCRDClient(Client *apiextensionsv1.ApiextensionsV1Client, log logr.Logger) *CRDClient {
	crdLogger := log.WithName("crd-manager")

	return &CRDClient{
		log:    crdLogger,
		Client: Client,
	}
}

func (m *CRDClient) Create(ctx context.Context, crd v1.CustomResourceDefinition) error {
	m.log.V(1).Info("Creating CRD", "name", crd.Name)
	_, err := m.Client.CustomResourceDefinitions().Create(
		ctx,
		&crd,
		metav1.CreateOptions{},
	)
	return err
}

func (m *CRDClient) Update(ctx context.Context, crd v1.CustomResourceDefinition) error {
	m.log.V(1).Info("Updating CRD", "name", crd.Name)
	_, err := m.Client.CustomResourceDefinitions().Update(
		ctx,
		&crd,
		metav1.UpdateOptions{},
	)
	return err
}

func (m *CRDClient) Ensure(ctx context.Context, crd v1.CustomResourceDefinition) error {
	m.log.V(1).Info("Ensuring CRD exists", "name", crd.Name)
	_, err := m.Describe(ctx, crd.Name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			err := m.Create(ctx, crd)
			if err != nil {
				return err
			}
			if err := m.WaitUntilReady(ctx, crd.Name, 150*time.Millisecond, 10); err != nil {
				return err
			}
			return nil
		}
		return err
	}

	// patch the CRD
	return m.Patch(ctx, crd)
}

func (m *CRDClient) WaitUntilReady(ctx context.Context, name string, delay time.Duration, maxAttempts int) error {
	attempts := 0

	m.log.V(1).Info("Waiting for CRD to be ready", "name", name)
	for {
		attempts++
		crd, err := m.Describe(ctx, name)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}

		if crd.Status.Conditions != nil {
			for _, condition := range crd.Status.Conditions {
				if condition.Type == v1.Established && condition.Status == v1.ConditionTrue {
					m.log.V(1).Info("CRD is ready", "name", name)
					return nil
				}
			}
		}

		if attempts >= maxAttempts {
			return apierrors.NewTimeoutError("CRD is not ready", -1)
		}
		time.Sleep(delay)
	}
}

func (m *CRDClient) Describe(ctx context.Context, name string) (*v1.CustomResourceDefinition, error) {
	m.log.V(1).Info("Describing CRD", "name", name)
	return m.Client.CustomResourceDefinitions().Get(
		ctx,
		name,
		metav1.GetOptions{},
	)
}

func (m *CRDClient) Delete(ctx context.Context, name string) error {
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

func (m *CRDClient) Patch(ctx context.Context, newCRD v1.CustomResourceDefinition) error {
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
