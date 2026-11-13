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

package resourcegraphdefinition

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"

	featuregatetesting "k8s.io/component-base/featuregate/testing"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	krofake "github.com/kubernetes-sigs/kro/pkg/client/fake"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

const migrationTestHub = "v1beta1"

// migrationTestCRD is a two-version instance CRD that still stores objects
// under the retired version.
func migrationTestCRD() *extv1.CustomResourceDefinition {
	return &extv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "networks.example.io"},
		Spec: extv1.CustomResourceDefinitionSpec{
			Group: "example.io",
			Names: extv1.CustomResourceDefinitionNames{Kind: "Network", Plural: "networks"},
			Versions: []extv1.CustomResourceDefinitionVersion{
				{Name: migrationTestHub, Served: true, Storage: true},
				{Name: "v1alpha1", Served: true},
			},
		},
		Status: extv1.CustomResourceDefinitionStatus{
			StoredVersions: []string{"v1alpha1", migrationTestHub},
		},
	}
}

func migrationTestRGD() *v1alpha1.ResourceGraphDefinition {
	return &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "networks", UID: "rgd-uid"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{Group: "example.io", APIVersion: migrationTestHub, Kind: "Network"},
		},
	}
}

// newMigrationReconciler wires a reconciler whose cluster serves the storage
// version migration API when serveMigrationAPI is set, with one instance
// stored under the hub version and a registered parent watch for it.
func newMigrationReconciler(
	t testing.TB,
	serveMigrationAPI bool,
	migrations ...runtime.Object,
) (*ResourceGraphDefinitionReconciler, *stubCRDManager, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	// The kro rewrite pass reads instances from the parent informer, which
	// only carries full objects — and therefore the seeded instance — when
	// revision pinning is on.
	featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, true)

	hubGVR := instanceGVRFor(migrationTestHub)
	instance := newInstance("stored-instance", 0, "")
	instance.SetAPIVersion("example.io/" + migrationTestHub)

	objects := append([]runtime.Object{instance}, migrations...)
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			hubGVR:                     "NetworkList",
			storageVersionMigrationGVR: "StorageVersionMigrationList",
		},
		objects...,
	)

	kubeClient := kubefake.NewSimpleClientset()
	if serveMigrationAPI {
		kubeClient.Resources = []*metav1.APIResourceList{{
			GroupVersion: storageVersionMigrationGroupVersion,
			APIResources: []metav1.APIResource{{Name: storageVersionMigrationGVR.Resource}},
		}}
	}

	clientSet := krofake.NewFakeSet(dynamicClient)
	clientSet.KubernetesClient = kubeClient

	dc := newRunningDynamicControllerWithInstances(t, []runtime.Object{instance}, hubGVR)
	require.NoError(t, dc.Register(context.Background(), hubGVR, func(context.Context, ctrl.Request) error { return nil }))

	crdManager := &stubCRDManager{getReturn: migrationTestCRD()}
	reconciler := &ResourceGraphDefinitionReconciler{
		clientSet:         clientSet,
		crdManager:        crdManager,
		dynamicController: dc,
		metadataLabeler:   metadata.NewKROMetaLabeler(),
		cfg:               Config{StorageMigrationTimeout: time.Hour},
	}
	return reconciler, crdManager, dynamicClient
}

func migrationObject(name string, age time.Duration, conditionType string) *unstructured.Unstructured {
	migration := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": storageVersionMigrationGroupVersion,
		"kind":       "StorageVersionMigration",
		"metadata": map[string]any{
			"name":              name,
			"creationTimestamp": metav1.NewTime(time.Now().Add(-age)).UTC().Format(time.RFC3339),
		},
		"spec": map[string]any{
			"resource": map[string]any{"group": "example.io", "resource": "networks"},
		},
	}}
	if conditionType != "" {
		_ = unstructured.SetNestedSlice(migration.Object, []any{
			map[string]any{"type": conditionType, "status": "True", "message": "from test"},
		}, "status", "conditions")
	}
	return migration
}

