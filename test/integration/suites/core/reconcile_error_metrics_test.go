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

package core_test

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/metrics"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

// instance_reconcile_errors_total counts reconcile-level failures.
//
// The counter is what operators alert on to find instances the controller
// cannot make progress on, so its scale has to stay stable: a resource-level
// apply failure that the controller expects to retry is reported through the
// instance's conditions, not by incrementing this counter. Otherwise a single
// instance waiting on something external — a namespace that doesn't exist
// yet, an admission webhook that is temporarily rejecting writes — produces
// unbounded error counts, and any absolute threshold built on this series
// starts firing on healthy-but-waiting workloads.
var _ = Describe("ReconcileErrorMetrics", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	It("does not count a retryable resource apply failure as a reconcile error", func(ctx SpecContext) {
		// The child targets a namespace that does not exist, so every apply
		// fails with a retryable error for as long as the spec runs.
		rgd := generator.NewResourceGraphDefinition("test-reconcile-error-metric",
			generator.WithSchema(
				"TestReconcileErrorMetric", "v1alpha1",
				map[string]any{
					"name": "string",
				},
				nil,
			),
			generator.WithResource("cm", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "${schema.spec.name}-cm",
					"namespace": "does-not-exist-" + rand.String(5),
				},
				"data": map[string]any{
					"managed": "yes",
				},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		gvr := schema.GroupVersionResource{
			Group:    krov1alpha1.KRODomainName,
			Version:  "v1alpha1",
			Resource: "testreconcileerrormetrics",
		}.String()
		before := testutil.ToFloat64(metrics.InstanceReconcileErrorsTotal.WithLabelValues(gvr))

		name := "reconcile-error-metric"
		instance := newInstance("TestReconcileErrorMetric", name, namespace, map[string]any{
			"name": name,
		})
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// The failure has to be visible to the user, otherwise this spec would
		// pass simply because nothing happened.
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)).To(Succeed())
			g.Expect(instanceConditions(instance)).To(ContainSubstring("does-not-exist"),
				"the instance should report the failing apply; conditions: %s",
				instanceConditions(instance))
		}, 60*time.Second, 2*time.Second).WithContext(ctx).Should(Succeed())

		// Give the controller several reconcile attempts, then check the counter
		// did not track them.
		Consistently(func(g Gomega, ctx SpecContext) {
			current := testutil.ToFloat64(metrics.InstanceReconcileErrorsTotal.WithLabelValues(gvr))
			g.Expect(current).To(Equal(before),
				"a retryable apply failure incremented instance_reconcile_errors_total "+
					"(%v -> %v); alerts with absolute thresholds on this series would fire "+
					"for an instance that is merely waiting", before, current)
		}, 20*time.Second, 2*time.Second).WithContext(ctx).Should(Succeed())
	})
})
