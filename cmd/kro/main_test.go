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

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/kubernetes-sigs/kro/pkg/features"
)

// runCLIEnv makes the test binary run the kro CLI instead of the tests, so a
// test can run the CLI in a child process with its own flags and environment.
const runCLIEnv = "KRO_TEST_RUN_CLI"

func TestMain(m *testing.M) {
	if os.Getenv(runCLIEnv) == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestFeatureGatesFlag(t *testing.T) {
	root := NewRootCommand()
	t.Cleanup(func() {
		require.NoError(t, features.FeatureGate.Set("CELOmitFunction=false"))
	})

	flag := root.PersistentFlags().Lookup("feature-gates")
	require.NotNil(t, flag, "root command should expose --feature-gates")
	assert.Contains(t, flag.Usage, string(features.CELOmitFunction))

	require.False(t, features.FeatureGate.Enabled(features.CELOmitFunction))

	require.NoError(t, root.ParseFlags([]string{"--feature-gates=CELOmitFunction=true"}))
	assert.True(t, features.FeatureGate.Enabled(features.CELOmitFunction))
}

// validate and generate have to talk to the cluster that --kubeconfig and
// --context select, not to the one that $KUBECONFIG and its current context
// point at.
func TestKubeconfigAndContextFlags(t *testing.T) {
	serverA, requestsA := newFakeAPIServer(t)
	serverB, requestsB := newFakeAPIServer(t)
	kubeconfigA := writeKubeconfig(t, "a", map[string]string{"a": serverA})
	kubeconfigB := writeKubeconfig(t, "b", map[string]string{"b": serverB})
	kubeconfigAB := writeKubeconfig(t, "a", map[string]string{"a": serverA, "b": serverB})
	rgdFile := filepath.Join(t.TempDir(), "rgd.yaml")
	require.NoError(t, os.WriteFile(rgdFile, []byte(testRGD), 0o600))

	tests := []struct {
		name         string
		kubeconfig   string // the value of $KUBECONFIG
		flags        []string
		command      []string // validate rgd when empty
		wantA, wantB bool
		wantOutput   string
		wantLogHost  string // the host that --verbose logs
	}{
		{
			name:       "$KUBECONFIG without flags",
			kubeconfig: kubeconfigA,
			wantA:      true,
		},
		{
			name:       "--kubeconfig instead of $KUBECONFIG",
			kubeconfig: kubeconfigA,
			flags:      []string{"--kubeconfig", kubeconfigB},
			wantB:      true,
		},
		{
			name:       "--context instead of the current context",
			kubeconfig: kubeconfigAB,
			flags:      []string{"--context", "b"},
			wantB:      true,
		},
		{
			name:       "generate crd with --context",
			kubeconfig: kubeconfigAB,
			flags:      []string{"--context", "b"},
			command:    []string{"generate", "crd"},
			wantB:      true,
		},
		{
			name:       "--context that doesn't exist",
			kubeconfig: kubeconfigAB,
			flags:      []string{"--context", "missing"},
			wantOutput: `context "missing" does not exist`,
		},
		{
			name:        "--verbose logs the cluster",
			kubeconfig:  kubeconfigAB,
			flags:       []string{"--context", "b", "--verbose"},
			wantB:       true,
			wantLogHost: serverB,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestsA.Store(0)
			requestsB.Store(0)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			command := tt.command
			if command == nil {
				command = []string{"validate", "rgd"}
			}
			args := slices.Concat(tt.flags, command, []string{"-f", rgdFile})
			cmd := exec.CommandContext(ctx, os.Args[0], args...)
			// HOME keeps the CLI away from the ~/.kube/config of whoever runs the test, and
			// with $KUBECONFIG set it doesn't try the in-cluster config when the test runs in a pod.
			cmd.Env = append(os.Environ(), runCLIEnv+"=1", "KUBECONFIG="+tt.kubeconfig, "HOME="+t.TempDir())
			// Validation fails against the fake servers. Only where the CLI connects matters.
			out, _ := cmd.CombinedOutput()

			assert.Equal(t, tt.wantA, requestsA.Load() > 0, "requests to context a, output:\n%s", out)
			assert.Equal(t, tt.wantB, requestsB.Load() > 0, "requests to context b, output:\n%s", out)
			if tt.wantOutput != "" {
				assert.Contains(t, string(out), tt.wantOutput)
			}
			if tt.wantLogHost != "" {
				assert.Contains(t, string(out), "Using cluster")
				assert.Contains(t, string(out), `"host": "`+tt.wantLogHost+`"`)
			} else {
				assert.NotContains(t, string(out), "Using cluster")
			}
		})
	}
}

// Every command reads the RGD file the same way, so a file the API server
// rejects fails before the CLI connects to the cluster.
func TestCommandsRejectInvalidRGDFile(t *testing.T) {
	server, requests := newFakeAPIServer(t)
	kubeconfig := writeKubeconfig(t, "a", map[string]string{"a": server})
	rgdFile := filepath.Join(t.TempDir(), "rgd.yaml")
	misspelled := strings.Replace(testRGD, "    kind: TestApp\n", "    kind: TestApp\n    gruop: example.com\n", 1)
	require.NoError(t, os.WriteFile(rgdFile, []byte(misspelled), 0o600))

	for _, command := range [][]string{
		{"validate", "rgd"},
		{"generate", "crd"},
		{"generate", "instance"},
		{"generate", "diagram"},
	} {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			requests.Store(0)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			cmd := exec.CommandContext(ctx, os.Args[0], append(command, "-f", rgdFile)...)
			cmd.Env = append(os.Environ(), runCLIEnv+"=1", "KUBECONFIG="+kubeconfig, "HOME="+t.TempDir())
			out, err := cmd.CombinedOutput()

			require.Error(t, err, "output:\n%s", out)
			assert.Contains(t, string(out), `strict decoding error: unknown field "spec.schema.gruop"`)
			assert.Zero(t, requests.Load(), "requests to the cluster, output:\n%s", out)
		})
	}
}

// newFakeAPIServer returns the URL of a server that counts its requests and
// answers each one with 404.
func newFakeAPIServer(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server.URL, &requests
}

// writeKubeconfig writes a kubeconfig with a context for each server, named
// like its key, and returns its path.
func writeKubeconfig(t *testing.T, currentContext string, servers map[string]string) string {
	t.Helper()
	config := clientcmdapi.NewConfig()
	for name, server := range servers {
		config.Clusters[name] = &clientcmdapi.Cluster{Server: server}
		config.AuthInfos[name] = &clientcmdapi.AuthInfo{}
		config.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: name}
	}
	config.CurrentContext = currentContext
	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, clientcmd.WriteToFile(*config, path))
	return path
}

const testRGD = `apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: testapps.kro.run
spec:
  schema:
    apiVersion: v1alpha1
    kind: TestApp
    spec:
      name: string
  resources:
    - id: config
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: ${schema.spec.name}
        data:
          key: value
`
