// Copyright 2025 The Kube Resource Orchestrator Authors
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

// Inspired by https://github.com/knative/pkg/tree/97c7258e3a98b81459936bc7a29dc6a9540fa357/apis,
// but we chose to diverge due to the unacceptably large dependency closure of knative/pkg.

package apis

import (
	"github.com/kro-run/kro/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Object interface {
	client.Object
	GetConditions() []v1alpha1.Condition
	SetConditions([]v1alpha1.Condition)
}

const (
	// ConditionReady specifies that the resource is ready.
	// For long-running resources.
	ConditionReady = "Ready"
	// ConditionSucceeded specifies that the resource has finished.
	// For resource which run to completion.
	ConditionSucceeded = "Succeeded"
)
