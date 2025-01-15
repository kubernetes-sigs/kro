// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package dynamiccontroller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

var _ Controller = &DynamicController{}

// Controller is an interface that describes objects capable of managing the lifecycle
// and configuration of GroupVersionResource (GVR) watches. Each GVR can be watched
// independently with its own handler function and configuration. The ResourceGroup
// revision number tracks which version of the ResourceGroup CR was used to generate
// the current handler and configuration. The controller ensures watches can be started,
// stopped, and reconfigured without impacting other running watches.
//
// The underlying queue implementation used to process watch events is hidden from
// consumers of this interface.
type Controller interface {
	// Run starts the controller and blocks until the context is canceled.
	Run(ctx context.Context) error

	// Stop gracefully shuts down the controller with the given timeout duration.
	// Returns error if shutdown fails or timeout is exceeded.
	Stop(timeout time.Duration) error

	// WatchGVRWithConfig starts watching a GVR using the provided handler and configuration.
	// The resourceGroupRevision should match the metadata.revision of the ResourceGroup CR
	// that generated this handler configuration. If the GVR is already being watched with
	// the same revision, this is a no-op.
	WatchGVRWithConfig(ctx context.Context, gvr schema.GroupVersionResource, handler Handler, resync time.Duration) error

	// UnwatchGVR stops watching a GVR and cleans up associated resources.
	// If the GVR is not being watched, this is a no-op.
	UnwatchGVR(ctx context.Context, gvr schema.GroupVersionResource) error

	// GetGVRWatchStatus returns the current watch configuration for a GVR.
	// Returns the ResourceGroup revision that generated the current config,
	// resync period, and whether the GVR is being watched.
	GetGVRWatchStatus(gvr schema.GroupVersionResource) WatchStatus

	// UpdateGVRHandler updates the handler for an existing watch with a new
	// handler generated from an updated ResourceGroup revision.
	// Returns an error if the GVR is not being watched.
	UpdateGVRHandler(ctx context.Context, gvr schema.GroupVersionResource, handler Handler) error

	// UpdateGVRResyncPeriod updates the resync period for an existing watch.
	// Returns an error if the GVR is not being watched.
	UpdateGVRResyncPeriod(ctx context.Context, gvr schema.GroupVersionResource, period time.Duration) error

	// Resync triggers an immediate list operation for the specified GVR.
	// Returns an error if the GVR is not being watched.
	ResyncGVR(ctx context.Context, gvr schema.GroupVersionResource) error
}
