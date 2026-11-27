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

package graphrevision

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/release-utils/version"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/features"
)

// TestUpdateStatusRecordsCELEnvironmentVersion pins the kro version onto a
// compiled revision: a pinned instance needs to know which CEL environment its
// revision was verified against.
func TestUpdateStatusRecordsCELEnvironmentVersion(t *testing.T) {
	for _, tt := range []struct {
		name        string
		gateEnabled bool
		want        string
	}{
		{name: "gate on records the kro version", gateEnabled: true, want: version.GetVersionInfo().GitVersion},
		{name: "gate off records nothing", gateEnabled: false, want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, features.FeatureGate, features.GraphRevisionPinning, tt.gateEnabled)

			scheme := runtime.NewScheme()
			require.NoError(t, internalv1alpha1.AddToScheme(scheme))
			require.NoError(t, v1alpha1.AddToScheme(scheme))

			obj := newTestGraphRevision("cel-env")
			cl := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&internalv1alpha1.GraphRevision{}).
				WithObjects(obj).Build()
			reconciler := &GraphRevisionReconciler{Client: cl}

			require.NoError(t, reconciler.updateStatus(context.Background(), obj, []string{"cm"}, nil))

			stored := &internalv1alpha1.GraphRevision{}
			require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(obj), stored))
			assert.Equal(t, tt.want, stored.Status.CELEnvironmentVersion)
			assert.Equal(t, []string{"cm"}, stored.Status.TopologicalOrder)
		})
	}
}
