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
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	krofake "github.com/kubernetes-sigs/kro/pkg/client/fake"
	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

type stubCRDManager struct {
	ensureErr         error
	deleteErr         error
	getErr            error
	getReturn         *extv1.CustomResourceDefinition
	ensureCalls       int
	lastAllowBreaking bool
	lastEnsure        extv1.CustomResourceDefinition
	deleted           []string
	requestedCRDNames []string
}

type stubManager struct {
	manager.Manager
	client            client.Client
	restMapper        apimeta.RESTMapper
	logger            logr.Logger
	scheme            *runtime.Scheme
	controllerOptions config.Controller
	cache             cache.Cache
	addErr            error
	addCalls          int
	lastRunnable      manager.Runnable
}

type stubGraphBuilder struct {
	build func(*v1alpha1.ResourceGraphDefinition, graph.RGDConfig) (*graph.Graph, error)
}

type stubHandlerRegistration struct{}

type stubInformer struct {
	cache.Informer
	mu       sync.Mutex
	handlers []toolscache.ResourceEventHandler
	stopped  bool
}

type stubCache struct {
	cache.Cache
	rgdInformer *stubInformer
	crdInformer *stubInformer
}

func (s *stubCRDManager) Ensure(_ context.Context, crd extv1.CustomResourceDefinition, allowBreakingChanges bool) error {
	s.ensureCalls++
	s.lastEnsure = *crd.DeepCopy()
	s.lastAllowBreaking = allowBreakingChanges
	return s.ensureErr
}

func (s *stubCRDManager) Delete(_ context.Context, name string) error {
	s.deleted = append(s.deleted, name)
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return nil
}

func (s *stubCRDManager) Get(_ context.Context, name string) (*extv1.CustomResourceDefinition, error) {
	s.requestedCRDNames = append(s.requestedCRDNames, name)
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.getReturn != nil {
		return s.getReturn.DeepCopy(), nil
	}
	return &extv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: extv1.CustomResourceDefinitionStatus{
			AcceptedNames: extv1.CustomResourceDefinitionNames{Kind: "Network"},
		},
	}, nil
}

func (m *stubManager) Add(r manager.Runnable) error {
	m.addCalls++
	m.lastRunnable = r
	return m.addErr
}

func (m *stubManager) GetClient() client.Client {
	return m.client
}

func (m *stubManager) GetRESTMapper() apimeta.RESTMapper {
	return m.restMapper
}

func (m *stubManager) GetLogger() logr.Logger {
	return m.logger
}

func (m *stubManager) GetScheme() *runtime.Scheme {
	return m.scheme
}

func (m *stubManager) GetControllerOptions() config.Controller {
	return m.controllerOptions
}

func (m *stubManager) GetCache() cache.Cache {
	return m.cache
}

func (s *stubGraphBuilder) NewResourceGraphDefinition(rgd *v1alpha1.ResourceGraphDefinition, config graph.RGDConfig) (*graph.Graph, error) {
	return s.build(rgd, config)
}

func (stubHandlerRegistration) HasSynced() bool {
	return true
}

func (s *stubInformer) AddEventHandler(handler toolscache.ResourceEventHandler) (toolscache.ResourceEventHandlerRegistration, error) {
	return s.AddEventHandlerWithOptions(handler, toolscache.HandlerOptions{})
}

func (s *stubInformer) AddEventHandlerWithResyncPeriod(handler toolscache.ResourceEventHandler, _ time.Duration) (toolscache.ResourceEventHandlerRegistration, error) {
	return s.AddEventHandlerWithOptions(handler, toolscache.HandlerOptions{})
}

func (s *stubInformer) AddEventHandlerWithOptions(handler toolscache.ResourceEventHandler, _ toolscache.HandlerOptions) (toolscache.ResourceEventHandlerRegistration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers = append(s.handlers, handler)
	return stubHandlerRegistration{}, nil
}

func (s *stubInformer) RemoveEventHandler(_ toolscache.ResourceEventHandlerRegistration) error {
	return nil
}

func (s *stubInformer) AddIndexers(toolscache.Indexers) error {
	return nil
}

func (s *stubInformer) HasSynced() bool {
	return true
}

func (s *stubInformer) IsStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *stubInformer) handlerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.handlers)
}

