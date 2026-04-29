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

// Package testenv provides a test environment for the experimental Graph
// controller. It manages the full lifecycle: envtest (kube-apiserver + etcd),
// binary build, subprocess start/stop, and health checks.
//
// Both the standard Go testing harness (main_test.go) and the Ginkgo-based
// upstream compat harness (setup_test.go) use this package.
package testenv

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Environment manages envtest + the Graph controller subprocess.
type Environment struct {
	// CRDDirectoryPaths are additional CRD directories to load into envtest
	// (e.g., ACK CRDs for external ref tests). Graph and GraphRevision CRDs
	// are always loaded from crds/.
	CRDDirectoryPaths []string

	// ResyncInterval overrides the per-node resync timer interval for the
	// controller subprocess. Zero uses the binary's default (30m).
	// Test suites that depend on fast resync should set this explicitly
	// rather than relying on shared infrastructure defaults.
	ResyncInterval time.Duration

	// LogWriter receives controller subprocess output. If nil, a file is
	// created at build/controller.log.
	LogWriter io.Writer

	testEnv        *envtest.Environment
	restConfig     *rest.Config
	binaryCmd      *exec.Cmd
	binaryDied     chan error
	kubeconfigPath string
	logFile        *os.File // only set if we created it
}

// Start builds the binary, starts envtest, starts the subprocess, and waits
// for health. Returns the REST config for creating clients.
func (e *Environment) Start() (*rest.Config, error) {
	moduleRoot := FindModuleRoot()

	// Build binary.
	binaryPath, err := buildBinary(moduleRoot)
	if err != nil {
		return nil, fmt.Errorf("building binary: %w", err)
	}

	// Set up log writer. If KRO_TEST_TAG is set, per-run log lives at
	// build/controller-<tag>.log — lets multiple test files keep their
	// own logs without clobbering each other.
	logWriter := e.LogWriter
	if logWriter == nil {
		logName := "controller.log"
		if tag := os.Getenv("KRO_TEST_TAG"); tag != "" {
			logName = "controller-" + tag + ".log"
		}
		logPath := filepath.Join(filepath.Dir(binaryPath), logName)
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return nil, fmt.Errorf("creating log file: %w", err)
		}
		e.logFile = f
		logWriter = f
		fmt.Fprintf(os.Stderr, "controller logs: %s\n", logPath)
	}

	// Start envtest with chart CRDs + any additional test CRDs.
	chartCRDDir := filepath.Join(moduleRoot, "experimental", "crds")
	crdPaths := append([]string{chartCRDDir}, e.CRDDirectoryPaths...)
	e.testEnv = &envtest.Environment{
		BinaryAssetsDirectory: ResolveEnvtestAssets(),
		CRDDirectoryPaths:     crdPaths,
	}
	cfg, err := e.testEnv.Start()
	if err != nil {
		e.cleanup()
		return nil, fmt.Errorf("starting envtest: %w", err)
	}
	e.restConfig = cfg

	// Write kubeconfig.
	e.kubeconfigPath, err = writeKubeconfig(cfg)
	if err != nil {
		e.cleanup()
		return nil, fmt.Errorf("writing kubeconfig: %w", err)
	}

	// Start binary.
	healthAddr, cmd, err := startBinary(binaryPath, e.kubeconfigPath, logWriter, e.ResyncInterval)
	if err != nil {
		e.cleanup()
		return nil, fmt.Errorf("starting binary: %w", err)
	}
	e.binaryCmd = cmd
	e.binaryDied = make(chan error, 1)
	go func() { e.binaryDied <- cmd.Wait() }()

	// Wait for health.
	if err := waitForHealthy(healthAddr, e.binaryDied); err != nil {
		e.cleanup()
		return nil, fmt.Errorf("waiting for health: %w", err)
	}

	return cfg, nil
}

// Client returns a controller-runtime client configured for the envtest
// API server with the default scheme.
func (e *Environment) Client() (client.Client, error) {
	return client.New(e.restConfig, client.Options{Scheme: scheme.Scheme})
}

// Stop tears down the subprocess and envtest.
func (e *Environment) Stop() {
	e.cleanup()
}

func (e *Environment) cleanup() {
	if e.binaryCmd != nil && e.binaryCmd.Process != nil {
		stopBinary(e.binaryCmd, e.binaryDied)
	}
	if e.kubeconfigPath != "" {
		os.Remove(e.kubeconfigPath)
	}
	if e.testEnv != nil {
		e.testEnv.Stop() //nolint:errcheck
	}
	if e.logFile != nil {
		e.logFile.Close()
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func FindModuleRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		panic("os.Getwd: " + err.Error())
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("cannot find go.mod from " + wd)
		}
		dir = parent
	}
}

func buildBinary(moduleRoot string) (string, error) {
	buildDir := filepath.Join(moduleRoot, "build")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		return "", fmt.Errorf("creating build dir: %w", err)
	}
	binaryPath := filepath.Join(buildDir, "kro-graph-controller")
	cmd := exec.Command("go", "build", "-o", binaryPath, "./experimental/cmd/")
	cmd.Dir = moduleRoot
	cmd.Stdout = os.Stderr // build output goes to stderr, not the log file
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

func startBinary(binaryPath, kubeconfigPath string, logWriter io.Writer, resyncInterval time.Duration) (string, *exec.Cmd, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("picking health port: %w", err)
	}
	healthAddr := ln.Addr().String()
	ln.Close()

	args := []string{
		"--health-probe-bind-address=" + healthAddr,
		"--metrics-bind-address=0",
		"--pprof-bind-address=0",
		"--max-workers=32",
	}
	if resyncInterval > 0 {
		args = append(args, "--node-resync-interval="+resyncInterval.String())
	}

	cmd := exec.Command(binaryPath, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfigPath)
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("starting binary: %w", err)
	}
	return healthAddr, cmd, nil
}

func waitForHealthy(healthAddr string, binaryDied <-chan error) error {
	healthURL := "http://" + healthAddr + "/healthz"
	httpClient := &http.Client{Timeout: 500 * time.Millisecond}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(60 * time.Second)

	for {
		select {
		case err := <-binaryDied:
			return fmt.Errorf("binary exited during startup: %v", err)
		case <-timeout:
			return fmt.Errorf("binary not healthy within 60s at %s", healthURL)
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

// ResolveEnvtestAssets returns the path to envtest binaries. Uses
// KUBEBUILDER_ASSETS if set, otherwise invokes setup-envtest.
func ResolveEnvtestAssets() string {
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
		fmt.Fprintf(os.Stderr,
			"setup-envtest not found or failed: %v\n%s\nRun: go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest\n",
			err, stderr)
		os.Exit(1)
	}
	return strings.TrimSpace(string(out))
}