func TestStorageMigrationCreatesMigrationObject(t *testing.T) {
	reconciler, crdManager, dynamicClient := newMigrationReconciler(t, true)

	done, err := reconciler.reconcileStorageMigration(context.Background(), migrationTestRGD())
	require.NoError(t, err)
	assert.False(t, done, "migration is still in flight")
	assert.Empty(t, crdManager.storedVersions, "stored versions must not be trimmed before the migration finishes")

	name := storageVersionMigrationName(migrationTestHub, "networks.example.io")
	created, err := dynamicClient.Resource(storageVersionMigrationGVR).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	resource, found, err := unstructured.NestedMap(created.Object, "spec", "resource")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, map[string]any{"group": "example.io", "resource": "networks"}, resource)
	assert.Equal(t, "networks", created.GetLabels()[metadata.ResourceGraphDefinitionNameLabel])
}

func TestStorageMigrationSucceeded(t *testing.T) {
	name := storageVersionMigrationName(migrationTestHub, "networks.example.io")
	reconciler, crdManager, dynamicClient := newMigrationReconciler(t, true,
		migrationObject(name, time.Minute, "Succeeded"))

	done, err := reconciler.reconcileStorageMigration(context.Background(), migrationTestRGD())
	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, []string{migrationTestHub}, crdManager.storedVersions["networks.example.io"])

	_, err = dynamicClient.Resource(storageVersionMigrationGVR).Get(context.Background(), name, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "a finished migration object is removed")
}

func TestStorageMigrationFallsBackWhenTheMigratorStalls(t *testing.T) {
	name := storageVersionMigrationName(migrationTestHub, "networks.example.io")
	reconciler, crdManager, dynamicClient := newMigrationReconciler(t, true,
		migrationObject(name, time.Hour, ""))
	reconciler.cfg.StorageMigrationTimeout = time.Minute

	done, err := reconciler.reconcileStorageMigration(context.Background(), migrationTestRGD())
	require.NoError(t, err)
	assert.True(t, done, "kro migrates the objects itself once the migrator misses its deadline")
	assert.Equal(t, []string{migrationTestHub}, crdManager.storedVersions["networks.example.io"])

	_, err = dynamicClient.Resource(storageVersionMigrationGVR).Get(context.Background(), name, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "the stalled migration object is removed")

	// A later migration of the same CRD does not wait for the migrator again.
	assert.True(t, reconciler.migrationStalled("networks.example.io"))
}

func TestStorageMigrationWaitsWhileTheMigratorIsWithinItsDeadline(t *testing.T) {
	name := storageVersionMigrationName(migrationTestHub, "networks.example.io")
	reconciler, crdManager, _ := newMigrationReconciler(t, true,
		migrationObject(name, time.Second, ""))

	done, err := reconciler.reconcileStorageMigration(context.Background(), migrationTestRGD())
	require.NoError(t, err)
	assert.False(t, done)
	assert.Empty(t, crdManager.storedVersions)
	assert.False(t, reconciler.migrationStalled("networks.example.io"))
}

func TestStorageMigrationWithoutClusterAPI(t *testing.T) {
	reconciler, crdManager, dynamicClient := newMigrationReconciler(t, false)

	done, err := reconciler.reconcileStorageMigration(context.Background(), migrationTestRGD())
	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, []string{migrationTestHub}, crdManager.storedVersions["networks.example.io"])

	// Every stored instance is rewritten through the hub version.
	assert.True(t, instancePatched(dynamicClient, "stored-instance"),
		"the kro pass must rewrite each instance through the hub version")
}

// instancePatched reports whether the fake dynamic client saw a patch of the
// named instance on the hub version's resource.
func instancePatched(dynamicClient *dynamicfake.FakeDynamicClient, name string) bool {
	hubGVR := instanceGVRFor(migrationTestHub)
	for _, action := range dynamicClient.Actions() {
		patch, ok := action.(clienttesting.PatchAction)
		if !ok {
			continue
		}
		if patch.GetResource() == hubGVR && patch.GetName() == name {
			return true
		}
	}
	return false
}

func TestStorageMigrationNoopWhenOnlyTheHubIsStored(t *testing.T) {
	reconciler, crdManager, _ := newMigrationReconciler(t, true)
	reconciler.crdManager.(*stubCRDManager).getReturn.Status.StoredVersions = []string{migrationTestHub}

	done, err := reconciler.reconcileStorageMigration(context.Background(), migrationTestRGD())
	require.NoError(t, err)
	assert.True(t, done)
	assert.Empty(t, crdManager.storedVersions, "nothing to migrate, nothing to write")
}
