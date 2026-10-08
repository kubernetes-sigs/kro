// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
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
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	ctrlgraph "github.com/kubernetes-sigs/kro/pkg/controller/graph"
	ctrlinstance "github.com/kubernetes-sigs/kro/pkg/controller/instance"
	"github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/testutil/environment"
)

// These specs run the watch managers as a limited RBAC identity on their own
// envtest control plane, so the apiserver itself returns Forbidden on
// list/watch. Nothing is faked: the blocked-watch classification, the
// WatchesHealthy condition and the Register fast-fail are all driven by real
// apiserver responses.
//
// The watch identity can read everything except what each spec withholds.
// Granting the missing verb mid-spec exercises the recovery path.

const watchUserRBACRoleName = "watch-user-reads"

// newWatchUserEnv boots an isolated environment whose watch managers
// impersonate watchUser, and binds that user to a ClusterRole that permits
// list/watch on every resource except those in denied.
func newWatchUserEnv(ctx SpecContext, watchUser string, denied ...string) *environment.Environment {
	testEnv, err := environment.New(ctx, environment.ControllerConfig{
		AllowCRDDeletion: true,
		ReconcileConfig: ctrlinstance.ReconcileConfig{
			DefaultRequeueDuration: 5 * time.Second,
		},
		LogWriter: GinkgoWriter,
		WatchUser: watchUser,
	})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		Expect(stopEnvironmentWithRetry(testEnv)).To(Succeed())
	})

	// A broad read grant with explicit carve-outs is awkward in RBAC (rules
	// are additive). Instead grant list/watch on every known group except the
	// denied resources by enumerating the resources the specs touch.
	rules := []rbacv1.PolicyRule{}
	for _, r := range []struct{ group, resource string }{
		{"", "configmaps"},
		{"", "namespaces"},
		{"", "secrets"},
		{krov1alpha1.KRODomainName, "*"},
	} {
		skip := false
		for _, d := range denied {
			if d == r.group+"/"+r.resource {
				skip = true
			}
		}
		if skip {
			continue
		}
		rules = append(rules, rbacv1.PolicyRule{
			APIGroups: []string{r.group},
			Resources: []string{r.resource},
			Verbs:     []string{"list", "watch"},
		})
	}
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: watchUserRBACRoleName},
		Rules:      rules,
	}
	ExpectWithOffset(1, testEnv.Client.Create(ctx, role)).To(Succeed())
	ExpectWithOffset(1, testEnv.Client.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: watchUserRBACRoleName},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: watchUserRBACRoleName},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: watchUser}},
	})).To(Succeed())
	return testEnv
}

// grantWatch adds list/watch on group/resource to the watch user's role.
func grantWatch(ctx SpecContext, testEnv *environment.Environment, group, resource string) {
	role := &rbacv1.ClusterRole{}
	ExpectWithOffset(1, testEnv.Client.Get(ctx, types.NamespacedName{Name: watchUserRBACRoleName}, role)).To(Succeed())
	role.Rules = append(role.Rules, rbacv1.PolicyRule{
		APIGroups: []string{group},
		Resources: []string{resource},
		Verbs:     []string{"list", "watch"},
	})
	ExpectWithOffset(1, testEnv.Client.Update(ctx, role)).To(Succeed())
}

func unstructuredCondition(obj *unstructured.Unstructured, condType string) map[string]any {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == condType {
			return m
		}
	}
	return nil
}