func (s *stubInformer) onAdd(obj client.Object) {
	s.mu.Lock()
	handlers := append([]toolscache.ResourceEventHandler(nil), s.handlers...)
	s.mu.Unlock()
	for _, handler := range handlers {
		handler.OnAdd(obj, false)
	}
}

func (s *stubInformer) onUpdate(oldObj, newObj client.Object) {
	s.mu.Lock()
	handlers := append([]toolscache.ResourceEventHandler(nil), s.handlers...)
	s.mu.Unlock()
	for _, handler := range handlers {
		handler.OnUpdate(oldObj, newObj)
	}
}

func (s *stubInformer) onDelete(obj client.Object) {
	s.mu.Lock()
	handlers := append([]toolscache.ResourceEventHandler(nil), s.handlers...)
	s.mu.Unlock()
	for _, handler := range handlers {
		handler.OnDelete(obj)
	}
}

func (s *stubCache) GetInformer(_ context.Context, obj client.Object, _ ...cache.InformerGetOption) (cache.Informer, error) {
	switch obj.(type) {
	case *v1alpha1.ResourceGraphDefinition:
		return s.rgdInformer, nil
	default:
		return s.crdInformer, nil
	}
}

func (s *stubCache) WaitForCacheSync(context.Context) bool {
	return true
}

func testScheme(t testing.TB) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, extv1.AddToScheme(scheme))

	return scheme
}

func newTestClient(t testing.TB, funcs interceptor.Funcs, objs ...client.Object) client.WithWatch {
	t.Helper()

	builder := clientfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&v1alpha1.ResourceGraphDefinition{}).
		WithInterceptorFuncs(funcs)
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	return builder.Build()
}

func getStoredRGD(t testing.TB, c client.Client, name string) *v1alpha1.ResourceGraphDefinition {
	t.Helper()

	stored := &v1alpha1.ResourceGraphDefinition{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: name}, stored))
	return stored
}

func newProcessedGraph() *graph.Graph {
	nodes := map[string]*graph.Node{
		"vpc": {
			Meta: graph.NodeMeta{ID: "vpc"},
		},
		"subnetA": {
			Meta: graph.NodeMeta{
				ID:           "subnetA",
				Dependencies: []string{"vpc"},
			},
		},
		"subnetB": {
			Meta: graph.NodeMeta{
				ID:           "subnetB",
				Dependencies: []string{"vpc"},
			},
		},
	}

	return &graph.Graph{
		Instance: &graph.Node{
			Meta: graph.NodeMeta{
				GVR: metadata.GetResourceGraphDefinitionInstanceGVR("example.io", "v1alpha1", "Network"),
			},
		},
		Nodes:            nodes,
		Resources:        nodes,
		TopologicalOrder: []string{"vpc", "subnetA", "subnetB"},
		CRD: &extv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "networks.example.io"},
			Spec: extv1.CustomResourceDefinitionSpec{
				Group: "example.io",
				Names: extv1.CustomResourceDefinitionNames{
					Kind:   "Network",
					Plural: "networks",
				},
			},
		},
	}
}

func newTestBuilder() resourceGraphBuilder {
	return &stubGraphBuilder{
		build: func(*v1alpha1.ResourceGraphDefinition, graph.RGDConfig) (*graph.Graph, error) {
			return newProcessedGraph(), nil
		},
	}
}

func newFailingBuilder(err error) resourceGraphBuilder {
	return &stubGraphBuilder{
		build: func(*v1alpha1.ResourceGraphDefinition, graph.RGDConfig) (*graph.Graph, error) {
			return nil, err
		},
	}
}

func newKROFakeSet() *krofake.FakeSet {
	return krofake.NewFakeSet(dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()))
}

func testDynamicControllerConfig() dynamiccontroller.Config {
	return dynamiccontroller.Config{
		Workers:              1,
		ResyncPeriod:         time.Hour,
		QueueMaxRetries:      1,
		MinRetryDelay:        time.Millisecond,
		MaxRetryDelay:        2 * time.Millisecond,
		RateLimit:            100,
		BurstLimit:           100,
		QueueShutdownTimeout: time.Second,
	}
}

func newDynamicController(t testing.TB) *dynamiccontroller.DynamicController {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, metav1.AddMetaToScheme(scheme))

	return dynamiccontroller.NewDynamicController(
		logr.Discard(),
		testDynamicControllerConfig(),
		metadatafake.NewSimpleMetadataClient(scheme),
		apimeta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "example.io", Version: "v1alpha1"}}),
	)
}

