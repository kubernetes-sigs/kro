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

// Package watch provides the shared informer-fleet machinery used by kro's
// controllers. A [Manager] owns one metadata-only informer per GVR, starting
// them lazily and reference-counting owners so an informer stops as soon as
// its last owner releases it. Informer callbacks are normalized into [Event]
// values and delivered to a single [EventHandler].
//
// The package is deliberately controller-agnostic: it does not know about
// work queues, parent CRDs, or controller-runtime. Consumers (the RGD/instance
// dynamic controller and the graph-engine watch router) layer their own event
// routing and reconcile dispatch on top of a shared Manager.
package watch

import "k8s.io/apimachinery/pkg/runtime/schema"

// EventType identifies the kind of change that triggered an event.
type EventType string

const (
	EventAdd    EventType = "add"
	EventUpdate EventType = "update"
	EventDelete EventType = "delete"
)

// Event is a normalized watch event emitted by the [Manager].
// Consumers decide what to act on -- no old/new comparison or generation
// filtering is performed by the watch layer.
type Event struct {
	Type      EventType
	GVR       schema.GroupVersionResource
	Name      string
	Namespace string
	Labels    map[string]string
	// OldLabels holds the labels from the previous version of the object
	// (populated only for update events). Used by collection watches to detect
	// label changes that cause an object to enter or leave a selector match.
	OldLabels map[string]string
}

// EventHandler processes a single watch event. The [Manager] invokes the
// handler synchronously from informer goroutines, so implementations must keep
// work fast -- push to a queue or fan-out channel for heavy work.
type EventHandler func(event Event)
