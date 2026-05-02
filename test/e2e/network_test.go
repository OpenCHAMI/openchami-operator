//go:build e2e
// +build e2e

/*
Copyright 2026 OpenCHAMI Authors.

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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/openchami/openchami-operator/test/utils"
)

// All package-level helpers and constants in this file are prefixed with
// "network" to avoid clashes with parallel sub-agents authoring
// lifecycle_test.go and observability_test.go.

const (
	// networkClusterTimeout is the upper bound for an OpenCHAMICluster to
	// progress through provisioning to a stable state in these tests.
	networkClusterTimeout = 5 * time.Minute

	// networkPollInterval is the polling cadence used by Eventually blocks.
	networkPollInterval = 5 * time.Second

	// networkProbeIntervalSeconds matches the spec.networkProbe.intervalSeconds
	// value applied by the test fixtures. Two intervals × this value bounds
	// how long a probe pod has to apply node labels.
	networkProbeIntervalSeconds = 10

	// networkLabelTimeout allows ~3× networkProbeIntervalSeconds plus buffer
	// for the probe to land a label on a kind node.
	networkLabelTimeout = 60 * time.Second
)

var _ = Describe("Network", Ordered, func() {
	Context("E2E-03: probe DaemonSet runs and labels nodes", func() {
		const clusterName = "netprobe03"

		BeforeAll(func() {
			networkApplyCluster(clusterName, networkProbeClusterYAML(clusterName, "10.0.0.0/24", "10.1.0.0/24"))
		})

		AfterAll(func() {
			networkDeleteCluster(clusterName)
		})

		It("should create the network-probe DaemonSet and have at least one pod ready", func() {
			ns := networkClusterNamespace(clusterName)
			ds := fmt.Sprintf("openchami-%s-network-probe", clusterName)

			By("waiting for the network-probe DaemonSet to exist")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "daemonset", ds, "-n", ns)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "network-probe DaemonSet should exist")
			}).WithTimeout(networkClusterTimeout).WithPolling(networkPollInterval).Should(Succeed())

			By("waiting for at least one network-probe pod to be ready")
			Eventually(func(g Gomega) {
				ready := networkDaemonSetNumberReady(g, ns, ds)
				g.Expect(ready).To(BeNumerically(">", 0),
					"network-probe DaemonSet should have numberReady > 0")
			}).WithTimeout(networkClusterTimeout).WithPolling(networkPollInterval).Should(Succeed())

			// TODO(probe-binary): The probe binary at cmd/probe/main.go is
			// currently a stub that exits 0; it does not perform real netlink
			// probing or apply node labels. Once the probe is real, re-enable
			// the label-presence assertion below (also see E2E-06).
			Skip("E2E-03: probe binary at cmd/probe/main.go is a stub; node-label probing pending")

			//nolint:govet // unreachable code is intentional until probe is implemented
			By("waiting for the probe to apply a provision-network-ready label to at least one node")
			label := networkNodeLabel(clusterName, "provision")
			Eventually(func(g Gomega) {
				ok := networkAnyNodeHasLabelKey(g, label)
				g.Expect(ok).To(BeTrue(),
					"expected at least one node to carry label %s after probe ran", label)
			}).WithTimeout(networkLabelTimeout).WithPolling(networkPollInterval).Should(Succeed())
		})
	})

	Context("E2E-04: CoreDHCP waits for provision-network-ready=true before scheduling", func() {
		const clusterName = "netdhcp04"

		var (
			labeledNode string
			labelKey    string
		)

		BeforeAll(func() {
			labelKey = networkNodeLabel(clusterName, "provision")
			yaml := networkProbeClusterYAML(clusterName, "10.0.0.0/24", "10.1.0.0/24")
			yaml = networkEnableCoreDHCP(yaml, "10.0.0.0/24", "10.0.0.100", "10.0.0.200")
			networkApplyCluster(clusterName, yaml)
		})

		AfterAll(func() {
			if labeledNode != "" && labelKey != "" {
				By("removing the manual provision-network-ready label from " + labeledNode)
				cmd := exec.Command("kubectl", "label", "node", labeledNode, labelKey+"-", "--overwrite")
				_, _ = utils.Run(cmd)
			}
			networkDeleteCluster(clusterName)
		})

		It("should reference the probe label in the CoreDHCP nodeSelector and only schedule once a node is labeled", func() {
			ns := networkClusterNamespace(clusterName)
			ds := "coredhcp"

			By("waiting for the coredhcp DaemonSet to exist")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "daemonset", ds, "-n", ns)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "coredhcp DaemonSet should exist")
			}).WithTimeout(networkClusterTimeout).WithPolling(networkPollInterval).Should(Succeed())

			By("verifying the coredhcp DaemonSet's nodeSelector references the probe label")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "daemonset", ds, "-n", ns,
					"-o", fmt.Sprintf("jsonpath={.spec.template.spec.nodeSelector['%s']}",
						strings.ReplaceAll(labelKey, ".", "\\.")))
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "kubectl get coredhcp DaemonSet failed")
				g.Expect(strings.TrimSpace(out)).To(Equal("true"),
					"coredhcp nodeSelector should require %s=true", labelKey)
			}).WithTimeout(networkClusterTimeout).WithPolling(networkPollInterval).Should(Succeed())

			By("verifying coredhcp has zero scheduled pods until a node is labeled")
			Consistently(func(g Gomega) {
				scheduled := networkDaemonSetCurrentNumberScheduled(g, ns, ds)
				g.Expect(scheduled).To(BeNumerically("==", 0),
					"coredhcp must not schedule before any node carries %s=true", labelKey)
			}).WithTimeout(20 * time.Second).WithPolling(networkPollInterval).Should(Succeed())

			By("manually labeling a node to simulate a successful provision-network probe")
			labeledNode = networkPickKindNode()
			cmd := exec.Command("kubectl", "label", "node", labeledNode, labelKey+"=true", "--overwrite")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "failed to label node %s", labeledNode)

			By("waiting for the coredhcp DaemonSet to schedule a pod on the labeled node")
			Eventually(func(g Gomega) {
				scheduled := networkDaemonSetCurrentNumberScheduled(g, ns, ds)
				ready := networkDaemonSetNumberReady(g, ns, ds)
				g.Expect(scheduled+ready).To(BeNumerically(">", 0),
					"coredhcp DaemonSet should schedule once a node carries %s=true", labelKey)
			}).WithTimeout(networkClusterTimeout).WithPolling(networkPollInterval).Should(Succeed())
		})
	})

	Context("E2E-05: Magellan waits for bmc-network-ready=true before scheduling", func() {
		const clusterName = "netmag05"

		var (
			labeledNode string
			labelKey    string
		)

		BeforeAll(func() {
			labelKey = networkNodeLabel(clusterName, "bmc")
			yaml := networkProbeClusterYAML(clusterName, "10.0.0.0/24", "10.1.0.0/24")
			yaml = networkEnableMagellan(yaml, "10.1.0.0/24")
			networkApplyCluster(clusterName, yaml)
		})

		AfterAll(func() {
			if labeledNode != "" && labelKey != "" {
				By("removing the manual bmc-network-ready label from " + labeledNode)
				cmd := exec.Command("kubectl", "label", "node", labeledNode, labelKey+"-", "--overwrite")
				_, _ = utils.Run(cmd)
			}
			networkDeleteCluster(clusterName)
		})

		It("should create an unsuspended Magellan CronJob whose podSelector references the BMC probe label", func() {
			ns := networkClusterNamespace(clusterName)
			cj := "magellan"

			By("waiting for the magellan CronJob to exist")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cronjob", cj, "-n", ns)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "magellan CronJob should exist")
			}).WithTimeout(networkClusterTimeout).WithPolling(networkPollInterval).Should(Succeed())

			By("verifying the magellan CronJob is not suspended")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cronjob", cj, "-n", ns,
					"-o", "jsonpath={.spec.suspend}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				// kubectl renders an unset bool as empty; treat both "" and "false" as not-suspended.
				v := strings.TrimSpace(out)
				g.Expect(v == "" || v == "false").To(BeTrue(),
					"magellan CronJob must not be suspended (got %q)", v)
			}).WithTimeout(networkClusterTimeout).WithPolling(networkPollInterval).Should(Succeed())

			By("verifying the magellan pod template's nodeSelector references the BMC probe label")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cronjob", cj, "-n", ns,
					"-o", fmt.Sprintf("jsonpath={.spec.jobTemplate.spec.template.spec.nodeSelector['%s']}",
						strings.ReplaceAll(labelKey, ".", "\\.")))
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "kubectl get magellan CronJob failed")
				g.Expect(strings.TrimSpace(out)).To(Equal("true"),
					"magellan nodeSelector should require %s=true", labelKey)
			}).WithTimeout(networkClusterTimeout).WithPolling(networkPollInterval).Should(Succeed())

			By("manually labeling a node to simulate a successful BMC-network probe")
			labeledNode = networkPickKindNode()
			cmd := exec.Command("kubectl", "label", "node", labeledNode, labelKey+"=true", "--overwrite")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "failed to label node %s", labeledNode)

			By("waiting for the magellan CronJob to remain healthy after the label is applied")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cronjob", cj, "-n", ns,
					"-o", "jsonpath={.metadata.name}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(Equal(cj),
					"magellan CronJob should still exist post-labeling")
			}).WithTimeout(networkClusterTimeout).WithPolling(networkPollInterval).Should(Succeed())
		})
	})

	Context("E2E-06: misconfigured subnet yields NetworkProbeReady=False with a Warning Event", func() {
		const clusterName = "netbad06"

		BeforeAll(func() {
			// TODO(probe-binary): The probe binary at cmd/probe/main.go is
			// currently a stub that exits 0; it does not validate subnets or
			// emit a per-node verdict. Once real probing is in place, drop
			// the Skip and let the test exercise the real failure path.
			Skip("E2E-06: probe binary at cmd/probe/main.go is a stub; subnet validation pending")

			//nolint:govet // unreachable until probe binary is implemented
			yaml := networkProbeClusterYAML(clusterName, "999.999.999.999/32", "10.1.0.0/24")
			networkApplyCluster(clusterName, yaml)
		})

		AfterAll(func() {
			networkDeleteCluster(clusterName)
		})

		It("should set ConditionNetworkProbeReady=False with reason NoEligibleNodes and emit a Warning Event", func() {
			ns := networkClusterNamespace(clusterName)

			By("waiting for ConditionNetworkProbeReady=False/NoEligibleNodes")
			Eventually(func(g Gomega) {
				status, reason := networkPollCondition(g, clusterName, "NetworkProbeReady")
				g.Expect(status).To(Equal("False"),
					"NetworkProbeReady should be False given an invalid subnet")
				g.Expect(reason).To(Equal("NoEligibleNodes"),
					"NetworkProbeReady reason should be NoEligibleNodes")
			}).WithTimeout(networkClusterTimeout).WithPolling(networkPollInterval).Should(Succeed())

			By("verifying a Warning Event with reason NoEligibleNodes was recorded")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "events", "-n", ns,
					"--field-selector=reason=NoEligibleNodes,type=Warning",
					"-o", "jsonpath={.items[*].metadata.name}")
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "kubectl get events failed")
				g.Expect(strings.TrimSpace(out)).NotTo(BeEmpty(),
					"expected at least one Warning Event with reason NoEligibleNodes")
			}).WithTimeout(networkClusterTimeout).WithPolling(networkPollInterval).Should(Succeed())
		})
	})
})

// networkClusterNamespace returns the per-cluster namespace convention
// "openchami-{clusterName}".
func networkClusterNamespace(clusterName string) string {
	return "openchami-" + clusterName
}

// networkNodeLabel returns the canonical label key the probe binary writes
// onto each node for a given probe type ("provision" or "bmc").
func networkNodeLabel(clusterName, probeType string) string {
	return fmt.Sprintf("openchami.org/%s/%s-network-ready", clusterName, probeType)
}

// networkApplyCluster writes the given YAML to a temp file and kubectl applies
// it. Test cluster CRs always live in the default namespace; the operator
// provisions per-cluster namespaces named openchami-{clusterName}.
func networkApplyCluster(clusterName, yaml string) {
	GinkgoHelper()
	By("applying OpenCHAMICluster " + clusterName)
	path := filepath.Join(GinkgoT().TempDir(), "network-"+clusterName+".yaml")
	Expect(os.WriteFile(path, []byte(yaml), 0o644)).To(Succeed(),
		"failed to write fixture for cluster %s", clusterName)
	cmd := exec.Command("kubectl", "apply", "-f", path)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "failed to apply cluster %s", clusterName)
}

// networkDeleteCluster deletes the cluster CR and tolerates already-gone
// resources so cleanup is idempotent across test ordering.
func networkDeleteCluster(clusterName string) {
	GinkgoHelper()
	By("deleting OpenCHAMICluster " + clusterName)
	cmd := exec.Command("kubectl", "delete", "openchamicluster", clusterName,
		"--ignore-not-found=true", "--wait=false")
	_, _ = utils.Run(cmd)
}

// networkPollCondition extracts the (status, reason) of a status.condition
// of the given type from an OpenCHAMICluster.
func networkPollCondition(g Gomega, clusterName, conditionType string) (string, string) {
	statusCmd := exec.Command("kubectl", "get", "openchamicluster", clusterName,
		"-o", fmt.Sprintf("jsonpath={.status.conditions[?(@.type==\"%s\")].status}", conditionType))
	statusOut, err := utils.Run(statusCmd)
	g.Expect(err).NotTo(HaveOccurred(), "kubectl get cluster status failed")

	reasonCmd := exec.Command("kubectl", "get", "openchamicluster", clusterName,
		"-o", fmt.Sprintf("jsonpath={.status.conditions[?(@.type==\"%s\")].reason}", conditionType))
	reasonOut, err := utils.Run(reasonCmd)
	g.Expect(err).NotTo(HaveOccurred(), "kubectl get cluster reason failed")

	return strings.TrimSpace(statusOut), strings.TrimSpace(reasonOut)
}

// networkDaemonSetNumberReady reads .status.numberReady off a DaemonSet.
func networkDaemonSetNumberReady(g Gomega, namespace, name string) int {
	cmd := exec.Command("kubectl", "get", "daemonset", name, "-n", namespace,
		"-o", "jsonpath={.status.numberReady}")
	out, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "kubectl get daemonset numberReady failed")
	return networkAtoi(g, out)
}

// networkDaemonSetCurrentNumberScheduled reads .status.currentNumberScheduled
// off a DaemonSet.
func networkDaemonSetCurrentNumberScheduled(g Gomega, namespace, name string) int {
	cmd := exec.Command("kubectl", "get", "daemonset", name, "-n", namespace,
		"-o", "jsonpath={.status.currentNumberScheduled}")
	out, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "kubectl get daemonset currentNumberScheduled failed")
	return networkAtoi(g, out)
}

// networkAtoi parses a (possibly empty) integer string from kubectl output;
// empty inputs are treated as 0 because jsonpath emits "" for unset fields.
func networkAtoi(g Gomega, s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	g.Expect(err).NotTo(HaveOccurred(), "expected integer, got %q", s)
	return n
}

// networkAnyNodeHasLabelKey returns true if at least one node currently
// carries the given label key (regardless of value).
func networkAnyNodeHasLabelKey(g Gomega, labelKey string) bool {
	cmd := exec.Command("kubectl", "get", "nodes",
		"-o", fmt.Sprintf("jsonpath={range .items[*]}{.metadata.labels['%s']}{\"\\n\"}{end}",
			strings.ReplaceAll(labelKey, ".", "\\.")))
	out, err := utils.Run(cmd)
	g.Expect(err).NotTo(HaveOccurred(), "kubectl get nodes failed")
	for _, line := range utils.GetNonEmptyLines(out) {
		if strings.TrimSpace(line) != "" {
			return true
		}
	}
	return false
}

// networkPickKindNode returns the name of the first node in the kind cluster.
// Used to manually mark a node ready when the probe binary is a stub.
func networkPickKindNode() string {
	GinkgoHelper()
	cmd := exec.Command("kubectl", "get", "nodes", "-o", "jsonpath={.items[0].metadata.name}")
	out, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "failed to list kind nodes")
	name := strings.TrimSpace(out)
	Expect(name).NotTo(BeEmpty(), "kind cluster has no nodes")
	return name
}

// networkProbeClusterYAML renders an OpenCHAMICluster manifest with network
// probing enabled and only the always-on services (SMD, tokensmith, boot,
// metadata) configured. CoreDHCP and Magellan are disabled by default; tests
// that need them call networkEnableCoreDHCP / networkEnableMagellan to splice
// in the relevant blocks.
func networkProbeClusterYAML(clusterName, provisionSubnet, bmcSubnet string) string {
	return fmt.Sprintf(`apiVersion: openchami.org/v1alpha1
kind: OpenCHAMICluster
metadata:
  name: %[1]s
  namespace: default
spec:
  clusterName: %[1]s
  domain: %[1]s.local
  platform:
    vault:
      address: "http://localhost:8200"
      authMethod: appRole
      appRoleSecretRef:
        name: %[1]s-approle
    objectStorage:
      endpoint: "http://localhost:4566"
      bucket: %[1]s-boot-images
      tlsInsecure: true
  networkProbe:
    enabled: true
    intervalSeconds: %[4]d
    provisionNetwork:
      subnet: %[2]s
    bmcNetwork:
      subnet: %[3]s
  services:
    smd:
      enabled: true
      replicas: 1
    tokensmith:
      enabled: true
      oidcProvider: vault
    bootService:
      enabled: true
      replicas: 1
    metadataService:
      enabled: true
      replicas: 1
    coreDHCP:
      enabled: false
    magellan:
      enabled: false
  networking:
    gatewayClass: envoy
    tls:
      issuer: selfsigned-dev
  database:
    instances: 1
    storageSize: 5Gi
  logging:
    enabled: false
  operatorChannel: stable
`, clusterName, provisionSubnet, bmcSubnet, networkProbeIntervalSeconds)
}

// networkEnableCoreDHCP rewrites the disabled coreDHCP block produced by
// networkProbeClusterYAML into an enabled one with a single lease range.
// The test relies on the operator generating the nodeSelector from probe
// labels (since networkProbe.enabled=true), so no nodeSelector is supplied.
func networkEnableCoreDHCP(yaml, leaseSubnet, leaseStart, leaseEnd string) string {
	block := fmt.Sprintf(`    coreDHCP:
      enabled: true
      leaseRanges:
        - subnet: %s
          start: %s
          end: %s
`, leaseSubnet, leaseStart, leaseEnd)
	return strings.Replace(yaml,
		"    coreDHCP:\n      enabled: false\n",
		block, 1)
}

// networkEnableMagellan rewrites the disabled magellan block produced by
// networkProbeClusterYAML into an enabled one with the supplied BMC subnet.
// nodeSelector is intentionally omitted; the operator generates it from probe
// labels.
func networkEnableMagellan(yaml, bmcSubnet string) string {
	block := fmt.Sprintf(`    magellan:
      enabled: true
      bmcSubnet: %s
      schedule: "*/30 * * * *"
      concurrencyPolicy: Forbid
`, bmcSubnet)
	return strings.Replace(yaml,
		"    magellan:\n      enabled: false\n",
		block, 1)
}