func newRunningDynamicController(t testing.TB) *dynamiccontroller.DynamicController {
	t.Helper()

	dc := newDynamicController(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		_ = dc.Start(ctx)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	t.Cleanup(func() {
		cancel()
		<-done
	})

	return dc
}

func newTestRGD(name string) *v1alpha1.ResourceGraphDefinition {
	rgd := generator.NewResourceGraphDefinition(name,
		generator.WithSchema(
			"Network", "v1alpha1",
			map[string]interface{}{
				"name": "string",
			},
			map[string]interface{}{
				"vpcID": "${vpc.status.vpcID}",
			},
		),
		generator.WithResource("vpc", map[string]interface{}{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1",
			"kind":       "VPC",
			"metadata": map[string]interface{}{
				"name": "test-vpc",
			},
			"spec": map[string]interface{}{
				"cidrBlocks":         []interface{}{"10.0.0.0/16"},
				"enableDNSSupport":   true,
				"enableDNSHostnames": true,
			},
		}, []string{"${vpc.status.state == 'available'}"}, nil),
		generator.WithResource("subnetA", map[string]interface{}{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1",
			"kind":       "Subnet",
			"metadata": map[string]interface{}{
				"name": "test-subnet-a",
			},
			"spec": map[string]interface{}{
				"cidrBlock": "10.0.1.0/24",
				"vpcID":     "${vpc.status.vpcID}",
			},
		}, []string{"${subnetA.status.state == 'available'}"}, nil),
		generator.WithResource("subnetB", map[string]interface{}{
			"apiVersion": "ec2.services.k8s.aws/v1alpha1",
			"kind":       "Subnet",
			"metadata": map[string]interface{}{
				"name": "test-subnet-b",
			},
			"spec": map[string]interface{}{
				"cidrBlock": "10.0.2.0/24",
				"vpcID":     "${vpc.status.vpcID}",
			},
		}, []string{"${subnetB.status.state == 'available'}"}, nil),
	)

	rgd.Spec.Schema.Group = "example.io"
	rgd.UID = types.UID(name + "-uid")
	rgd.Generation = 1
	rgd.CreationTimestamp = metav1.NewTime(time.Unix(1700000000, 0))

	return rgd
}

func conditionFor(t testing.TB, rgd *v1alpha1.ResourceGraphDefinition, conditionType string) *v1alpha1.Condition {
	t.Helper()

	cond := rgdConditionTypes.For(rgd).Get(conditionType)
	require.NotNil(t, cond)
	return cond
}

func expectedResourcesInfo() []v1alpha1.ResourceInformation {
	return []v1alpha1.ResourceInformation{
		{
			ID: "subnetA",
			Dependencies: []v1alpha1.Dependency{
				{ID: "vpc"},
			},
		},
		{
			ID: "subnetB",
			Dependencies: []v1alpha1.Dependency{
				{ID: "vpc"},
			},
		},
	}
}

func TestNewResourceGraphDefinitionReconciler(t *testing.T) {
	r := NewResourceGraphDefinitionReconciler(
		newKROFakeSet(),
		true,
		nil,
		nil,
		7,
		graph.RGDConfig{MaxCollectionSize: 32},
	)

	require.NotNil(t, r)
	assert.True(t, r.allowCRDDeletion)
	assert.NotNil(t, r.crdManager)
	assert.Equal(t, 7, r.maxConcurrentReconciles)
	assert.Equal(t, graph.RGDConfig{MaxCollectionSize: 32}, r.rgdConfig)
	assert.Equal(t, metadata.NewKROMetaLabeler().Labels(), r.metadataLabeler.Labels())
}

func TestFindRGDsForCRD(t *testing.T) {
	reconciler := &ResourceGraphDefinitionReconciler{}

	tests := []struct {
		name string
		obj  client.Object
		want []reconcile.Request
	}{
		{
			name: "returns nil when metadata access fails",
			obj:  nil,
		},
		{
			name: "ignores resources not owned by kro",
			obj: &extv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "unowned"},
			},
		},
		{
			name: "ignores owned resources without rgd name label",
			obj: &extv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "owned",
					Labels: map[string]string{
						metadata.OwnedLabel: "true",
					},
				},
			},
		},
		{
			name: "maps owned CRDs back to their rgd",
			obj: &extv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "owned",
					Labels: map[string]string{
						metadata.ManagedByLabelKey:                metadata.ManagedByKROValue,
						metadata.ResourceGraphDefinitionNameLabel: "demo-rgd",
					},
				},
			},
			want: []reconcile.Request{{
				NamespacedName: types.NamespacedName{Name: "demo-rgd"},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, reconciler.findRGDsForCRD(context.Background(), tt.obj))
		})
	}
}

