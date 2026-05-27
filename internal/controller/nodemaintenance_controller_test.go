/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	maintenancev1 "linode.com/node-maintenance-controller/api/v1"
)

var _ = Describe("NodeMaintenance Controller", func() {
	const (
		nodeName = "test-node"
		nmName   = nodeName // NodeMaintenance name == Node name
	)

	ctx := context.Background()

	// nmKey is cluster-scoped: no namespace.
	nmKey := types.NamespacedName{Name: nmName}
	nodeKey := types.NamespacedName{Name: nodeName}

	scheduledAt := metav1.Time{Time: time.Now().Add(2 * time.Hour)}

	var testNode *corev1.Node
	var testNM *maintenancev1.NodeMaintenance

	BeforeEach(func() {
		By("Creating the test Node")
		testNode = &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			},
			Spec: corev1.NodeSpec{
				ProviderID: "linode://12345",
			},
		}
		err := k8sClient.Get(ctx, nodeKey, &corev1.Node{})
		if errors.IsNotFound(err) {
			Expect(k8sClient.Create(ctx, testNode)).To(Succeed())
		}

		By("Creating the NodeMaintenance object")
		testNM = &maintenancev1.NodeMaintenance{
			ObjectMeta: metav1.ObjectMeta{
				// Cluster-scoped: no namespace field.
				Name: nmName,
			},
			Spec: maintenancev1.NodeMaintenanceSpec{
				NodeName:        nodeName,
				LinodeID:        12345,
				ScheduledAt:     scheduledAt,
				MaintenanceType: "reboot",
				WasSchedulable:  true,
			},
		}
		err = k8sClient.Get(ctx, nmKey, &maintenancev1.NodeMaintenance{})
		if errors.IsNotFound(err) {
			Expect(k8sClient.Create(ctx, testNM)).To(Succeed())
		}
	})

	AfterEach(func() {
		By("Deleting the NodeMaintenance object")
		nm := &maintenancev1.NodeMaintenance{}
		if err := k8sClient.Get(ctx, nmKey, nm); err == nil {
			Expect(k8sClient.Delete(ctx, nm)).To(Succeed())
		}

		By("Deleting the test Node")
		node := &corev1.Node{}
		if err := k8sClient.Get(ctx, nodeKey, node); err == nil {
			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
		}
	})

	Context("When the phase is Pending", func() {
		It("Should apply signals to the Node without error", func() {
			By("Setting the NodeMaintenance phase to Pending")
			nm := &maintenancev1.NodeMaintenance{}
			Expect(k8sClient.Get(ctx, nmKey, nm)).To(Succeed())
			nm.Status.Phase = maintenancev1.MaintenancePhasePending
			nm.Status.LastSyncTime = &metav1.Time{Time: time.Now()}
			Expect(k8sClient.Status().Update(ctx, nm)).To(Succeed())

			By("Running the reconciler")
			reconciler := &NodeMaintenanceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nmKey})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Node has the maintenance label")
			node := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, node)).To(Succeed())
			Expect(node.Labels).To(HaveKey(labelUpcomingMaintenance))
			Expect(node.Labels[labelUpcomingMaintenance]).To(Equal("true"))

			By("Verifying the Node has the maintenance taint")
			hasTaint := false
			for _, t := range node.Spec.Taints {
				if t.Key == taintKey && t.Effect == corev1.TaintEffectNoSchedule {
					hasTaint = true
					break
				}
			}
			Expect(hasTaint).To(BeTrue())

			By("Verifying the NodeMaintenance status flags are set")
			Expect(k8sClient.Get(ctx, nmKey, nm)).To(Succeed())
			Expect(nm.Status.NodeLabelApplied).To(BeTrue())
			Expect(nm.Status.NodeTaintApplied).To(BeTrue())
			Expect(nm.Status.NodeConditionApplied).To(BeTrue())
		})
	})

	Context("When the phase is Completed", func() {
		It("Should remove signals and delete the NodeMaintenance", func() {
			By("Applying signals first (simulating a prior Pending reconcile)")
			reconciler := &NodeMaintenanceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			nm := &maintenancev1.NodeMaintenance{}
			Expect(k8sClient.Get(ctx, nmKey, nm)).To(Succeed())
			nm.Status.Phase = maintenancev1.MaintenancePhasePending
			Expect(k8sClient.Status().Update(ctx, nm)).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nmKey})
			Expect(err).NotTo(HaveOccurred())

			By("Transitioning the NodeMaintenance to Completed")
			Expect(k8sClient.Get(ctx, nmKey, nm)).To(Succeed())
			now := metav1.Now()
			nm.Status.Phase = maintenancev1.MaintenancePhaseCompleted
			nm.Status.LastSyncTime = &now
			Expect(k8sClient.Status().Update(ctx, nm)).To(Succeed())

			By("Running the reconciler for the Completed phase")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nmKey})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Node no longer has the maintenance label")
			node := &corev1.Node{}
			Expect(k8sClient.Get(ctx, nodeKey, node)).To(Succeed())
			Expect(node.Labels).NotTo(HaveKey(labelUpcomingMaintenance))

			By("Verifying the Node no longer has the maintenance taint")
			for _, t := range node.Spec.Taints {
				Expect(t.Key).NotTo(Equal(taintKey))
			}

			By("Verifying the NodeMaintenance has been deleted")
			Eventually(func() bool {
				return errors.IsNotFound(k8sClient.Get(ctx, nmKey, &maintenancev1.NodeMaintenance{}))
			}, 5*time.Second, 100*time.Millisecond).Should(BeTrue())
		})
	})
})
