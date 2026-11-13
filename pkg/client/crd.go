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
	"errors"
	"fmt"
	"time"

	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/util/retry"
	logr "sigs.k8s.io/controller-runtime/pkg/log"

	crdcompat "github.com/kubernetes-sigs/kro/pkg/graph/crd/compat"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

const (
	// DefaultPollInterval is the default interval for polling CRD status
	defaultPollInterval = 150 * time.Millisecond
	// DefaultTimeout is the default timeout for waiting for CRD status
	defaultTimeout = 2 * time.Minute
)

var _ CRDClient = &CRDWrapper{}

// ErrVersionRemovalBlocked is returned when a CRD update would stop serving a
// version that is still stored or still referenced by a pinned graph revision.
// It is never overridable by the allow-breaking-changes annotation.
var ErrVersionRemovalBlocked = errors.New("version removal blocked")

// ErrSpokeSchemaMismatch is returned when a non-storage version's schema
// cannot serve what the storage version accepts. With conversion strategy None
// the API server returns the stored object verbatim under every version, so
// such a spoke would fail to read existing objects.
var ErrSpokeSchemaMismatch = errors.New("spoke schema mismatch")

// EnsureOptions tunes the rules Ensure applies to a CRD update.
type EnsureOptions struct {
	// AllowBreakingChanges permits schema changes that would otherwise be
	// rejected. It never permits removing a stored or in-use version, nor a
	// backwards move of the storage version.
	AllowBreakingChanges bool
	// RejectDefaultChanges rejects changes to schema defaults. Set under the
	// Manual rollout strategy, where a default change would silently alter
	// instances still pinned to an older graph revision.
	RejectDefaultChanges bool
	// MultiVersion allows the storage version to move forward in Kubernetes
	// version priority. Without it, schema.apiVersion is immutable.
	MultiVersion bool
	// VersionsInUse reports the instance API versions pinned graph revisions
	// still reconcile against. May be nil; it is called at most once, and
	// only when a version would stop being served.
	VersionsInUse func() (sets.Set[string], error)
}

// CRDClient represents operations for managing CustomResourceDefinitions
type CRDClient interface {
	// EnsureCreated ensures a CRD exists and is ready
	Ensure(ctx context.Context, crd v1.CustomResourceDefinition, opts EnsureOptions) error

	// Delete removes a CRD if it exists
	Delete(ctx context.Context, name string) error

	// Get retrieves a CRD by name
	Get(ctx context.Context, name string) (*v1.CustomResourceDefinition, error)

	// SetStoredVersions replaces status.storedVersions on a CRD.
	SetStoredVersions(ctx context.Context, name string, versions []string) error
}

