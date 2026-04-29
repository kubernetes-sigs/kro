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

package metrics

import "github.com/prometheus/client_golang/prometheus"

// Register registers all kro metrics with the given Prometheus registerer.
func Register(registry prometheus.Registerer) {
	// Wire client-go adapters first so metrics are captured immediately,
	// before the collectors become scrapeable.
	registerClientGoAdapters()

	registry.MustRegister(
		// CEL
		ExprEvalTotal,
		ExprEvalDuration,

		// Runtime
		NodeEvalTotal,
		NodeEvalDuration,
		NodeEvalErrorsTotal,
		RuntimeCreationTotal,
		RuntimeCreationDuration,
		NodeIgnoredCheckTotal,
		NodeIgnoredTotal,
		NodeReadyCheckTotal,
		NodeNotReadyTotal,
		CollectionSize,

		// Dynamic controller
		DynReconcileTotal,
		DynRequeueTotal,
		DynReconcileDuration,
		DynGVRCount,
		DynQueueLength,
		DynHandlerCount,
		DynHandlerAttachTotal,
		DynHandlerDetachTotal,
		DynHandlerErrorsTotal,
		DynInformerSyncDuration,
		DynInformerEventsTotal,
		DynWatchCount,
		DynInstanceWatchCount,
		DynWatchRequestCount,
		DynRouteTotal,
		DynRouteMatchTotal,

		// Instance controller
		InstanceStateTransitionsTotal,
		InstanceReconcileDurationSeconds,
		InstanceReconcileTotal,
		InstanceReconcileErrorsTotal,
		InstanceGraphResolutionSuccessTotal,
		InstanceGraphResolutionFailuresTotal,
		InstanceGraphResolutionPendingTotal,
		InstanceConditionCurrentStatusSeconds,

		// RGD controller
		RGDGraphBuildTotal,
		RGDGraphBuildDuration,
		RGDGraphBuildErrorsTotal,
		RGDStateTransitionsTotal,
		RGDDeletionsTotal,
		RGDDeletionDuration,
		RGDGraphRevisionIssueTotal,
		RGDGraphRevisionWaitTotal,
		RGDGraphRevisionResolutionTotal,
		RGDGraphRevisionRegistryMissTotal,
		RGDGraphRevisionGCDeletedTotal,
		RGDGraphRevisionGCErrorsTotal,

		// GraphRevision controller
		GraphRevisionCompileTotal,
		GraphRevisionCompileDuration,
		GraphRevisionStatusUpdateErrorsTotal,
		GraphRevisionActivationDeferredTotal,
		GraphRevisionFinalizerEvictionsTotal,

		// Schema resolver
		SchemaResolverCacheHitsTotal,
		SchemaResolverCacheMissesTotal,
		SchemaResolverCacheSize,
		SchemaResolverCacheEvictionsTotal,
		SchemaResolverAPICallDuration,
		SchemaResolverSingleflightDeduplicatedTotal,
		SchemaResolutionErrorsTotal,

		// Revision registry
		GraphRevisionRegistryEntries,
		GraphRevisionRegistryTransitions,
		GraphRevisionRegistryEvictions,

		// Client-go
		requestLatency,
		rateLimiterLatency,
		requestSize,
		responseSize,
		requestRetry,
	)
}
