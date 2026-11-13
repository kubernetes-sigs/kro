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

package resourcegraphdefinition

import (
	"context"
	"fmt"
	"strings"

	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// storageVersionMigrationGVR is the cluster-scoped resource the in-tree
// storage version migrator reconciles. The typed v1 API is not in the
// Kubernetes libraries kro builds against, so kro drives it unstructured.
var storageVersionMigrationGVR = schema.GroupVersionResource{
	Group:    "storagemigration.k8s.io",
	Version:  "v1",
	Resource: "storageversionmigrations",
}

const storageVersionMigrationGroupVersion = "storagemigration.k8s.io/v1"

// reconcileStorageMigration rewrites every instance that is still stored under
// a retired version, so the old version can stop being served.
//
// It prefers the API server's own storage version migrator, which exists from
// Kubernetes 1.33 onwards. When that API is absent, or the migration it ran
// failed, kro rewrites the objects itself by re-applying them through the hub
// version.
//
// done is false while a migration is still in flight; the caller requeues.
func (r *ResourceGraphDefinitionReconciler) reconcileStorageMigration(
	ctx context.Context,
	rgd *v1alpha1.ResourceGraphDefinition,
) (bool, error) {
	log := ctrl.LoggerFrom(ctx)

	crdName := extractCRDName(rgd.Spec.Schema)
	crd, err := r.crdManager.Get(ctx, crdName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("getting CRD %s for storage migration: %w", crdName, err)
	}

	hub := ""
	for _, version := range crd.Spec.Versions {
		if version.Storage {
			hub = version.Name
			break
		}
	}
	if hub == "" {
		return true, nil
	}

	migrationName := storageVersionMigrationName(hub, crd.Name)
	stored := crd.Status.StoredVersions
	if len(stored) == 0 || (len(stored) == 1 && stored[0] == hub) {
		// Nothing is stored under a retired version. Any migration object kro
		// created was already deleted when the migration finished, so this
		// path makes no API calls.
		return true, nil
	}

	if r.storageVersionMigrationAvailable() {
		done, err := r.reconcileMigrationObject(ctx, crd, hub, migrationName)
		if err != nil {
			return false, err
		}
		if !done {
			return false, nil
		}
		log.V(1).Info("storage version migration completed", "crd", crd.Name, "version", hub)
	} else {
		done, err := r.rewriteInstances(ctx, crd, hub)
		if err != nil {
			return false, err
		}
		if !done {
			// No synced informer yet: retry on the next pass.
			return false, nil
		}
	}

	if err := r.crdManager.SetStoredVersions(ctx, crd.Name, []string{hub}); err != nil {
		return false, fmt.Errorf("setting stored versions of CRD %s: %w", crd.Name, err)
	}
	r.deleteStorageVersionMigration(ctx, migrationName)
	return true, nil
}

// reconcileMigrationObject drives one StorageVersionMigration object through
// its lifecycle. It returns done once the migration succeeded; a failed
// migration is deleted and reported as not done so the kro pass takes over on
// the next reconcile.
func (r *ResourceGraphDefinitionReconciler) reconcileMigrationObject(
	ctx context.Context,
	crd *v1.CustomResourceDefinition,
	hub, migrationName string,
) (bool, error) {
	log := ctrl.LoggerFrom(ctx)
	client := r.clientSet.Dynamic().Resource(storageVersionMigrationGVR)

	existing, err := client.Get(ctx, migrationName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		migration := newStorageVersionMigration(migrationName, crd)
		r.metadataLabeler.ApplyLabels(migration)
		labels := migration.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[metadata.ResourceGraphDefinitionNameLabel] = crd.Spec.Names.Plural
		migration.SetLabels(labels)

		if _, err := client.Create(ctx, migration, metav1.CreateOptions{}); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return false, nil
			}
			return false, fmt.Errorf("creating storage version migration %s: %w", migrationName, err)
		}
		log.V(1).Info("created storage version migration", "name", migrationName, "version", hub)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting storage version migration %s: %w", migrationName, err)
	}

	switch state, message := migrationState(existing); state {
	case "Succeeded":
		return true, nil
	case "Failed":
		log.Info("storage version migration failed; migrating through kro instead",
			"name", migrationName, "message", message)
		r.deleteStorageVersionMigration(ctx, migrationName)
		return r.rewriteInstances(ctx, crd, hub)
	default:
		return false, nil
	}
}

