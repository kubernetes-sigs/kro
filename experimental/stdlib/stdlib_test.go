package stdlib_test

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Standard library integration tests
//
// These tests verify the full type tower materializes through the Graph
// controller's reconciliation loop. The binary boots with --bootstrap,
// which installs Graph + GraphRevision CRDs and then applies stdlib
// resources with retry. The tests wait for CRDs to appear — proving the
// tower's dependency chain resolves naturally.
//
// Test progression:
//   1. Kind CRD appears (kind.yaml creates it)
//   2. Decorator CRD appears (Kind controller processes decorator.yaml)
//   3. Singleton CRD appears (Kind controller processes singleton.yaml)
// ═══════════════════════════════════════════════════════════════════════════════

var (
	testEnv   *envtest.Environment
	k8sClient client.Client
	dynClient dynamic.Interface
	ctx       context.Context
	cancel    context.CancelFunc
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(true)))
	flag.Parse()
	ctx, cancel = context.WithCancel(context.Background())

	// Build the binary.
	binaryPath, err := buildBinary()
	if err != nil {
		panic("building binary: " + err.Error())
	}

	// Shared log file.
	logPath := filepath.Join(filepath.Dir(binaryPath), "stdlib-test.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		panic("creating log file: " + err.Error())
	}
	defer logFile.Close()
	fmt.Fprintf(os.Stderr, "stdlib test logs: %s\n", logPath)

	// Start envtest.
	testEnv = &envtest.Environment{
		BinaryAssetsDirectory: resolveEnvtestAssets(),
		ControlPlane: envtest.ControlPlane{
			APIServer: &envtest.APIServer{Out: logFile, Err: logFile},
			Etcd:      &envtest.Etcd{Out: logFile, Err: logFile},
		},
	}
	cfg, err := testEnv.Start()
	if err != nil {
		panic("starting envtest: " + err.Error())
	}
	defer func() {
		cancel()
		testEnv.Stop() //nolint:errcheck
	}()

	// Signal handler.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
		testEnv.Stop() //nolint:errcheck
		os.Exit(1)
	}()

	// Clients.
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		panic("creating client: " + err.Error())
	}
	dynClient, err = dynamic.NewForConfig(cfg)
	if err != nil {
		panic("creating dynamic client: " + err.Error())
	}

	// Write kubeconfig, start binary.
	kubeconfigPath, err := writeKubeconfig(cfg)
	if err != nil {
		panic("writing kubeconfig: " + err.Error())
	}
	defer os.Remove(kubeconfigPath)

	healthAddr, cmd, err := startBinary(binaryPath, kubeconfigPath, logFile)
	if err != nil {
		panic("starting binary: " + err.Error())
	}
	binaryDied := make(chan error, 1)
	go func() { binaryDied <- cmd.Wait() }()

	if err := waitForHealthy(ctx, healthAddr, binaryDied); err != nil {
		cmd.Process.Signal(syscall.SIGKILL) //nolint:errcheck
		panic("waiting for binary readiness: " + err.Error())
	}

	code := m.Run()

	stopBinary(cmd, binaryDied)
	cancel()
	testEnv.Stop() //nolint:errcheck
	os.Exit(code)
}

// ═══════════════════════════════════════════════════════════════════════════════
// Stdlib materialization tests
// ═══════════════════════════════════════════════════════════════════════════════

func TestTowerKindCRD(t *testing.T) {
	// The Kind CRD is created by kind-controller.yaml (a Graph).
	// This is the first CRD the stdlib creates — everything else depends on it.
	waitForCRD(t, "kinds.experimental.kro.run", 60*time.Second)
}

func TestTowerDecoratorCRD(t *testing.T) {
	// The Decorator CRD is created by the Kind controller
	// processing decorator.yaml (a Kind for Decorator).
	waitForCRD(t, "decorators.experimental.kro.run", 60*time.Second)
}

func TestTowerSingletonCRD(t *testing.T) {
	// The Singleton CRD is created by the Kind controller
	// processing singleton-definition.yaml.
	waitForCRD(t, "singletons.experimental.kro.run", 60*time.Second)
}

