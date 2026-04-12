package graphcontroller_test

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
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
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

	// -----------------------------------------------------------------------
	// 1. Build the kro binary once. Go's build cache makes this near-instant
	//    after the first build.
	// -----------------------------------------------------------------------
	binaryPath, err := buildBinary()
	if err != nil {
		panic("building binary: " + err.Error())
	}

	// -----------------------------------------------------------------------
	// 2. Create a shared log file for all subprocess output (envtest's
	//    kube-apiserver + etcd, and the controller binary). Having everything
	//    in one file, correlated by timestamp, makes debugging failures
	//    straightforward. Each test creates a unique namespace
	//    (graph-test-xxxxx), so: grep graph-test-xxxxx build/controller.log
	// -----------------------------------------------------------------------
	logPath := filepath.Join(filepath.Dir(binaryPath), "controller.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		panic("creating log file: " + err.Error())
	}
	defer logFile.Close()
	fmt.Fprintf(os.Stderr, "controller logs: %s\n", logPath)

	// -----------------------------------------------------------------------
	// 3. Start envtest (kube-apiserver + etcd), logging to the shared file.
	// -----------------------------------------------------------------------
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

	// envtest starts etcd and kube-apiserver as child processes. On macOS,
	// if the parent exits without calling testEnv.Stop(), these children
	// get reparented to PID 1 and run forever — there is no equivalent of
	// Linux's PR_SET_PDEATHSIG. The three mechanisms below ensure Stop is
	// called regardless of how the process exits:
	//
	//   1. defer     — panics during setup (the lines below can panic)
	//   2. signal    — SIGINT/SIGTERM (Ctrl+C)
	//   3. timeout   — fires just before Go's hard-kill test timeout
	//
	// On the normal path, Stop is called explicitly before os.Exit.
	// Duplicate Stop calls are safe (envtest checks if already stopped).

	// 1. Defer catches panics from the setup code below.
	defer func() {
		cancel()
		testEnv.Stop() //nolint:errcheck
	}()

	// 2. Signal handler catches Ctrl+C before Go's default handler
	//    (which calls os.Exit, skipping defers).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\nreceived %s, stopping envtest\n", sig)
		cancel()
		testEnv.Stop() //nolint:errcheck
		os.Exit(1)
	}()

	// 3. Go's test timeout (default 10m, -timeout flag) fires a panic from
	//    a background goroutine, which doesn't run TestMain's defers. We
	//    race our own timer to fire 5s earlier, giving us time to call Stop
	//    from a goroutine we control.
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

	// -----------------------------------------------------------------------
	// 3. Write kubeconfig for the binary.
	// -----------------------------------------------------------------------
	kubeconfigPath, err := writeKubeconfig(cfg)
	if err != nil {
		panic("writing kubeconfig: " + err.Error())
	}
	defer os.Remove(kubeconfigPath)

	// -----------------------------------------------------------------------
	// 4. Install test-specific CRDs (RGD, SimpleApp). The binary's
	//    --bootstrap installs Graph and GraphRevision from embedded manifests.
	// -----------------------------------------------------------------------
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		panic("creating client: " + err.Error())
	}

	testCRDs := []struct {
		name    string
		builder func() *apiextensionsv1.CustomResourceDefinition
	}{
		{"resourcegraphdefinitions.test.kro.run", buildRGDCRD},
		{"simpleapps.test.kro.run", buildSimpleAppCRD},
	}
	for _, crd := range testCRDs {
		if err := k8sClient.Create(ctx, crd.builder()); err != nil {
			panic("creating CRD " + crd.name + ": " + err.Error())
		}
	}
	for _, crd := range testCRDs {
		if err := waitForCRD(ctx, k8sClient, crd.name); err != nil {
			panic("waiting for CRD " + crd.name + ": " + err.Error())
		}
	}

	// -----------------------------------------------------------------------
	// 5. Start the kro binary as a subprocess. --bootstrap installs Graph
	//    and GraphRevision CRDs and waits for them to become established.
	//    Logs go to the same shared file as envtest.
	// -----------------------------------------------------------------------
	healthAddr, cmd, err := startBinary(binaryPath, kubeconfigPath, logFile)
	if err != nil {
		panic("starting binary: " + err.Error())
	}

	// Monitor for unexpected binary exit.
	binaryDied := make(chan error, 1)
	go func() {
		binaryDied <- cmd.Wait()
	}()

	// -----------------------------------------------------------------------
	// 6. Wait for readiness.
	// -----------------------------------------------------------------------
	if err := waitForHealthy(ctx, healthAddr, binaryDied); err != nil {
		// Binary crashed or didn't become healthy — stop it and fail.
		cmd.Process.Signal(syscall.SIGKILL) //nolint:errcheck
		panic("waiting for binary readiness: " + err.Error())
	}

	// -----------------------------------------------------------------------
	// 7. Run tests.
	// -----------------------------------------------------------------------
	code := m.Run()

	// -----------------------------------------------------------------------
	// 8. Teardown: stop binary, then envtest.
	// -----------------------------------------------------------------------
	stopBinary(cmd, binaryDied)
	cancel()
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
	}
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// Envtest binary resolution
// ---------------------------------------------------------------------------

