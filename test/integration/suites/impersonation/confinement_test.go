// Copyright 2025 The Kube Resource Orchestrator Authors
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

// Package impersonation stands up a dedicated, self-contained envtest control
// plane with authorization-mode=RBAC (the controller-runtime default) to PROVE
// Graph impersonation confines resource writes: a Graph naming a ServiceAccount
// whose RBAC forbids the write must fail to apply, and the resource must never
// be created. This is deliberately NOT run against the shared core suite's
// apiserver (which grants every impersonated SA cluster-admin so existing
// suites stay permissive); confinement can only be observed where RBAC actually
// denies the impersonated identity.
package impersonation

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	ctrlgraph "github.com/kubernetes-sigs/kro/pkg/controller/graph"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
)

// TestGraphImpersonationConfinement proves an impersonated ServiceAccount whose
// RBAC forbids a write cannot apply the Graph's resource. It stands up its OWN
// envtest (RBAC-enforcing), grants a limited SA read-only access to ConfigMaps,
// then submits a Graph (as that SA) that tries to CREATE a ConfigMap. The apply
// must be refused and the ConfigMap must never appear.
func TestGraphImpersonationConfinement(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping envtest-backed confinement test in -short mode")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Own control plane: default authorization-mode is RBAC in controller-runtime
	// envtest, which is exactly what makes confinement observable.
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "..", "helm", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := expv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kro scheme: %v", err)
	}

	adminClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("admin client: %v", err)
	}

	const ns = "confined"
	const saName = "reader"

	create(t, ctx, adminClient, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	create(t, ctx, adminClient, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: saName}})

	// The SA may only GET configmaps — never CREATE them. So the Graph's apply
	// (a server-side apply == create) is forbidden for this identity.
	create(t, ctx, adminClient, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cm-reader"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"configmaps"},
			Verbs:     []string{"get", "list", "watch"},
		}},
	})
	create(t, ctx, adminClient, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cm-reader"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "cm-reader"},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Namespace: ns,
			Name:      saName,
		}},
	})

	// Minimal manager + Graph reconciler wired exactly like production
	// (cmd/controller/graphengine.go): impersonated client + SSAR authz gate,
	// RequireImpersonation on.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 server.Options{BindAddress: "0"},
		Controller:              ctrlconfig.Controller{SkipNameValidation: ptr(true)},
		GracefulShutdownTimeout: ptr(5 * time.Second),
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatalf("http client: %v", err)
	}
	cmp, err := compiler.NewCompiler(cfg, httpClient)
	if err != nil {
		t.Fatalf("new compiler: %v", err)
	}

	exec := executor.NewSimple(mgr.GetClient())
	exec.ConflictDetection = true

	baseCfg := mgr.GetConfig()
	mapper := mgr.GetRESTMapper()
	impersonation := ctrlgraph.NewImpersonation(exec, func(user string) (client.Client, error) {
		ic := rest.CopyConfig(baseCfg)
		ic.Impersonate = rest.ImpersonationConfig{UserName: user}
		return client.New(ic, client.Options{Mapper: mapper})
	}, func(user string) (authorizationv1client.AuthorizationV1Interface, error) {
		ic := rest.CopyConfig(baseCfg)
		ic.Impersonate = rest.ImpersonationConfig{UserName: user}
		cs, err := kubernetes.NewForConfig(ic)
		if err != nil {
			return nil, err
		}
		return cs.AuthorizationV1(), nil
	})

	reconciler := &ctrlgraph.Reconciler{
		Client:               mgr.GetClient(),
		Compiler:             cmp,
		Registry:             registry.New(),
		Executor:             exec,
		Impersonation:        impersonation,
		RequireImpersonation: true,
		MaxCollectionSize:    1000,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup reconciler: %v", err)
	}

	mgrCtx, mgrCancel := context.WithCancel(ctx)
	defer mgrCancel()
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.Start(mgrCtx) }()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}

	// A Graph naming the read-only SA that tries to CREATE a ConfigMap.
	g := &expv1alpha1.Graph{
		ObjectMeta: metav1.ObjectMeta{Name: "confined", Namespace: ns},
		Spec: expv1alpha1.GraphSpec{
			ServiceAccountName: saName,
			Nodes: []expv1alpha1.Node{{
				ID: "cm",
				Template: rawExt(t, map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "forbidden-cm"},
					"data":       map[string]any{"hello": "world"},
				}),
			}},
		},
	}
	if err := adminClient.Create(ctx, g); err != nil {
		t.Fatalf("create graph: %v", err)
	}

	// The Graph must report ResourcesConverged=False (apply refused). Poll with a
	// bounded deadline; no unbounded sleep.
	key := types.NamespacedName{Namespace: ns, Name: "confined"}
	deadline := time.Now().Add(30 * time.Second)
	var lastMsg string
	converged := false
	for time.Now().Before(deadline) {
		got := &expv1alpha1.Graph{}
		if err := adminClient.Get(ctx, key, got); err == nil {
			for _, c := range got.Status.Conditions {
				if c.Type != ctrlgraph.ResourcesConverged {
					continue
				}
				if c.Status == metav1.ConditionFalse {
					converged = true
				}
				if c.Message != nil {
					lastMsg = *c.Message
				}
			}
			if converged {
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}

	if !converged {
		t.Fatalf("expected Graph ResourcesConverged=False (apply refused by RBAC); last message=%q", lastMsg)
	}
	if !strings.Contains(lastMsg, "forbidden") && !strings.Contains(lastMsg, "cannot") {
		t.Errorf("expected a forbidden/denied apply message, got %q", lastMsg)
	}

	// The confined write must NOT have happened: the ConfigMap must not exist.
	cm := &corev1.ConfigMap{}
	err = adminClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "forbidden-cm"}, cm)
	if err == nil {
		t.Fatalf("confinement breach: ConfigMap %q/forbidden-cm was created despite RBAC denial", ns)
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected error checking ConfigMap absence: %v", err)
	}

	mgrCancel()
	select {
	case <-errCh:
	case <-time.After(10 * time.Second):
		t.Log("manager did not stop within 10s")
	}
}

func ptr[T any](v T) *T { return &v }

func create(t *testing.T, ctx context.Context, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create %T: %v", obj, err)
	}
}

func rawExt(t *testing.T, v map[string]any) *runtime.RawExtension {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal template: %v", err)
	}
	return &runtime.RawExtension{Raw: b}
}
