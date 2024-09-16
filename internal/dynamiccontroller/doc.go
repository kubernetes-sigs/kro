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

// The main purpose of this package is to expose a form of dynamic controller
// that can be used to create and manage "micro" controllers that are used to
// manage resources in Kubernetes.
//
// In it's core, Symphony needs to be able to dynamically manage new resources
// that are defined by users. For each new resource, Symphony needs to create a
// a new informer to start watching for changes to the resource. And similarly,
// delete the informer when the resource is deleted. All while not affecting
// the performance of the system not disrupting the operation of other resources.
//
// This reminds me of the concept of Envoy hot restarts - this is not as critical
// as that but it is a similar concept.
//
// Why not just use the controller-runtime library?
// 1. The controller-runtime library is great for creating controllers that are
//    statically defined. Symphony needs to be able to create controllers that
//    are dynamically defined.
// 2. The controller-runtime library comes with a lot of overhead that Symphony
//    does not need. For example, Symphony does not need to use the leader election
//    feature of the controller-runtime library. Another example is metrics.
//    Symphony does not need to expose metrics.
// 3. The controller-runtime library is not flexible enough for Symphony's needs.
//    For example, Symphony needs to be able to dynamically create and delete
//    informers. The controller-runtime library does not provide a way to do this.
//
// In the future, we will need to add more features to this package. For example,
// we will need to implement a different flavor of leader election that is more
// suited to Symphony's needs (thinking sharding and CEL-cost-aware leader election).
//
// Ideally we would like to open source this package so that other projects can
// benefit from it.
