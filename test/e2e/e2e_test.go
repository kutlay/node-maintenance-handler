//go:build e2e
// +build e2e

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

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"linode.com/node-maintenance-controller/test/utils"
)

// namespace where the project is deployed in
const namespace = "node-maintenance-controller-system"

// serviceAccountName created for the project
const serviceAccountName = "node-maintenance-controller-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "node-maintenance-controller-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "node-maintenance-controller-metrics-binding"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("creating the linode-token Secret so the poller can start without error")
		Expect(applyYAML(fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: linode-token
  namespace: %s
type: Opaque
stringData:
  token: fake-token-e2e-test`, namespace))).To(Succeed(), "Failed to create linode-token Secret")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace,
			"--ignore-not-found=true")
		_, _ = utils.Run(cmd)

		By("cleaning up the metrics ClusterRoleBinding")
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName,
			"--ignore-not-found=true")
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				By("getting the name of the controller-manager pod")
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				By("validating the pod's status")
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			Expect(applyYAML(fmt.Sprintf(`apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: %s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: node-maintenance-controller-metrics-reader
subjects:
- kind: ServiceAccount
  name: %s
  namespace: %s`, metricsRoleBindingName, serviceAccountName, namespace))).
				To(Succeed(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd := exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": [
								"for i in $(seq 1 30); do curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1"
							],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})

	// -------------------------------------------------------------------------
	// NodeMaintenance reconciler e2e tests
	//
	// These tests bypass the Linode poller entirely.  They directly create and
	// patch NodeMaintenance objects (as the poller would) and verify that the
	// reconciler correctly applies and removes maintenance signals (label, taint,
	// Node condition) on the target Node.
	//
	// Fake Nodes are used so the Kind control-plane node is never disrupted.
	// -------------------------------------------------------------------------
	Context("NodeMaintenance reconciler", Ordered, func() {
		// Fixed names used by the per-test node+NM pair.  A fresh node is
		// created in each It block and deleted in AfterEach.
		const (
			testNodeName    = "e2e-test-node"
			testNMName      = "e2e-test-node" // NM name == Node name (1:1 mapping)
			gcNodeName      = "e2e-gc-test-node"
			gcNMName        = "e2e-gc-test-node"
			fakeScheduledAt = "2026-12-01T00:00:00Z"
			linodeID        = 12345
		)

		AfterEach(func() {
			By("cleaning up test NodeMaintenance objects")
			for _, nm := range []string{testNMName, gcNMName} {
				cmd := exec.Command("kubectl", "delete", "nodemaintenance", nm,
					"--ignore-not-found=true", "--timeout=20s")
				_, _ = utils.Run(cmd)
			}

			By("cleaning up test Node objects")
			for _, node := range []string{testNodeName, gcNodeName} {
				cmd := exec.Command("kubectl", "delete", "node", node,
					"--ignore-not-found=true", "--timeout=20s")
				_, _ = utils.Run(cmd)
			}
		})

		It("should apply maintenance label, taint, and condition to a Node when Pending", func() {
			By("creating a fake test node")
			Expect(createFakeNode(testNodeName)).To(Succeed())

			By("creating a NodeMaintenance object for the test node")
			Expect(createNodeMaintenance(testNMName, testNodeName, linodeID, fakeScheduledAt, true)).
				To(Succeed())

			// The reconciler processes the NM immediately on creation (empty
			// phase → reconcileActive).  Setting Pending here also acts as a
			// heartbeat that re-triggers the reconciler.
			By("setting the NodeMaintenance phase to Pending")
			Expect(setNMPhase(testNMName, "Pending")).To(Succeed())

			By("waiting for the maintenance label to appear on the node")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "node", testNodeName, "-o",
					`jsonpath={.metadata.labels.maintenance\.linode\.com/upcoming-maintenance}`)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal("true"))
			}).Should(Succeed())

			By("verifying the NoSchedule taint is applied")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "node", testNodeName, "-o",
					`jsonpath={.spec.taints[?(@.key=='maintenance.linode.com/upcoming-maintenance')].effect}`)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal("NoSchedule"))
			}).Should(Succeed())

			By("verifying the UpcomingMaintenance node condition is set to True")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "node", testNodeName, "-o",
					`jsonpath={.status.conditions[?(@.type=='UpcomingMaintenance')].status}`)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal("True"))
			}).Should(Succeed())

			By("verifying the NodeMaintenance status flags are set")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "nodemaintenance", testNMName, "-o",
					`jsonpath={.status.nodeLabelApplied},{.status.nodeTaintApplied},{.status.nodeConditionApplied}`)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal("true,true,true"))
			}).Should(Succeed())
		})

		It("should remove maintenance signals and delete the NodeMaintenance when Completed", func() {
			By("creating a fake test node")
			Expect(createFakeNode(testNodeName)).To(Succeed())

			By("creating a NodeMaintenance and waiting for Pending signals to be applied")
			Expect(createNodeMaintenance(testNMName, testNodeName, linodeID, fakeScheduledAt, true)).
				To(Succeed())
			Expect(setNMPhase(testNMName, "Pending")).To(Succeed())

			By("waiting for the maintenance label to appear (signals applied)")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "node", testNodeName, "-o",
					`jsonpath={.metadata.labels.maintenance\.linode\.com/upcoming-maintenance}`)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal("true"))
			}).Should(Succeed())

			By("transitioning the NodeMaintenance to Completed")
			Expect(setNMPhase(testNMName, "Completed")).To(Succeed())

			By("waiting for the maintenance label to be removed from the node")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "node", testNodeName, "-o",
					`jsonpath={.metadata.labels.maintenance\.linode\.com/upcoming-maintenance}`)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(BeEmpty())
			}).Should(Succeed())

			By("verifying the NoSchedule taint is removed from the node")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "node", testNodeName, "-o",
					`jsonpath={.spec.taints[?(@.key=='maintenance.linode.com/upcoming-maintenance')].effect}`)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(BeEmpty())
			}).Should(Succeed())

			By("verifying the UpcomingMaintenance condition is removed from the node")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "node", testNodeName, "-o",
					`jsonpath={.status.conditions[?(@.type=='UpcomingMaintenance')].status}`)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(BeEmpty())
			}).Should(Succeed())

			By("verifying the NodeMaintenance object is deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "nodemaintenance", testNMName,
					"-o", "jsonpath={.metadata.name}", "--ignore-not-found=true")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(BeEmpty())
			}).Should(Succeed())
		})

		It("should uncordon a node on Completed when WasSchedulable is true", func() {
			By("creating a fake test node (starts schedulable)")
			Expect(createFakeNode(testNodeName)).To(Succeed())

			By("creating a NodeMaintenance with wasSchedulable=true and setting Pending")
			Expect(createNodeMaintenance(testNMName, testNodeName, linodeID, fakeScheduledAt, true)).
				To(Succeed())
			Expect(setNMPhase(testNMName, "Pending")).To(Succeed())

			By("waiting for signals to be applied")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "nodemaintenance", testNMName, "-o",
					"jsonpath={.status.nodeLabelApplied}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal("true"))
			}).Should(Succeed())

			By("cordoning the test node (simulating what Draino would do)")
			cmd := exec.Command("kubectl", "cordon", testNodeName)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the node is now unschedulable")
			cmd = exec.Command("kubectl", "get", "node", testNodeName, "-o", "jsonpath={.spec.unschedulable}")
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(out)).To(Equal("true"))

			By("transitioning the NodeMaintenance to Completed")
			Expect(setNMPhase(testNMName, "Completed")).To(Succeed())

			By("waiting for the node to be uncordoned by the controller")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "node", testNodeName,
					"-o", "jsonpath={.spec.unschedulable}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// When a node is schedulable, spec.unschedulable is absent (empty string).
				g.Expect(strings.TrimSpace(out)).To(BeEmpty())
			}).Should(Succeed())

			By("verifying the NodeMaintenance is deleted after completion")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "nodemaintenance", testNMName,
					"-o", "jsonpath={.metadata.name}", "--ignore-not-found=true")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(BeEmpty())
			}).Should(Succeed())
		})

		It("should not uncordon a node on Completed when WasSchedulable is false", func() {
			By("creating a fake test node")
			Expect(createFakeNode(testNodeName)).To(Succeed())

			By("cordoning the test node before maintenance (it was already unschedulable)")
			cmd := exec.Command("kubectl", "cordon", testNodeName)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating a NodeMaintenance with wasSchedulable=false")
			Expect(createNodeMaintenance(testNMName, testNodeName, linodeID, fakeScheduledAt, false)).
				To(Succeed())
			Expect(setNMPhase(testNMName, "Pending")).To(Succeed())

			By("waiting for signals to be applied")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "nodemaintenance", testNMName, "-o",
					"jsonpath={.status.nodeLabelApplied}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal("true"))
			}).Should(Succeed())

			By("transitioning to Completed")
			Expect(setNMPhase(testNMName, "Completed")).To(Succeed())

			By("waiting for the NodeMaintenance to be deleted")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "nodemaintenance", testNMName,
					"-o", "jsonpath={.metadata.name}", "--ignore-not-found=true")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(BeEmpty())
			}).Should(Succeed())

			By("verifying the node remains unschedulable (was not uncordoned)")
			cmd = exec.Command("kubectl", "get", "node", testNodeName,
				"-o", "jsonpath={.spec.unschedulable}")
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(out)).To(Equal("true"))
		})

		It("should garbage-collect NodeMaintenance via ownerReference when the Node is deleted", func() {
			By("creating a fake node for the GC test")
			Expect(createFakeNode(gcNodeName)).To(Succeed())

			By("retrieving the fake node's UID")
			cmd := exec.Command("kubectl", "get", "node", gcNodeName, "-o", "jsonpath={.metadata.uid}")
			uid, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			uid = strings.TrimSpace(uid)
			Expect(uid).NotTo(BeEmpty(), "node UID should not be empty")

			By("creating a NodeMaintenance with an ownerReference pointing to the fake node")
			Expect(createNodeMaintenanceWithOwnerRef(gcNMName, gcNodeName, uid)).To(Succeed())

			By("verifying the NodeMaintenance exists")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "nodemaintenance", gcNMName,
					"-o", "jsonpath={.metadata.name}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal(gcNMName))
			}).Should(Succeed())

			By("deleting the fake node")
			cmd = exec.Command("kubectl", "delete", "node", gcNodeName, "--wait=true", "--timeout=30s")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the NodeMaintenance to be garbage-collected")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "nodemaintenance", gcNMName,
					"-o", "jsonpath={.metadata.name}", "--ignore-not-found=true")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(BeEmpty())
			}, 3*time.Minute, time.Second).Should(Succeed())
		})
	})
})

// ---------------------------------------------------------------------------
// Helper functions
// ---------------------------------------------------------------------------

// applyYAML writes manifest to a temp file and runs "kubectl apply -f".
func applyYAML(manifest string) error {
	tmp, err := os.CreateTemp("", "e2e-*.yaml")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.WriteString(manifest); err != nil {
		return fmt.Errorf("writing manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}

	cmd := exec.Command("kubectl", "apply", "-f", tmp.Name())
	_, err = utils.Run(cmd)
	return err
}

// createFakeNode creates a minimal Node object in the cluster.
// It has no kubelet backing it; it exists purely as an API object for tests.
func createFakeNode(name string) error {
	return applyYAML(fmt.Sprintf(`apiVersion: v1
kind: Node
metadata:
  name: %s
spec:
  providerID: linode://99999`, name))
}

// createNodeMaintenance creates a NodeMaintenance CR via kubectl apply.
// Status (including phase) must be set separately via setNMPhase.
func createNodeMaintenance(name, nodeName string, linodeID int64, scheduledAt string, wasSchedulable bool) error {
	return applyYAML(fmt.Sprintf(`apiVersion: maintenance.linode.com/v1
kind: NodeMaintenance
metadata:
  name: %s
spec:
  nodeName: %s
  linodeID: %d
  scheduledAt: "%s"
  maintenanceType: reboot
  wasSchedulable: %t`, name, nodeName, linodeID, scheduledAt, wasSchedulable))
}

// createNodeMaintenanceWithOwnerRef creates a NodeMaintenance CR with an ownerReference
// pointing to the named Node.  When the Node is deleted, Kubernetes GC deletes this NM.
func createNodeMaintenanceWithOwnerRef(name, nodeName, uid string) error {
	return applyYAML(fmt.Sprintf(`apiVersion: maintenance.linode.com/v1
kind: NodeMaintenance
metadata:
  name: %s
  ownerReferences:
  - apiVersion: v1
    kind: Node
    name: %s
    uid: %s
spec:
  nodeName: %s
  linodeID: 99999
  scheduledAt: "2026-12-01T00:00:00Z"
  maintenanceType: reboot
  wasSchedulable: true`, name, nodeName, uid, nodeName))
}

// setNMPhase patches the status.phase (and lastSyncTime) of a NodeMaintenance
// via the status subresource.
func setNMPhase(name, phase string) error {
	patch := fmt.Sprintf(`{"status":{"phase":%q,"lastSyncTime":%q}}`,
		phase, time.Now().UTC().Format(time.RFC3339))
	cmd := exec.Command("kubectl", "patch", "nodemaintenance", name,
		"--subresource=status", "--type=merge", "-p", patch)
	_, err := utils.Run(cmd)
	return err
}

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	By("creating temporary file to store the token request")
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		By("executing kubectl command to create the token")
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		By("parsing the JSON output to extract the token")
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