func TestTowerDecoratorInstanceCreatable(t *testing.T) {
	waitForCRD(t, "decorators.experimental.kro.run", 60*time.Second)

	decorator := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Decorator",
		"metadata": map[string]any{
			"name":      "test-decorator",
			"namespace": "default",
		},
		"spec": map[string]any{
			"watch": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"selector":   map[string]any{},
			},
			"nodes": []any{},
		},
	}}
	gvr := schema.GroupVersionResource{
		Group:    "experimental.kro.run",
		Version:  "v1alpha1",
		Resource: "decorators",
	}
	_, err := dynClient.Resource(gvr).Namespace("default").Create(ctx, decorator, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating Decorator instance: %v", err)
	}
	t.Cleanup(func() {
		dynClient.Resource(gvr).Namespace("default").Delete(ctx, "test-decorator", metav1.DeleteOptions{}) //nolint:errcheck
	})
	t.Logf("Decorator instance created successfully — CRD schema is valid")
}

func TestTowerSingletonInstanceCreatable(t *testing.T) {
	waitForCRD(t, "singletons.experimental.kro.run", 60*time.Second)

	singleton := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Singleton",
		"metadata": map[string]any{
			"name":      "test-singleton",
			"namespace": "default",
		},
		"spec": map[string]any{
			"priority": int64(100),
			"template": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "test-config", "namespace": "default"},
			},
		},
	}}
	gvr := schema.GroupVersionResource{
		Group:    "experimental.kro.run",
		Version:  "v1alpha1",
		Resource: "singletons",
	}
	_, err := dynClient.Resource(gvr).Namespace("default").Create(ctx, singleton, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating Singleton instance: %v", err)
	}
	t.Cleanup(func() {
		dynClient.Resource(gvr).Namespace("default").Delete(ctx, "test-singleton", metav1.DeleteOptions{}) //nolint:errcheck
	})
	t.Logf("Singleton instance created successfully — CRD schema is valid")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Cross-primitive: user Kind → CRD → instance → resources
//
// This test exercises the full pipeline: create a Kind that defines
// a new type (TestWidget), wait for the CRD to appear, create a
// TestWidget instance, and verify the per-instance ConfigMap appears.
// ═══════════════════════════════════════════════════════════════════════════════

func TestTowerUserKind(t *testing.T) {
	// Wait for ALL stdlib CRDs to be fully materialized first.
	// This ensures the Kind controller is stable before we add a user Kind.
	waitForCRD(t, "kinds.experimental.kro.run", 60*time.Second)
	waitForCRD(t, "decorators.experimental.kro.run", 60*time.Second)
	waitForCRD(t, "singletons.experimental.kro.run", 60*time.Second)

	// Create a Kind that defines kind: TestWidget.
	// Both the Kind and the TestWidget instance are created in kro-system
	// because WatchKind is namespace-scoped — the L1 controller Graph lives
	// in kro-system and only watches instances in that namespace.
	// See design doc § Constraints: Namespace scoping.
	k := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "experimental.kro.run/v1alpha1",
		"kind":       "Kind",
		"metadata": map[string]any{
			"name":      "testwidget",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"kind":    "TestWidget",
			"group":   "test.stdlib.kro.run",
			"version": "v1alpha1",
			"schema": map[string]any{
				"spec": map[string]any{
					"message": "string | default=hello",
				},
			},
			"nodes": []any{
				map[string]any{
					"id": "cm",
					"template": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"name":      "${schema.metadata.name}-config",
							"namespace": "${schema.metadata.namespace}",
						},
						"data": map[string]any{
							"message": "${schema.spec.message}",
						},
					},
				},
			},
		},
	}}

	kindGVR := schema.GroupVersionResource{
		Group: "experimental.kro.run", Version: "v1alpha1", Resource: "kinds",
	}
	_, err := dynClient.Resource(kindGVR).Namespace("kro-system").Create(ctx, k, metav1.CreateOptions{})
	require.NoError(t, err, "creating TestWidget Kind")
	t.Cleanup(func() {
		dynClient.Resource(kindGVR).Namespace("kro-system").Delete(ctx, "testwidget", metav1.DeleteOptions{}) //nolint:errcheck
	})

	// Wait for the TestWidget CRD to appear.
	t.Log("waiting for TestWidget CRD...")
	waitForCRD(t, "testwidgets.test.stdlib.kro.run", 60*time.Second)
	t.Log("TestWidget CRD established")

	// Create a TestWidget instance.
	widget := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "test.stdlib.kro.run/v1alpha1",
		"kind":       "TestWidget",
		"metadata": map[string]any{
			"name":      "my-widget",
			"namespace": "kro-system",
		},
		"spec": map[string]any{
			"message": "hello from stdlib test",
		},
	}}

	widgetGVR := schema.GroupVersionResource{
		Group: "test.stdlib.kro.run", Version: "v1alpha1", Resource: "testwidgets",
	}
	_, err = dynClient.Resource(widgetGVR).Namespace("kro-system").Create(ctx, widget, metav1.CreateOptions{})
	require.NoError(t, err, "creating TestWidget instance")
	t.Cleanup(func() {
		dynClient.Resource(widgetGVR).Namespace("kro-system").Delete(ctx, "my-widget", metav1.DeleteOptions{}) //nolint:errcheck
	})

	// Wait for the per-instance ConfigMap to appear.
	// The new architecture adds more Graph nesting (Kind → Decorator →
	// sub-Graph) so allow extra time for the full chain to reconcile.
	t.Log("waiting for per-instance ConfigMap...")
	cmGVR := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		cm, err := dynClient.Resource(cmGVR).Namespace("kro-system").Get(ctx, "my-widget-config", metav1.GetOptions{})
		if err == nil {
			data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
			assert.Equal(t, "hello from stdlib test", data["message"],
				"ConfigMap should have the widget's message")
			t.Log("ConfigMap created with correct data — full pipeline works")
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("ConfigMap my-widget-config not created within 120s")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Helpers
// ═══════════════════════════════════════════════════════════════════════════════

func waitForCRD(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, crd)
		if err == nil {
			for _, cond := range crd.Status.Conditions {
				if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
					t.Logf("CRD %s established", name)
					return
				}
			}
		}
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatalf("unexpected error checking CRD %s: %v", name, err)
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("CRD %s not established within %s", name, timeout)
}

