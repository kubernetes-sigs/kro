// Package resync_test runs e2e tests that exercise the per-node resync timer.
// These tests require a short --node-resync-interval (5s) so the timer fires
// during the test. The main e2e suite runs with the production default (30m)
// to ensure propagation correctness without timer assistance.
package resync_test

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

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/ellistarn/kro/experimental/stdlib"
)

var (
	testEnv   *envtest.Environment
	k8sClient client.Client
	ctx       context.Context
	cancel    context.CancelFunc
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(true)))
	flag.Parse()
	ctx, cancel = context.WithCancel(context.Background())

	binaryPath, err := buildBinary()
	if err != nil {
		panic("building binary: " + err.Error())
	}

	logPath := filepath.Join(filepath.Dir(binaryPath), "controller-resync.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		panic("creating log file: " + err.Error())
	}
	defer logFile.Close()
	fmt.Fprintf(os.Stderr, "resync test controller logs: %s\n", logPath)

	chartCRDDir := filepath.Join(filepath.Dir(filepath.Dir(binaryPath)), "crds")
	testEnv = &envtest.Environment{
		BinaryAssetsDirectory: resolveEnvtestAssets(),
		CRDDirectoryPaths:     []string{chartCRDDir},
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\nreceived %s, stopping envtest\n", sig)
		cancel()
		testEnv.Stop() //nolint:errcheck
		os.Exit(1)
	}()

	if f := flag.Lookup("test.timeout"); f != nil {
		if d, err := time.ParseDuration(f.Value.String()); err == nil && d > 5*time.Second {
			time.AfterFunc(d-5*time.Second, func() {
				fmt.Fprintln(os.Stderr, "test timeout approaching, stopping envtest")
				cancel()
				testEnv.Stop() //nolint:errcheck
				os.Exit(1)
			})
		}
	}

	kubeconfigPath, err := writeKubeconfig(cfg)
	if err != nil {
		panic("writing kubeconfig: " + err.Error())
	}
	defer os.Remove(kubeconfigPath)

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		panic("creating client: " + err.Error())
	}

	// No custom CRDs needed — crds/ provides Graph, GraphRevision,
	// and Kind CRDs, and these tests only use ConfigMaps as managed resources.

	healthAddr, cmd, err := startBinary(binaryPath, kubeconfigPath, logFile)
	if err != nil {
		panic("starting binary: " + err.Error())
	}

	binaryDied := make(chan error, 1)
	go func() {
		binaryDied <- cmd.Wait()
	}()

	if err := waitForHealthy(ctx, healthAddr, binaryDied); err != nil {
		cmd.Process.Signal(syscall.SIGKILL) //nolint:errcheck
		panic("waiting for binary readiness: " + err.Error())
	}

	// Apply stdlib — the controller is pure substrate, tests install
	// what they need.
	if err := stdlib.Apply(ctx, logf.Log.WithName("stdlib"), cfg); err != nil {
		cmd.Process.Signal(syscall.SIGKILL) //nolint:errcheck
		panic("applying stdlib: " + err.Error())
	}

	code := m.Run()

	stopBinary(cmd, binaryDied)
	cancel()
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
	}
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// Infrastructure (adapted from test/e2e/main_test.go)
// ---------------------------------------------------------------------------

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
		fmt.Fprintf(os.Stderr, "setup-envtest not found or failed: %v\n%s\nRun: go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest\n", err, stderr)
		os.Exit(1)
	}
	return strings.TrimSpace(string(out))
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
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return "", fmt.Errorf("creating build dir: %w", err)
	}

	binaryPath := filepath.Join(buildDir, "kro-graph-controller")
	cmd := exec.Command("go", "build", "-o", binaryPath, "./cmd/")
	cmd.Dir = moduleRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build: %w", err)
	}
	return binaryPath, nil
}

func writeKubeconfig(cfg *rest.Config) (string, error) {
	kubeconfig := clientcmdapi.NewConfig()
	kubeconfig.Clusters["envtest"] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
		TLSServerName:            cfg.ServerName,
	}
	kubeconfig.AuthInfos["envtest"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
		Token:                 cfg.BearerToken,
	}
	kubeconfig.Contexts["envtest"] = &clientcmdapi.Context{
		Cluster:  "envtest",
		AuthInfo: "envtest",
	}
	kubeconfig.CurrentContext = "envtest"

	f, err := os.CreateTemp("", "kro-resync-test-kubeconfig-*.yaml")
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

func startBinary(binaryPath, kubeconfigPath string, logFile *os.File) (healthAddr string, cmd *exec.Cmd, err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("picking health port: %w", err)
	}
	healthAddr = ln.Addr().String()
	ln.Close()

	cmd = exec.Command(binaryPath,
		"--health-probe-bind-address="+healthAddr,
		"--metrics-bind-address=0",
		"--pprof-bind-address=0",
		"--max-workers=32",
		"--node-resync-interval=5s",
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
			return fmt.Errorf("binary exited unexpectedly during startup: %v", err)
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("binary did not become healthy within 30s at %s", healthURL)
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
		fmt.Fprintln(os.Stderr, "binary did not exit within 10s after SIGTERM, sending SIGKILL")
		cmd.Process.Signal(syscall.SIGKILL) //nolint:errcheck
		<-binaryDied
	}
}
