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
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kubernetes-sigs/kro/pkg/metadata"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	logr "sigs.k8s.io/controller-runtime/pkg/log"

	crdcompat "github.com/kubernetes-sigs/kro/pkg/graph/crd/compat"
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
	Ensure(ctx context.Context, crd v1.CustomResourceDefinition, allowBreakingChanges bool) error

	// Delete removes a CRD if it exists
	Delete(ctx context.Context, name string) error

	// Get retrieves a CRD by name
	Get(ctx context.Context, name string) (*v1.CustomResourceDefinition, error)
}

// CRDInterface provides a simplified interface for CRD operations
type CRDInterface interface {
	// Ensure ensures a CRD exists, up-to-date, and is ready. This can be
	// a dangerous operation as it will update the CRD if it already exists.
	//
	// If allowBreakingChanges is false and the update contains breaking schema
	// changes, an error is returned. Set to true to force the update anyway.
	Ensure(ctx context.Context, crd v1.CustomResourceDefinition, allowBreakingChanges bool) error

	// Get retrieves a CRD by name
	Get(ctx context.Context, name string) (*v1.CustomResourceDefinition, error)

	// Delete removes a CRD if it exists
	Delete(ctx context.Context, name string) error
}

// CRDWrapper provides a simplified interface for CRD operations
type CRDWrapper struct {
	client       apiextensionsv1.CustomResourceDefinitionInterface
	pollInterval time.Duration
	timeout      time.Duration
}

var _ CRDInterface = (*CRDWrapper)(nil)

// CRDWrapperConfig contains configuration for the CRD wrapper
type CRDWrapperConfig struct {
	Client       apiextensionsv1.ApiextensionsV1Interface
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
		pollInterval: cfg.PollInterval,
		timeout:      cfg.Timeout,
	}
}

// Ensure ensures a CRD exists, up-to-date, and is ready. This can be
// a dangerous operation as it will update the CRD if it already exists.
//
// If a CRD does exist, it will compare the existing CRD with the desired CRD
// and update it if necessary. If the existing CRD has breaking changes and
// allowBreakingChanges is false, it will return an error.
func (w *CRDWrapper) Ensure(ctx context.Context, desired v1.CustomResourceDefinition, allowBreakingChanges bool) error {
	log := logr.FromContext(ctx)
	existing, err := w.Get(ctx, desired.Name)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to check for existing CRD: %w", err)
		}

		log.Info("Creating CRD", "name", desired.Name)
		if err := w.create(ctx, desired); err != nil {
			return fmt.Errorf("failed to create CRD: %w", err)
		}
	} else {
		// Check ownership first
		kroOwned, nameMatch, idMatch := metadata.CompareRGDOwnership(existing.ObjectMeta, desired.ObjectMeta)
		if !kroOwned {
			return fmt.Errorf(
				"failed to update CRD %s: CRD already exists and is not owned by KRO", desired.Name,
			)
		}

		if !nameMatch {
			existingRGDName := existing.Labels[metadata.ResourceGraphDefinitionNameLabel]
			return fmt.Errorf(
				"failed to update CRD %s: CRD is owned by another ResourceGraphDefinition %s",
				desired.Name, existingRGDName,
			)
		}

		if nameMatch && !idMatch {
			log.Info(
				"Adopting CRD with different RGD ID - RGD may have been deleted and recreated",
				"crd", desired.Name,
				"existingRGDID", existing.Labels[metadata.ResourceGraphDefinitionIDLabel],
				"newRGDID", desired.Labels[metadata.ResourceGraphDefinitionIDLabel],
			)
		}

		// Preserve old CRD versions not present in the desired spec
		w.mergeVersions(&desired, existing)

		// Check for breaking schema changes
		report, err := crdcompat.CompareVersions(existing.Spec.Versions, desired.Spec.Versions)
		if err != nil {
			return fmt.Errorf("failed to check schema compatibility: %w", err)
		}

		// If there are no changes at all, we can skip the update
		if !report.HasChanges() {
			log.V(1).Info("CRD is up-to-date", "name", desired.Name)
			return nil
		}

		// Check for breaking changes
		if !report.IsCompatible() {
			log.Info("Breaking changes detected in CRD update", "name", desired.Name, "breakingChanges", len(report.BreakingChanges), "summary", report)
			if !allowBreakingChanges {
				return fmt.Errorf("cannot update CRD %s: breaking changes detected: %s", desired.Name, report)
			}
			log.Info("Allowing breaking changes", "name", desired.Name)
		}

		log.Info("Updating existing CRD", "name", desired.Name)
		if err := w.patch(ctx, desired); err != nil {
			return fmt.Errorf("failed to patch CRD: %w", err)
		}
	}

	return w.waitForReady(ctx, desired.Name)
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
	log := logr.FromContext(ctx)
	log.Info("Deleting CRD", "name", name)

	err := w.client.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete CRD: %w", err)
	}
	return nil
}

// waitForReady waits for a CRD to become ready
func (w *CRDWrapper) waitForReady(ctx context.Context, name string) error {
	log := logr.FromContext(ctx)
	log.Info("Waiting for CRD to become ready", "name", name)

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

// mergeVersions preserves old CRD versions that are not in the new CRD spec,
// preventing status.storedVersions validation errors during updates.
func (w *CRDWrapper) mergeVersions(newCRD, oldCRD *v1.CustomResourceDefinition) {
	newVersions := make(map[string]bool)
	for _, v := range newCRD.Spec.Versions {
		newVersions[v.Name] = true
	}

	for _, oldVer := range oldCRD.Spec.Versions {
		if !newVersions[oldVer.Name] {
			preservedVer := oldVer
			preservedVer.Storage = false
			newCRD.Spec.Versions = append(newCRD.Spec.Versions, preservedVer)
		}
	}
}