func TestSetupWithManager(t *testing.T) {
	tests := []struct {
		name    string
		addErr  error
		wantErr string
		check   func(*testing.T, *ResourceGraphDefinitionReconciler, *krofake.FakeSet, *stubManager, client.WithWatch, apimeta.RESTMapper, *v1alpha1.ResourceGraphDefinition)
	}{
		{
			name: "registers the controller runnable",
			check: func(t *testing.T, reconciler *ResourceGraphDefinitionReconciler, fakeSet *krofake.FakeSet, mgr *stubManager, fakeClient client.WithWatch, mapper apimeta.RESTMapper, rgd *v1alpha1.ResourceGraphDefinition) {
				assert.Equal(t, fakeClient, reconciler.Client)
				assert.Equal(t, mapper, fakeSet.RESTMapper())
				assert.Equal(t, 1, mgr.addCalls)
				assert.NotNil(t, mgr.lastRunnable)

				startCtx, cancel := context.WithCancel(context.Background())
				startDone := make(chan error, 1)
				go func() {
					startDone <- mgr.lastRunnable.Start(startCtx)
				}()
				t.Cleanup(func() {
					cancel()
					require.NoError(t, <-startDone)
				})

				cache := mgr.cache.(*stubCache)
				require.Eventually(t, func() bool {
					return cache.rgdInformer.handlerCount() > 0 && cache.crdInformer.handlerCount() > 0
				}, time.Second, 10*time.Millisecond)

				oldRGD := rgd.DeepCopy()
				newRGD := rgd.DeepCopy()
				newRGD.Generation++
				cache.rgdInformer.onUpdate(oldRGD, newRGD)

				require.Eventually(t, func() bool {
					stored := getStoredRGD(t, fakeClient, rgd.Name)
					return metadata.HasResourceGraphDefinitionFinalizer(stored) && stored.Status.State == v1alpha1.ResourceGraphDefinitionStateActive
				}, time.Second, 10*time.Millisecond)

				crd := &extv1.CustomResourceDefinition{
					ObjectMeta: metav1.ObjectMeta{
						Name: "networks.example.io",
						Labels: map[string]string{
							metadata.ManagedByLabelKey:                metadata.ManagedByKROValue,
							metadata.ResourceGraphDefinitionNameLabel: rgd.Name,
						},
					},
				}
				cache.crdInformer.onAdd(crd)
				cache.crdInformer.onUpdate(crd.DeepCopy(), crd.DeepCopy())
				cache.crdInformer.onDelete(crd.DeepCopy())
			},
		},
		{
			name:    "returns runnable registration errors",
			addErr:  errors.New("add boom"),
			wantErr: "add boom",
			check: func(t *testing.T, reconciler *ResourceGraphDefinitionReconciler, fakeSet *krofake.FakeSet, mgr *stubManager, fakeClient client.WithWatch, mapper apimeta.RESTMapper, _ *v1alpha1.ResourceGraphDefinition) {
				assert.Equal(t, fakeClient, reconciler.Client)
				assert.Equal(t, mapper, fakeSet.RESTMapper())
				assert.Equal(t, 1, mgr.addCalls)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := newTestRGD("setup-manager")
			fakeClient := newTestClient(t, interceptor.Funcs{}, rgd.DeepCopy())
			fakeSet := newKROFakeSet()
			skipNameValidation := true
			mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{
				v1alpha1.GroupVersion,
				extv1.SchemeGroupVersion,
			})
			cache := &stubCache{
				rgdInformer: &stubInformer{},
				crdInformer: &stubInformer{},
			}
			mgr := &stubManager{
				client:            fakeClient,
				restMapper:        mapper,
				logger:            logr.Discard(),
				scheme:            testScheme(t),
				cache:             cache,
				controllerOptions: config.Controller{SkipNameValidation: &skipNameValidation},
				addErr:            tt.addErr,
			}

			reconciler := NewResourceGraphDefinitionReconciler(fakeSet, true, newRunningDynamicController(t), nil, 3, graph.RGDConfig{})
			reconciler.rgBuilder = newTestBuilder()
			reconciler.crdManager = &stubCRDManager{}
			err := reconciler.SetupWithManager(mgr)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}

			tt.check(t, reconciler, fakeSet, mgr, fakeClient, mapper, rgd)
		})
	}
}

