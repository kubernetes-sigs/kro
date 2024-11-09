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
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	// DefaultPollInterval is the default interval for polling CRD status
	defaultPollInterval = 150 * time.Millisecond
	// DefaultTimeout is the default timeout for waiting for CRD status
	defaultTimeout = 2 * time.Minute
)

var _ CRDClient = &CRDWrapper{}

// CRDClient represents operations for managing CustomResourceDefinitions
type CRDClient interface {
	// EnsureCreated ensures a CRD exists and is ready
	Ensure(ctx context.Context, crd v1.CustomResourceDefinition) error

	// Delete removes a CRD if it exists
	Delete(ctx context.Context, name string) error

	// Get retrieves a CRD by name
	Get(ctx context.Context, name string) (*v1.CustomResourceDefinition, error)
}

// CRDWrapper provides a simplified interface for CRD operations
type CRDWrapper struct {
	client       apiextensionsv1.CustomResourceDefinitionInterface
	log          logr.Logger
	pollInterval time.Duration
	timeout      time.Duration
}

// CRDWrapperConfig contains configuration for the CRD wrapper
type CRDWrapperConfig struct {
	Client       *apiextensionsv1.ApiextensionsV1Client
	Log          logr.Logger
	PollInterval time.Duration
	Timeout      time.Duration
}

// DefaultConfig returns a CRDWrapperConfig with default values
func DefaultCRDWrapperConfig() CRDWrapperConfig {
	return CRDWrapperConfig{
		PollInterval: defaultPollInterval,
		Timeout:      defaultTimeout,
	}
}

// newCRDWrapper creates a new CRD wrapper
func newCRDWrapper(cfg CRDWrapperConfig) *CRDWrapper {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}

	return &CRDWrapper{
		client:       cfg.Client.CustomResourceDefinitions(),
		log:          cfg.Log.WithName("crd-wrapper"),
		pollInterval: cfg.PollInterval,
		timeout:      cfg.Timeout,
	}
}

// Ensure ensures a CRD exists, up-to-date, and is ready. This can be
// a dangerous operation as it will update the CRD if it already exists.
//
// The caller is responsible for ensuring the CRD, isn't introducing
// breaking changes.
func (w *CRDWrapper) Ensure(ctx context.Context, crd v1.CustomResourceDefinition) error {
	_, err := w.Get(ctx, crd.Name)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to check for existing CRD: %w", err)
		}

		w.log.Info("Creating CRD", "name", crd.Name)
		if err := w.create(ctx, crd); err != nil {
			return fmt.Errorf("failed to create CRD: %w", err)
		}
	} else {
		w.log.Info("Updating existing CRD", "name", crd.Name)
		if err := w.patch(ctx, crd); err != nil {
			return fmt.Errorf("failed to patch CRD: %w", err)
		}
	}

	return w.waitForReady(ctx, crd.Name)
}

// Get retrieves a CRD by name
func (w *CRDWrapper) Get(ctx context.Context, name string) (*v1.CustomResourceDefinition, error) {
	return w.client.Get(ctx, name, metav1.GetOptions{})
}

func (w *CRDWrapper) create(ctx context.Context, crd v1.CustomResourceDefinition) error {
	_, err := w.client.Create(ctx, &crd, metav1.CreateOptions{})
	return err
}

func (w *CRDWrapper) patch(ctx context.Context, newCRD v1.CustomResourceDefinition) error {
	patchBytes, err := json.Marshal(newCRD)
	if err != nil {
		return fmt.Errorf("failed to marshal CRD for patch: %w", err)
	}

	_, err = w.client.Patch(
		ctx,
		newCRD.Name,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{},
	)
	return err
}

// Delete removes a CRD if it exists
func (w *CRDWrapper) Delete(ctx context.Context, name string) error {
	w.log.Info("Deleting CRD", "name", name)

	err := w.client.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete CRD: %w", err)
	}
	return nil
}

// waitForReady waits for a CRD to become ready
func (w *CRDWrapper) waitForReady(ctx context.Context, name string) error {
	w.log.Info("Waiting for CRD to become ready", "name", name)

	return wait.PollUntilContextTimeout(ctx, w.pollInterval, w.timeout, true,
		func(ctx context.Context) (bool, error) {
			crd, err := w.Get(ctx, name)
			if err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}

			for _, cond := range crd.Status.Conditions {
				if cond.Type == v1.Established && cond.Status == v1.ConditionTrue {
					return true, nil
				}
			}

			return false, nil
		})
}