// CRDInterface provides a simplified interface for CRD operations
type CRDInterface interface {
	// Ensure ensures a CRD exists, up-to-date, and is ready. This can be
	// a dangerous operation as it will update the CRD if it already exists.
	//
	// Breaking schema changes are rejected unless opts.AllowBreakingChanges is
	// set. Version removal and backwards storage-version moves are rejected
	// unconditionally.
	Ensure(ctx context.Context, crd v1.CustomResourceDefinition, opts EnsureOptions) error

	// Get retrieves a CRD by name
	Get(ctx context.Context, name string) (*v1.CustomResourceDefinition, error)

	// Delete removes a CRD if it exists
	Delete(ctx context.Context, name string) error

	// SetStoredVersions replaces status.storedVersions on a CRD. It is used
	// after a storage version migration to retire the old version.
	SetStoredVersions(ctx context.Context, name string, versions []string) error
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
// If a CRD does exist, it compares the existing CRD with the desired one and
// patches it when they differ, after checking, in order:
//
//  1. every desired non-storage version can serve objects stored under the
//     storage version (conversion strategy is None);
//  2. the storage version only moves forward, and only with MultiVersion;
//  3. no version that is stored or in use stops being served;
//  4. no incompatible edit to a non-storage version;
//  5. no breaking change to the storage version;
//  6. no default change when RejectDefaultChanges is set.
func (w *CRDWrapper) Ensure(ctx context.Context, desired v1.CustomResourceDefinition, opts EnsureOptions) error {
	log := logr.FromContext(ctx)

	// Applies to creates too: a spoke that cannot serve hub-stored objects is
	// broken from the first object onwards.
	if err := checkSpokesServeStorage(desired, opts); err != nil {
		return err
	}

	existing, err := w.Get(ctx, desired.Name)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to check for existing CRD: %w", err)
		}

		log.Info("Creating CRD", "name", desired.Name)
		if err := w.create(ctx, desired); err != nil {
			return fmt.Errorf("failed to create CRD: %w", err)
		}
		return w.waitForReady(ctx, desired.Name)
	}

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

	if !idMatch {
		log.Info(
			"Adopting CRD with different RGD ID - RGD may have been deleted and recreated",
			"crd", desired.Name,
			"existingRGDID", existing.Labels[metadata.ResourceGraphDefinitionIDLabel],
			"newRGDID", desired.Labels[metadata.ResourceGraphDefinitionIDLabel],
		)
	}

	oldStorage := storageVersion(existing)
	newStorage := storageVersion(&desired)
	if oldStorage != newStorage {
		if !opts.MultiVersion {
			return fmt.Errorf(
				"cannot update CRD %s: schema.apiVersion is immutable (current %s, desired %s) unless the MultiVersionSchema feature gate is enabled",
				desired.Name, oldStorage, newStorage,
			)
		}
		if version.CompareKubeAwareVersionStrings(newStorage, oldStorage) <= 0 {
			return fmt.Errorf(
				"cannot update CRD %s: schema.apiVersion may only move forward in Kubernetes version priority (current %s, desired %s)",
				desired.Name, oldStorage, newStorage,
			)
		}
	}

	// Check for breaking schema changes
	report, err := crdcompat.CompareVersions(existing.Spec.Versions, desired.Spec.Versions)
	if err != nil {
		return fmt.Errorf("failed to check schema compatibility: %w", err)
	}

	if err := w.checkRetiredVersions(existing, desired, report, opts); err != nil {
		return err
	}

	for name, versionReport := range report.Versions {
		if name == newStorage || versionReport.IsCompatible() {
			continue
		}
		if !opts.AllowBreakingChanges {
			return fmt.Errorf("%w: version %s of CRD %s differs incompatibly from the served schema: %s",
				ErrSpokeSchemaMismatch, name, desired.Name, versionReport)
		}
		log.Info("Allowing breaking changes to a non-storage version", "name", desired.Name, "version", name)
	}

	if storageReport, ok := report.Versions[newStorage]; ok && !storageReport.IsCompatible() {
		log.Info("Breaking changes detected in CRD update", "name", desired.Name, "breakingChanges", len(storageReport.BreakingChanges), "summary", storageReport)
		if !opts.AllowBreakingChanges {
			return fmt.Errorf("cannot update CRD %s: breaking changes detected: %s", desired.Name, storageReport)
		}
		log.Info("Allowing breaking changes", "name", desired.Name)
	}

	if opts.RejectDefaultChanges && !opts.AllowBreakingChanges {
		for name, versionReport := range report.Versions {
			for _, change := range versionReport.NonBreakingChanges {
				if change.ChangeType != crdcompat.DefaultChanged {
					continue
				}
				return fmt.Errorf(
					"cannot update CRD %s: default changes are not allowed with rollout strategy Manual: version %s: %s",
					desired.Name, name, change.Description(),
				)
			}
		}
	}

	// If there are no changes at all, we can skip the update. The API server
	// defaults an omitted conversion to None, so compare the effective
	// strategies rather than the pointers: otherwise every single-version CRD
	// looks changed on every reconcile.
	namesChanged := !equality.Semantic.DeepEqual(existing.Spec.Names, desired.Spec.Names)
	conversionChanged := conversionStrategy(existing.Spec.Conversion) != conversionStrategy(desired.Spec.Conversion)
	if !report.HasChanges() && !namesChanged && !conversionChanged {
		log.V(1).Info("CRD is up-to-date", "name", desired.Name)
		return nil
	}

	log.Info("Updating existing CRD", "name", desired.Name)
	if err := w.patch(ctx, desired); err != nil {
		return fmt.Errorf("failed to patch CRD: %w", err)
	}

	return w.waitForReady(ctx, desired.Name)
}

// checkSpokesServeStorage rejects a desired CRD whose non-storage versions
// cannot serve objects stored under its storage version. kro converts with
// strategy None, so the API server hands the stored object to every served
// version unchanged.
func checkSpokesServeStorage(desired v1.CustomResourceDefinition, opts EnsureOptions) error {
	if len(desired.Spec.Versions) < 2 {
		return nil
	}
	var hub *v1.CustomResourceDefinitionVersion
	for i := range desired.Spec.Versions {
		if desired.Spec.Versions[i].Storage {
			hub = &desired.Spec.Versions[i]
			break
		}
	}
	if hub == nil {
		return fmt.Errorf("cannot update CRD %s: no storage version declared", desired.Name)
	}
	for i := range desired.Spec.Versions {
		spoke := desired.Spec.Versions[i]
		if spoke.Storage {
			continue
		}
		report, err := crdcompat.CompareAcrossVersions(spoke, *hub)
		if err != nil {
			return fmt.Errorf("failed to check schema compatibility: %w", err)
		}
		if report.IsCompatible() || opts.AllowBreakingChanges {
			continue
		}
		return fmt.Errorf(
			"cannot update CRD %s: version %s cannot be converted to %s without a conversion webhook: %s",
			desired.Name, spoke.Name, hub.Name, report,
		)
	}
	return nil
}