var _ = Describe("WatchesHealthy", func() {
	It("reports a Forbidden child watch on the instance and recovers when access is granted", func(ctx SpecContext) {
		testEnv := newWatchUserEnv(ctx, "kro-watch-user", "/configmaps")

		suffix := rand.String(5)
		rgdName := "wh-instance-" + suffix
		kind := "WhInstance" + suffix
		rgd := configmapRGD(rgdName, kind)
		Expect(testEnv.Client.Create(ctx, rgd)).To(Succeed())

		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			g.Expect(testEnv.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)).To(Succeed())
			g.Expect(fresh.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 30*time.Second, 250*time.Millisecond).Should(Succeed())

		ns := testEnv.CreateNamespace(GinkgoT())
		instance := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": fmt.Sprintf("%s/v1alpha1", krov1alpha1.KRODomainName),
			"kind":       kind,
			"metadata":   map[string]any{"name": "inst", "namespace": ns},
			"spec":       map[string]any{"data": "v"},
		}}
		Expect(testEnv.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) { _ = testEnv.Client.Delete(ctx, instance) })

		// Apply does not wait on the watch: the ConfigMap lands even though
		// the watch on configmaps is Forbidden.
		Eventually(func(g Gomega) {
			cm := &corev1.ConfigMap{}
			g.Expect(testEnv.Client.Get(ctx, types.NamespacedName{Name: "cm-inst", Namespace: ns}, cm)).To(Succeed())
		}, 30*time.Second, 250*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(testEnv.Client.Get(ctx, client.ObjectKeyFromObject(instance), instance)).To(Succeed())
			c := unstructuredCondition(instance, ctrlinstance.WatchesHealthy)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c["status"]).To(Equal("False"))
			g.Expect(c["reason"]).To(Equal("WatchBlocked"))
			g.Expect(c["message"]).To(ContainSubstring("configmaps"))
			g.Expect(c["message"]).To(ContainSubstring("Forbidden"))
		}, 30*time.Second, 250*time.Millisecond).Should(Succeed())

		grantWatch(ctx, testEnv, "", "configmaps")

		// The informer's reflector retries on its own and the recovery event
		// re-enqueues the instance. No edit to the instance is needed.
		Eventually(func(g Gomega) {
			g.Expect(testEnv.Client.Get(ctx, client.ObjectKeyFromObject(instance), instance)).To(Succeed())
			c := unstructuredCondition(instance, ctrlinstance.WatchesHealthy)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c["status"]).To(Equal("True"))
			g.Expect(c["reason"]).To(Equal("AllWatchesSynced"))
		}, 90*time.Second, 500*time.Millisecond).Should(Succeed())
	})

	It("reports a Forbidden child watch on a Graph and recovers when access is granted", func(ctx SpecContext) {
		testEnv := newWatchUserEnv(ctx, "kro-watch-user", "/configmaps")
		t := GinkgoT()
		ns := testEnv.CreateNamespace(t)

		g := &krov1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "wh", Namespace: ns},
			Spec: krov1alpha1.GraphSpec{
				Nodes: []krov1alpha1.Node{{
					ID: "cm",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "wh-cm"},
						"data":       map[string]any{"k": "v"},
					}),
				}},
			},
		}
		testEnv.CreateGraph(t, g)

		Eventually(func(gm Gomega) {
			cm := &corev1.ConfigMap{}
			gm.Expect(testEnv.Client.Get(ctx, types.NamespacedName{Name: "wh-cm", Namespace: ns}, cm)).To(Succeed())
		}, 30*time.Second, 250*time.Millisecond).Should(Succeed())

		blocked := testEnv.AwaitCondition(t, client.ObjectKeyFromObject(g), ctrlgraph.WatchesHealthy, metav1.ConditionFalse, 30*time.Second)
		Expect(*blocked.Reason).To(Equal("WatchBlocked"))
		Expect(*blocked.Message).To(ContainSubstring("configmaps"))
		Expect(*blocked.Message).To(ContainSubstring("Forbidden"))

		grantWatch(ctx, testEnv, "", "configmaps")

		healthy := testEnv.AwaitCondition(t, client.ObjectKeyFromObject(g), ctrlgraph.WatchesHealthy, metav1.ConditionTrue, 90*time.Second)
		Expect(*healthy.Reason).To(Equal("AllWatchesSynced"))
	})

	It("fails RGD registration fast when the parent kind cannot be watched and recovers when access is granted", func(ctx SpecContext) {
		suffix := rand.String(5)
		rgdName := "wh-parent-" + suffix
		kind := "WhParent" + suffix
		plural := metadata.ResolvePlural(kind, "")

		// Deny the whole kro.run group so the generated instance kind is
		// Forbidden to the watch user. Nothing else in this spec needs it.
		testEnv := newWatchUserEnv(ctx, "kro-watch-user", krov1alpha1.KRODomainName+"/*")

		rgd := configmapRGD(rgdName, kind)
		Expect(testEnv.Client.Create(ctx, rgd)).To(Succeed())

		// The sync timeout is 30s. A Forbidden parent watch must surface as
		// ControllerReady=False well before that instead of waiting it out.
		start := time.Now()
		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			g.Expect(testEnv.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)).To(Succeed())
			c := findConditionByTypeInEnv(fresh.Status.Conditions, krov1alpha1.ConditionType(resourcegraphdefinition.ControllerReady))
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(c.Reason).NotTo(BeNil())
			g.Expect(*c.Reason).To(Equal("FailedToStart"))
			g.Expect(c.Message).NotTo(BeNil())
			g.Expect(*c.Message).To(ContainSubstring("forbidden"))
		}, 20*time.Second, 250*time.Millisecond).Should(Succeed())
		Expect(time.Since(start)).To(BeNumerically("<", 20*time.Second))

		grantWatch(ctx, testEnv, krov1alpha1.KRODomainName, plural)

		// Register re-runs on the RGD's retry and now succeeds.
		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			g.Expect(testEnv.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)).To(Succeed())
			g.Expect(fresh.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			c := findConditionByTypeInEnv(fresh.Status.Conditions, krov1alpha1.ConditionType(resourcegraphdefinition.ControllerReady))
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
		}, 90*time.Second, 500*time.Millisecond).Should(Succeed())
	})
})
