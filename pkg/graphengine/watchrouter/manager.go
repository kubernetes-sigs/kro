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

package watchrouter

import (
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/metadata"

	kwatch "github.com/kubernetes-sigs/kro/pkg/watch"
)

// Manager owns the lifecycle of one shared informer per GVR. It is the shared
// implementation from pkg/watch; the alias keeps the watchrouter import path
// stable. The graph engine runs informers without metrics instrumentation, so
// Manager.Metrics is left nil.
type Manager = kwatch.Manager

// NewManager constructs a Manager that fans informer events into onEvent. See
// pkg/watch for the full contract.
func NewManager(client metadata.Interface, resync time.Duration, onEvent EventHandler, log logr.Logger) *Manager {
	return kwatch.NewManager(client, resync, onEvent, log)
}
