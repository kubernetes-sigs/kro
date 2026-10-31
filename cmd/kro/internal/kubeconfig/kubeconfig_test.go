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

package kubeconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const twoContexts = `apiVersion: v1
kind: Config
current-context: broken
clusters:
- name: broken
  cluster:
    server: https://127.0.0.1:1
- name: good
  cluster:
    server: https://good.example.com:6443
contexts:
- name: broken
  context:
    cluster: broken
    user: u
- name: good
  context:
    cluster: good
    user: u
users:
- name: u
  user:
    token: dummy
`

const singleContext = `apiVersion: v1
kind: Config
current-context: env
clusters:
- name: env
  cluster:
    server: https://env.example.com:6443
contexts:
- name: env
  context:
    cluster: env
    user: u
users:
- name: u
  user:
    token: dummy
`

func writeKubeconfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

func TestRESTConfig(t *testing.T) {
	explicit := writeKubeconfig(t, twoContexts)
	fromEnv := writeKubeconfig(t, singleContext)

	tests := []struct {
		name     string
		opts     Options
		envPath  string
		wantHost string
		wantErr  bool
	}{
		{
			name:     "explicit kubeconfig uses its current-context",
			opts:     Options{KubeConfigPath: explicit},
			envPath:  fromEnv,
			wantHost: "https://127.0.0.1:1",
		},
		{
			name:     "context overrides current-context",
			opts:     Options{KubeConfigPath: explicit, Context: "good"},
			envPath:  fromEnv,
			wantHost: "https://good.example.com:6443",
		},
		{
			name:     "falls back to KUBECONFIG when no path is given",
			opts:     Options{},
			envPath:  fromEnv,
			wantHost: "https://env.example.com:6443",
		},
		{
			name:     "context applies to KUBECONFIG",
			opts:     Options{Context: "good"},
			envPath:  explicit,
			wantHost: "https://good.example.com:6443",
		},
		{
			name:    "unknown context is an error",
			opts:    Options{KubeConfigPath: explicit, Context: "missing"},
			envPath: fromEnv,
			wantErr: true,
		},
		{
			name:    "missing explicit kubeconfig is an error",
			opts:    Options{KubeConfigPath: filepath.Join(t.TempDir(), "nope")},
			envPath: fromEnv,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KUBECONFIG", tt.envPath)

			cfg, err := tt.opts.RESTConfig()
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantHost, cfg.Host)
		})
	}
}