// checkRetiredVersions rejects removing — or ceasing to serve — any version
// that the API server still lists as stored, or that a pinned graph revision
// still reconciles against. Neither is overridable.
func (w *CRDWrapper) checkRetiredVersions(
	existing *v1.CustomResourceDefinition,
	desired v1.CustomResourceDefinition,
	report *crdcompat.VersionsReport,
	opts EnsureOptions,
) error {
	retired := append([]string{}, report.Removed...)
	desiredByName := make(map[string]v1.CustomResourceDefinitionVersion, len(desired.Spec.Versions))
	for _, version := range desired.Spec.Versions {
		desiredByName[version.Name] = version
	}
	for _, old := range existing.Spec.Versions {
		if next, ok := desiredByName[old.Name]; ok && old.Served && !next.Served {
			retired = append(retired, old.Name)
		}
	}
	if len(retired) == 0 {
		return nil
	}

	stored := sets.New(existing.Status.StoredVersions...)
	var inUse sets.Set[string]
	for _, name := range retired {
		if stored.Has(name) {
			return fmt.Errorf("%w: version %s of CRD %s is still listed in status.storedVersions",
				ErrVersionRemovalBlocked, name, desired.Name)
		}
		if opts.VersionsInUse == nil {
			continue
		}
		if inUse == nil {
			used, err := opts.VersionsInUse()
			if err != nil {
				return fmt.Errorf("failed to determine versions in use for CRD %s: %w", desired.Name, err)
			}
			inUse = used
		}
		if inUse.Has(name) {
			return fmt.Errorf("%w: version %s of CRD %s is still used by a pinned graph revision",
				ErrVersionRemovalBlocked, name, desired.Name)
		}
	}
	return nil
}

// storageVersion returns the name of the CRD's storage version, or "" when
// none is marked.
func storageVersion(crd *v1.CustomResourceDefinition) string {
	for _, v := range crd.Spec.Versions {
		if v.Storage {
			return v.Name
		}
	}
	return ""
}

// conversionStrategy returns a CRD's effective conversion strategy. A nil
// conversion means the API server's default, None.
func conversionStrategy(conversion *v1.CustomResourceConversion) v1.ConversionStrategyType {
	if conversion == nil || conversion.Strategy == "" {
		return v1.NoneConverter
	}
	return conversion.Strategy
}

// SetStoredVersions replaces status.storedVersions on a CRD. The API server
// only ever appends to that list; trimming it after a storage migration is the
// migrator's job, and here kro's.
func (w *CRDWrapper) SetStoredVersions(ctx context.Context, name string, versions []string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		crd, err := w.client.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if equality.Semantic.DeepEqual(crd.Status.StoredVersions, versions) {
			return nil
		}
		crd.Status.StoredVersions = versions
		_, err = w.client.UpdateStatus(ctx, crd, metav1.UpdateOptions{})
		return err
	})
}

// Get retrieves a CRD by name
func (w *CRDWrapper) Get(ctx context.Context, name string) (*v1.CustomResourceDefinition, error) {
	return w.client.Get(ctx, name, metav1.GetOptions{})
}

func (w *CRDWrapper) create(ctx context.Context, crd v1.CustomResourceDefinition) error {
	_, err := w.client.Create(ctx, &crd, metav1.CreateOptions{})
	return err
}

func (w *CRDWrapper) patch(ctx context.Context, crd v1.CustomResourceDefinition) error {
	patchBytes, err := crdMergePatch(crd)
	if err != nil {
		return err
	}

	_, err = w.client.Patch(ctx, crd.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	return err
}

func crdMergePatch(crd v1.CustomResourceDefinition) ([]byte, error) {
	// The desired CRD is used as a JSON merge-patch document, not as the full
	// post-patch object. Omitted fields preserve existing/defaulted values. The
	// alias fields need explicit nulls because their CRD type uses omitempty.
	patch := map[string]any{
		"spec": map[string]any{
			"group": crd.Spec.Group,
			"names": map[string]any{
				"plural":     crd.Spec.Names.Plural,
				"singular":   crd.Spec.Names.Singular,
				"kind":       crd.Spec.Names.Kind,
				"listKind":   crd.Spec.Names.ListKind,
				"shortNames": crd.Spec.Names.ShortNames,
				"categories": crd.Spec.Names.Categories,
			},
			"scope":    crd.Spec.Scope,
			"versions": crd.Spec.Versions,
		},
	}

	if len(crd.Labels) > 0 || len(crd.Annotations) > 0 {
		metadata := map[string]any{}
		if len(crd.Labels) > 0 {
			metadata["labels"] = crd.Labels
		}
		if len(crd.Annotations) > 0 {
			metadata["annotations"] = crd.Annotations
		}
		patch["metadata"] = metadata
	}

	if crd.Spec.Conversion != nil || crd.Spec.PreserveUnknownFields {
		spec := patch["spec"].(map[string]any)
		if crd.Spec.Conversion != nil {
			spec["conversion"] = crd.Spec.Conversion
		}
		if crd.Spec.PreserveUnknownFields {
			spec["preserveUnknownFields"] = crd.Spec.PreserveUnknownFields
		}
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal CRD merge patch: %w", err)
	}
	return patchBytes, nil
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
