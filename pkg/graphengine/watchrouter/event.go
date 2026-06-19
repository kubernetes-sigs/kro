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

import "k8s.io/apimachinery/pkg/runtime/schema"

// EventType identifies the kind of informer change that produced an event.
type EventType string

const (
	EventAdd    EventType = "add"
	EventUpdate EventType = "update"
	EventDelete EventType = "delete"
)

// Event is a normalized informer event emitted by the Manager. The
// shape is deliberately small — Labels and OldLabels are populated only when
// useful (collection routing) so consumers don't have to deal with raw
// runtime.Objects.
type Event struct {
	Type      EventType
	GVR       schema.GroupVersionResource
	Name      string
	Namespace string
	Labels    map[string]string
	// OldLabels carries the previous version's labels on update events.
	// Used by collection watches to catch label-loss transitions where an
	// object stops matching a selector and the consumer still needs to
	// re-reconcile (e.g. to drop it from the membership set).
	OldLabels map[string]string
}

// EventHandler processes a single Event. The Manager invokes the
// handler synchronously from informer goroutines, so implementations must
// keep work fast — push to a queue or fan-out channel for heavy work.
type EventHandler func(event Event)
