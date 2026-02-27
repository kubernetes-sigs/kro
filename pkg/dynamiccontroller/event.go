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

package dynamiccontroller

import "k8s.io/apimachinery/pkg/runtime/schema"

// EventType identifies the kind of change that triggered an event.
type EventType string

const (
	EventAdd    EventType = "add"
	EventUpdate EventType = "update"
	EventDelete EventType = "delete"
)

// Event is a normalized watch event emitted by the WatchManager.
// Consumers decide what to act on -- no old/new comparison or generation
// filtering is performed by the watch layer.
type Event struct {
	Type      EventType
	GVR       schema.GroupVersionResource
	Name      string
	Namespace string
	Labels    map[string]string
}

// EventHandler processes a single watch event.
type EventHandler func(event Event)
