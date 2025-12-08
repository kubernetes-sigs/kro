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

package watchtracker

import "k8s.io/apimachinery/pkg/runtime/schema"

// EventType represents the type of watch event from an informer.
type EventType string

const (
	// EventTypeSynced indicates the informer cache has synced.
	EventTypeSynced EventType = "Synced"
	// EventTypeError indicates a watch error occurred.
	EventTypeError EventType = "Error"
)

// Event represents a watch event from an informer (sync complete or error).
type Event struct {
	GVR   schema.GroupVersionResource
	Error error
	Type  EventType
}
