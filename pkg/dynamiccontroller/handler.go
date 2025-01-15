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

	ctrl "sigs.k8s.io/controller-runtime"
)

// HandlerFunc processes a single resource, represented by a controller runtime Request.
// It is expected to be idempotent and should handle its own logging and error reporting.
type HandlerFunc func(ctx context.Context, req ctrl.Request) error

// Handler wraps a HandlerFunc with metadata about its origin and identity.
// This metadata helps track changes in the handler's implementation and the ResourceGroup
// version that produced it.
type Handler struct {
	// Func is the handler function that processes individual resources
	Func HandlerFunc

	// ResourceGroupRevision indicates which version of the ResourceGroup CR
	// was responsible for generating this handler
	ResourceGroupRevision int64

	// Hash uniquely identifies the handler function implementation.
	// This allows for quick equality checking between handlers.
	Hash string
}