func buildBinary() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	moduleRoot := wd
	for {
		if _, err := os.Stat(filepath.Join(moduleRoot, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(moduleRoot)
		if parent == moduleRoot {
			return "", fmt.Errorf("cannot find go.mod from %s", wd)
		}
		moduleRoot = parent
	}
	buildDir := filepath.Join(moduleRoot, "build")
	os.MkdirAll(buildDir, 0o755) //nolint:errcheck
	binaryPath := filepath.Join(buildDir, "kro-graph-controller")
	cmd := exec.Command("go", "build", "-o", binaryPath, "./experimental/cmd/")
	cmd.Dir = moduleRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build: %w", err)
	}
	return binaryPath, nil
}

func resolveEnvtestAssets() string {
	if v := os.Getenv("KUBEBUILDER_ASSETS"); v != "" {
		return v
	}
	cmd := exec.Command("setup-envtest", "use", "1.35.x", "-p", "path")
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		fmt.Fprintf(os.Stderr, "setup-envtest failed: %v\n%s\n", err, stderr)
		os.Exit(1)
	}
	return strings.TrimSpace(string(out))
}

func writeKubeconfig(cfg *rest.Config) (string, error) {
	kubeconfig := clientcmdapi.NewConfig()
	kubeconfig.Clusters["envtest"] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
	}
	kubeconfig.AuthInfos["envtest"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
	}
	kubeconfig.Contexts["envtest"] = &clientcmdapi.Context{
		Cluster:  "envtest",
		AuthInfo: "envtest",
	}
	kubeconfig.CurrentContext = "envtest"
	f, err := os.CreateTemp("", "kro-stdlib-kubeconfig-*.yaml")
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := clientcmd.Write(*kubeconfig)
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func startBinary(binaryPath, kubeconfigPath string, logFile *os.File) (string, *exec.Cmd, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("picking health port: %w", err)
	}
	healthAddr := ln.Addr().String()
	ln.Close()

	cmd := exec.Command(binaryPath,
		"--bootstrap",
		"--health-probe-bind-address="+healthAddr,
		"--metrics-bind-address=0",
		"--drift-interval=5s",
	)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("starting binary: %w", err)
	}
	return healthAddr, cmd, nil
}

func waitForHealthy(ctx context.Context, healthAddr string, binaryDied <-chan error) error {
	healthURL := "http://" + healthAddr + "/healthz"
	httpClient := &http.Client{Timeout: 500 * time.Millisecond}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(30 * time.Second)
	for {
		select {
		case err := <-binaryDied:
			return fmt.Errorf("binary exited unexpectedly: %v", err)
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("binary not healthy within 30s")
		case <-ticker.C:
			resp, err := httpClient.Get(healthURL)
			if err != nil {
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
}

func stopBinary(cmd *exec.Cmd, binaryDied <-chan error) {
	select {
	case <-binaryDied:
		return
	default:
	}
	cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck
	select {
	case <-binaryDied:
	case <-time.After(10 * time.Second):
		cmd.Process.Signal(syscall.SIGKILL) //nolint:errcheck
		<-binaryDied
	}
}