// rewriteInstances re-writes every cached instance through the hub version so
// the API server stores it under the hub's encoding. It is idempotent, so a
// partial pass simply resumes on the next reconcile. done is false when the
// instance cache is not available yet.
func (r *ResourceGraphDefinitionReconciler) rewriteInstances(
	ctx context.Context,
	crd *v1.CustomResourceDefinition,
	hub string,
) (bool, error) {
	gvr := schema.GroupVersionResource{
		Group:    crd.Spec.Group,
		Version:  hub,
		Resource: crd.Spec.Names.Plural,
	}
	keys, ok := r.dynamicController.InstanceKeys(gvr)
	if !ok {
		return false, nil
	}

	client := r.clientSet.Dynamic().Resource(gvr)
	for _, key := range keys {
		var err error
		if key.Namespace != "" {
			_, err = client.Namespace(key.Namespace).Patch(ctx, key.Name, types.MergePatchType, []byte("{}"), metav1.PatchOptions{})
		} else {
			_, err = client.Patch(ctx, key.Name, types.MergePatchType, []byte("{}"), metav1.PatchOptions{})
		}
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return false, fmt.Errorf("rewriting instance %s: %w", key, err)
		}
	}
	return true, nil
}

// deleteStorageVersionMigration removes a finished migration object. Failures
// are ignored: a leftover object is harmless and the next reconcile retries.
func (r *ResourceGraphDefinitionReconciler) deleteStorageVersionMigration(ctx context.Context, name string) {
	if !r.storageVersionMigrationAvailable() {
		return
	}
	err := r.clientSet.Dynamic().Resource(storageVersionMigrationGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		ctrl.LoggerFrom(ctx).V(1).Info("failed to delete storage version migration", "name", name, "error", err)
	}
}

// storageVersionMigrationAvailable reports whether the cluster serves
// storagemigration.k8s.io/v1. The answer is discovered once: a cluster does
// not gain or lose a built-in API group without an API server restart, and
// this runs on every RGD reconcile.
func (r *ResourceGraphDefinitionReconciler) storageVersionMigrationAvailable() bool {
	r.storageMigrationAPIOnce.Do(func() {
		resources, err := r.clientSet.Kubernetes().Discovery().ServerResourcesForGroupVersion(storageVersionMigrationGroupVersion)
		if err != nil || resources == nil {
			return
		}
		for _, resource := range resources.APIResources {
			if resource.Name == storageVersionMigrationGVR.Resource {
				r.storageMigrationAPI = true
				return
			}
		}
	})
	return r.storageMigrationAPI
}

// newStorageVersionMigration builds the migration object for one CRD.
func newStorageVersionMigration(name string, crd *v1.CustomResourceDefinition) *unstructured.Unstructured {
	migration := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": storageVersionMigrationGroupVersion,
		"kind":       "StorageVersionMigration",
		"metadata": map[string]any{
			"name": name,
		},
		"spec": map[string]any{
			"resource": map[string]any{
				"group":    crd.Spec.Group,
				"resource": crd.Spec.Names.Plural,
			},
		},
	}}
	migration.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: v1.SchemeGroupVersion.String(),
		Kind:       "CustomResourceDefinition",
		Name:       crd.Name,
		UID:        crd.UID,
	}})
	return migration
}

// migrationState returns the terminal condition of a StorageVersionMigration,
// or "" while it is still running.
func migrationState(migration *unstructured.Unstructured) (string, string) {
	conditions, found, err := unstructured.NestedSlice(migration.Object, "status", "conditions")
	if err != nil || !found {
		return "", ""
	}
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		conditionType, _ := condition["type"].(string)
		status, _ := condition["status"].(string)
		if status != string(metav1.ConditionTrue) {
			continue
		}
		if conditionType == "Succeeded" || conditionType == "Failed" {
			message, _ := condition["message"].(string)
			return conditionType, message
		}
	}
	return "", ""
}

// storageVersionMigrationName builds a deterministic, length-bounded object
// name for the migration of one CRD to one storage version.
func storageVersionMigrationName(hub, crdName string) string {
	name := "kro-" + hub + "-" + crdName
	if len(name) > 253 {
		name = name[:253]
	}
	return strings.TrimRight(name, "-.")
}