// resolveEnvtestAssets returns the path to envtest binaries (etcd,
// kube-apiserver). If KUBEBUILDER_ASSETS is already set, it's used as-is.
// Otherwise, setup-envtest is invoked to resolve the path — it downloads
// binaries on first use and caches them in its OS-default store directory.
//
// This lets `go test ./experimental/...` work from any terminal or IDE
// without the Makefile wrapper that previously set KUBEBUILDER_ASSETS.
func resolveEnvtestAssets() string {
	if v := os.Getenv("KUBEBUILDER_ASSETS"); v != "" {
		return v
	}
	cmd := exec.Command("setup-envtest", "use", "1.35.x", "-p", "path")
	out, err := cmd.Output()
	if err != nil {
		// Output() captures stdout only; grab stderr from the ExitError for diagnostics.
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		fmt.Fprintf(os.Stderr, "setup-envtest not found or failed: %v\n%s\nRun: go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest\n", err, stderr)
		os.Exit(1)
	}
	return strings.TrimSpace(string(out))
}

// ---------------------------------------------------------------------------
// Binary build
// ---------------------------------------------------------------------------

// buildBinary compiles the experimental controller binary into ./build/ in
// the repository root. Go's build cache makes repeated builds near-instant.
func buildBinary() (string, error) {
	// Find the module root (where go.mod lives) so we can build relative to it.
	// The test runs from experimental/controller/test/, so walk up.
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
	cmd := exec.Command("go", "build", "-o", binaryPath, "./experimental/cmd/")
	cmd.Dir = moduleRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build: %w", err)
	}
	return binaryPath, nil
}

// ---------------------------------------------------------------------------
// Kubeconfig
// ---------------------------------------------------------------------------

// writeKubeconfig converts a *rest.Config into a kubeconfig file and returns
// its path. The caller is responsible for removing the file.
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

	f, err := os.CreateTemp("", "kro-test-kubeconfig-*.yaml")
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

// ---------------------------------------------------------------------------
// Binary lifecycle
// ---------------------------------------------------------------------------

// startBinary starts the kro binary as a subprocess and returns the health
// probe address and the exec.Cmd. The binary bootstraps its own CRDs
// (Graph, GraphRevision) before starting the controller.
//
// Controller logs are written to the shared log file (build/controller.log).
// Each test creates a unique namespace (graph-test-xxxxx), so to debug a
// specific test failure: grep graph-test-xxxxx build/controller.log
func startBinary(binaryPath, kubeconfigPath string, logFile *os.File) (healthAddr string, cmd *exec.Cmd, err error) {
	// Pick a random port for the health probe. We bind a listener, read back
	// the port, then close it before passing to the binary. There's a small
	// TOCTOU window, but envtest does the same thing and it's fine in practice.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("picking health port: %w", err)
	}
	healthAddr = ln.Addr().String()
	ln.Close()

	cmd = exec.Command(binaryPath,
		"--bootstrap",
		"--health-probe-bind-address="+healthAddr,
		"--metrics-bind-address=0",
		"--max-workers=32",
	)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("starting binary: %w", err)
	}
	return healthAddr, cmd, nil
}

// waitForHealthy polls the binary's health endpoint until it responds 200,
// or fails if the binary dies or the context is cancelled.
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

// stopBinary sends SIGTERM to the binary and waits for it to exit. If it
// doesn't exit within 10 seconds, it sends SIGKILL. A non-zero exit code
// after SIGTERM is logged as a warning — it may indicate a shutdown bug.
func stopBinary(cmd *exec.Cmd, binaryDied <-chan error) {
	// Check if already dead.
	select {
	case <-binaryDied:
		return
	default:
	}

	cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck
	start := time.Now()

	select {
	case err := <-binaryDied:
		elapsed := time.Since(start)
		if elapsed > 2*time.Second {
			fmt.Fprintf(os.Stderr, "binary shutdown took %s (>2s) — investigate\n", elapsed.Round(time.Millisecond))
		}
		if err != nil {
			// Non-zero exit after SIGTERM is expected (exit code 1 from
			// signal handling). Only log truly unexpected errors.
			if exitErr, ok := err.(*exec.ExitError); ok {
				if exitErr.ExitCode() > 1 {
					fmt.Fprintf(os.Stderr, "binary exited with code %d after SIGTERM\n", exitErr.ExitCode())
				}
			} else {
				fmt.Fprintf(os.Stderr, "binary exit error: %v\n", err)
			}
		}
	case <-time.After(10 * time.Second):
		fmt.Fprintln(os.Stderr, "binary did not exit within 10s after SIGTERM, sending SIGKILL")
		cmd.Process.Signal(syscall.SIGKILL) //nolint:errcheck
		<-binaryDied
	}
}