func TestReconcile(t *testing.T) {
	tests := []struct {
		name  string
		build func(*testing.T) (*ResourceGraphDefinitionReconciler, client.WithWatch, *v1alpha1.ResourceGraphDefinition, *stubCRDManager)
		check func(*testing.T, ctrl.Result, error, client.WithWatch, *v1alpha1.ResourceGraphDefinition, *stubCRDManager)
	}{
		{
			name: "returns set managed errors",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, client.WithWatch, *v1alpha1.ResourceGraphDefinition, *stubCRDManager) {
				rgd := newTestRGD("reconcile-managed-error")
				c := newTestClient(t, interceptor.Funcs{
					Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
						return errors.New("patch boom")
					},
				}, rgd.DeepCopy())

				return &ResourceGraphDefinitionReconciler{Client: c}, c, rgd, nil
			},
			check: func(t *testing.T, result ctrl.Result, err error, _ client.WithWatch, _ *v1alpha1.ResourceGraphDefinition, _ *stubCRDManager) {
				assert.Equal(t, ctrl.Result{}, result)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "patch boom")
			},
		},
		{
			name: "reconciles active rgds successfully",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, client.WithWatch, *v1alpha1.ResourceGraphDefinition, *stubCRDManager) {
				rgd := newTestRGD("reconcile-active")
				c := newTestClient(t, interceptor.Funcs{}, rgd.DeepCopy())
				manager := &stubCRDManager{}

				return &ResourceGraphDefinitionReconciler{
					Client:            c,
					metadataLabeler:   metadata.NewKROMetaLabeler(),
					rgBuilder:         newTestBuilder(),
					dynamicController: newRunningDynamicController(t),
					crdManager:        manager,
					clientSet:         newKROFakeSet(),
					instanceLogger:    logr.Discard(),
				}, c, rgd, manager
			},
			check: func(t *testing.T, result ctrl.Result, err error, c client.WithWatch, rgd *v1alpha1.ResourceGraphDefinition, _ *stubCRDManager) {
				require.NoError(t, err)
				assert.Equal(t, ctrl.Result{}, result)

				stored := getStoredRGD(t, c, rgd.Name)
				assert.True(t, metadata.HasResourceGraphDefinitionFinalizer(stored))
				assert.Equal(t, v1alpha1.ResourceGraphDefinitionStateActive, stored.Status.State)
				assert.Equal(t, []string{"vpc", "subnetA", "subnetB"}, stored.Status.TopologicalOrder)
				assert.Equal(t, expectedResourcesInfo(), stored.Status.Resources)
			},
		},
		{
			name: "joins reconcile and status errors",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, client.WithWatch, *v1alpha1.ResourceGraphDefinition, *stubCRDManager) {
				rgd := newTestRGD("reconcile-status-join")
				c := newTestClient(t, interceptor.Funcs{
					SubResourcePatch: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
						return errors.New("status boom")
					},
				}, rgd.DeepCopy())

				return &ResourceGraphDefinitionReconciler{
					Client:          c,
					metadataLabeler: metadata.NewKROMetaLabeler(),
					rgBuilder:       newFailingBuilder(errors.New("naming convention violation")),
					clientSet:       newKROFakeSet(),
				}, c, rgd, nil
			},
			check: func(t *testing.T, result ctrl.Result, err error, _ client.WithWatch, _ *v1alpha1.ResourceGraphDefinition, _ *stubCRDManager) {
				assert.Equal(t, ctrl.Result{}, result)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "status boom")
				assert.Contains(t, err.Error(), "naming convention violation")
			},
		},
		{
			name: "removes the finalizer on delete after successful cleanup",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, client.WithWatch, *v1alpha1.ResourceGraphDefinition, *stubCRDManager) {
				rgd := newTestRGD("reconcile-delete")
				metadata.SetResourceGraphDefinitionFinalizer(rgd)
				now := metav1.Now()
				rgd.DeletionTimestamp = &now

				c := newTestClient(t, interceptor.Funcs{}, rgd.DeepCopy())
				dc := newRunningDynamicController(t)
				gvr := metadata.GetResourceGraphDefinitionInstanceGVR(rgd.Spec.Schema.Group, rgd.Spec.Schema.APIVersion, rgd.Spec.Schema.Kind)
				require.NoError(t, dc.Register(context.Background(), gvr, func(context.Context, ctrl.Request) error { return nil }))

				manager := &stubCRDManager{}
				return &ResourceGraphDefinitionReconciler{
					Client:            c,
					allowCRDDeletion:  true,
					dynamicController: dc,
					crdManager:        manager,
				}, c, rgd, manager
			},
			check: func(t *testing.T, result ctrl.Result, err error, c client.WithWatch, rgd *v1alpha1.ResourceGraphDefinition, manager *stubCRDManager) {
				require.NoError(t, err)
				assert.Equal(t, ctrl.Result{}, result)

				stored := &v1alpha1.ResourceGraphDefinition{}
				err = c.Get(context.Background(), client.ObjectKey{Name: rgd.Name}, stored)
				require.Error(t, err)
				assert.True(t, apierrors.IsNotFound(err))
				assert.Equal(t, []string{extractCRDName(rgd.Spec.Schema.Group, rgd.Spec.Schema.Kind)}, manager.deleted)
			},
		},
		{
			name: "preserves the finalizer when cleanup fails",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, client.WithWatch, *v1alpha1.ResourceGraphDefinition, *stubCRDManager) {
				rgd := newTestRGD("reconcile-delete-error")
				metadata.SetResourceGraphDefinitionFinalizer(rgd)
				now := metav1.Now()
				rgd.DeletionTimestamp = &now

				c := newTestClient(t, interceptor.Funcs{}, rgd.DeepCopy())
				manager := &stubCRDManager{deleteErr: errors.New("delete boom")}
				return &ResourceGraphDefinitionReconciler{
					Client:            c,
					allowCRDDeletion:  true,
					dynamicController: newRunningDynamicController(t),
					crdManager:        manager,
				}, c, rgd, manager
			},
			check: func(t *testing.T, result ctrl.Result, err error, c client.WithWatch, rgd *v1alpha1.ResourceGraphDefinition, _ *stubCRDManager) {
				assert.Equal(t, ctrl.Result{}, result)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "failed to cleanup CRD")
				assert.True(t, metadata.HasResourceGraphDefinitionFinalizer(getStoredRGD(t, c, rgd.Name)))
			},
		},
		{
			name: "returns finalizer removal errors after successful cleanup",
			build: func(t *testing.T) (*ResourceGraphDefinitionReconciler, client.WithWatch, *v1alpha1.ResourceGraphDefinition, *stubCRDManager) {
				rgd := newTestRGD("reconcile-delete-unmanaged-error")
				metadata.SetResourceGraphDefinitionFinalizer(rgd)
				now := metav1.Now()
				rgd.DeletionTimestamp = &now

				c := newTestClient(t, interceptor.Funcs{
					Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
						return errors.New("patch boom")
					},
				}, rgd.DeepCopy())

				dc := newRunningDynamicController(t)
				gvr := metadata.GetResourceGraphDefinitionInstanceGVR(rgd.Spec.Schema.Group, rgd.Spec.Schema.APIVersion, rgd.Spec.Schema.Kind)
				require.NoError(t, dc.Register(context.Background(), gvr, func(context.Context, ctrl.Request) error { return nil }))

				return &ResourceGraphDefinitionReconciler{
					Client:            c,
					dynamicController: dc,
					crdManager:        &stubCRDManager{},
				}, c, rgd, nil
			},
			check: func(t *testing.T, result ctrl.Result, err error, c client.WithWatch, rgd *v1alpha1.ResourceGraphDefinition, _ *stubCRDManager) {
				assert.Equal(t, ctrl.Result{}, result)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "patch boom")
				assert.True(t, metadata.HasResourceGraphDefinitionFinalizer(getStoredRGD(t, c, rgd.Name)))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reconciler, c, rgd, manager := tt.build(t)
			result, err := reconciler.Reconcile(context.Background(), rgd)
			tt.check(t, result, err, c, rgd, manager)
		})
	}
}
