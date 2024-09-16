// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package errors

import "errors"

// meh i don't like error packages, keeping it temporarily

var (
	// ErrResourceNotFound is returned when a resource is not found in the collection.
	ErrResourceNotFound = errors.New("resource not found")
	// ErrInvalidReference is returned when a reference is not valid.
	ErrInvalidReference = errors.New("invalid reference")
	// ErrCyclicDependency is returned when a cyclic dependency is found.
	ErrCyclicReference = errors.New("cyclic reference")
	// ErrSelfReference is returned when a resource references itself.
	ErrSelfReference = errors.New("self reference")
)
