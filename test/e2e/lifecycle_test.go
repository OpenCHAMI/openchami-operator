//go:build e2e
// +build e2e

// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// lifecycleDevVaultAddr resolves the dev Vault address, honouring the
// E2E_VAULT_ADDR env var so non-kind dev loops can point at their own
// Vault without editing source.
func lifecycleDevVaultAddr() string {
	if v := os.Getenv(lifecycleVaultAddrEnvVar); v != "" {
		return v
	}
	return lifecycleDevVaultAddrDefault
}

// Lifecycle test constants and helpers.
//
// All package-level identifiers in this file are prefixed with "lifecycle"
// to avoid clashing with helpers introduced by sibling sub-agents working
// on network_test.go and observability_test.go in the same package.

const (
	// lifecycleClusterTimeout is the upper bound on cluster Ready transitions.
	lifecycleClusterTimeout = 5 * time.Minute

	// lifecycleClusterPolling is the polling interval for cluster state checks.
	lifecycleClusterPolling = 5 * time.Second

	// lifecycleNamespaceTimeout is the upper bound on namespace teardown
	// after the parent OpenCHAMICluster is deleted.
	lifecycleNamespaceTimeout = 3 * time.Minute

	// lifecycleConditionTimeout is the upper bound on condition transitions
	// that don't require full cluster Ready (e.g. VaultConfigured=False).
	lifecycleConditionTimeout = 2 * time.Minute

	// lifecycleDevVaultAddrDefault is the in-cluster Vault address provided
	// by `make dev-up` in the kind environment. The active address is
	// resolved by lifecycleResolveDevVaultAddr() — set the
	// E2E_VAULT_ADDR env var to override (e.g. for non-kind dev loops).
	lifecycleDevVaultAddrDefault = "https://vault.vault-system.svc.cluster.local:8200"
	lifecycleVaultAddrEnvVar     = "E2E_VAULT_ADDR"

	// lifecycleBadVaultAddr is a deliberately unreachable Vault endpoint
	// used to drive ConditionVaultConfigured=False / Reason=Unreachable.
	lifecycleBadVaultAddr = "https://vault-does-not-exist.invalid:8200"

	// lifecycleConditionTrue / lifecycleConditionFalse are the metav1.Condition
	// status string literals; reused across helpers to keep golangci-lint
	// (goconst) happy.
	lifecycleConditionTrue  = "True"
	lifecycleConditionFalse = "False"
)

// lifecycleClusterYAML returns a minimal valid OpenCHAMICluster manifest.
// dhcpDiscriminator is appended to the CoreDHCP nodeSelector to force a
// distinct selector per cluster (so two clusters don't collide on the
// "DHCP node exclusivity" invariant).
func lifecycleClusterYAML(name, vaultAddr, dhcpDiscriminator, operatorChannel, pinnedVersion string) string {
	pinnedLine := ""
	if pinnedVersion != "" {
		pinnedLine = fmt.Sprintf("\n  pinnedVersion: %q", pinnedVersion)
	}
	if operatorChannel == "" {
		operatorChannel = "stable"
	}
	return fmt.Sprintf(`apiVersion: openchami.openchami.org/v1alpha1
kind: OpenCHAMICluster
metadata:
  name: %[1]s
spec:
  clusterName: %[1]s
  domain: %[1]s.local
  platform:
    vault:
      address: %[2]q
      authMethod: appRole
      appRoleSecretRef:
        name: %[1]s-approle
    objectStorage:
      endpoint: "http://localhost:4566"
      bucket: %[1]s-boot-images
      tlsInsecure: true
  networkProbe:
    enabled: false
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
      enabled: true
      nodeSelector:
        openchami.org/dhcp: %[3]q
      leaseRanges:
        - subnet: 192.168.100.0/24
          start: 192.168.100.100
          end: 192.168.100.200
    magellan:
      enabled: true
      bmcSubnet: 192.168.200.0/24
      nodeSelector:
        openchami.org/bmc: %[3]q
      schedule: "0 * * * *"
  networking:
    gatewayClass: envoy
    tls:
      issuer: selfsigned-dev
  database:
    instances: 1
    storageSize: 5Gi
  logging:
    enabled: true
    logBucket: %[1]s-logs
    retentionDays: 7
    flushIntervalSeconds: 30
  operatorChannel: %[4]s%[5]s
`, name, vaultAddr, dhcpDiscriminator, operatorChannel, pinnedLine)
}

// lifecycleApplyCluster pipes the given cluster YAML through `kubectl apply -f -`.
func lifecycleApplyCluster(name, vaultAddr, dhcpDiscriminator string) {
	yaml := lifecycleClusterYAML(name, vaultAddr, dhcpDiscriminator, "stable", "")
	lifecycleApplyYAML(yaml)
}

// lifecycleApplyClusterWithChannel applies a cluster with a non-default
// operator channel and optional pinned version (used by E2E-09).
func lifecycleApplyClusterWithChannel(name, vaultAddr, dhcpDiscriminator, channel, pinnedVersion string) {
	yaml := lifecycleClusterYAML(name, vaultAddr, dhcpDiscriminator, channel, pinnedVersion)
	lifecycleApplyYAML(yaml)
}

// lifecycleApplyYAML shells out to `kubectl apply -f -` with the given
// manifest on stdin. Fails the calling spec on non-zero exit.
func lifecycleApplyYAML(yaml string) {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yaml)
	output, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "kubectl apply failed: %s", string(output))
}

// lifecycleApplyYAMLExpectError applies a manifest expected to be REJECTED
// by the validating webhook. Returns the combined kubectl output for
// substring matching by the caller.
func lifecycleApplyYAMLExpectError(yaml string) (string, error) {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yaml)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// lifecycleDeleteCluster removes the cluster CR. Errors during delete are
// reported but not fatal (the spec is responsible for asserting teardown).
func lifecycleDeleteCluster(name string) {
	cmd := exec.Command("kubectl", "delete", "openchamicluster", name, "--ignore-not-found", "--wait=false")
	output, err := cmd.CombinedOutput()
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl delete openchamicluster %s: %s\n", name, string(output))
	}
}

// lifecyclePatchVaultAddress patches the cluster's spec.platform.vault.address
// using a strategic merge patch.
func lifecyclePatchVaultAddress(name, addr string) {
	patch := fmt.Sprintf(`{"spec":{"platform":{"vault":{"address":%q}}}}`, addr)
	cmd := exec.Command("kubectl", "patch", "openchamicluster", name, "--type=merge", "-p", patch)
	output, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "kubectl patch failed: %s", string(output))
}

// lifecycleClearVersionPin patches the cluster back to the stable channel
// and clears any PinnedVersion (E2E-09 recovery).
func lifecycleClearVersionPin(name string) {
	patch := `{"spec":{"operatorChannel":"stable","pinnedVersion":null}}`
	cmd := exec.Command("kubectl", "patch", "openchamicluster", name, "--type=merge", "-p", patch)
	output, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "kubectl patch failed: %s", string(output))
}

// lifecycleConditionStatus reads the `.status.conditions[?(@.type==<condType>)].status`
// jsonpath. Returns "" when the cluster or condition is missing.
func lifecycleConditionStatus(name, condType string) string {
	jsonpath := fmt.Sprintf(`jsonpath={.status.conditions[?(@.type==%q)].status}`, condType)
	cmd := exec.Command("kubectl", "get", "openchamicluster", name, "-o", jsonpath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// lifecycleConditionReason reads the `.status.conditions[?(@.type==<condType>)].reason`
// jsonpath. Returns "" when the cluster or condition is missing.
func lifecycleConditionReason(name, condType string) string {
	jsonpath := fmt.Sprintf(`jsonpath={.status.conditions[?(@.type==%q)].reason}`, condType)
	cmd := exec.Command("kubectl", "get", "openchamicluster", name, "-o", jsonpath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

// lifecycleClusterReady returns true when the Ready condition is "True".
func lifecycleClusterReady(name string) bool {
	return lifecycleConditionStatus(name, "Ready") == lifecycleConditionTrue
}

// lifecycleNamespaceGone returns true when the per-cluster namespace
// (openchami-<name>) no longer exists.
func lifecycleNamespaceGone(name string) bool {
	ns := "openchami-" + name
	cmd := exec.Command("kubectl", "get", "namespace", ns, "--ignore-not-found", "-o", "name")
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Treat get errors as "still around"; the spec will keep polling.
		return false
	}
	return strings.TrimSpace(string(output)) == ""
}

var _ = Describe("Lifecycle", Ordered, func() {
	// Each context cleans up its own cluster CRs in AfterEach so that
	// failures in one don't leave residue for later contexts.
	Context("E2E-01 single cluster lifecycle", func() {
		const name = "venado"

		AfterEach(func() {
			lifecycleDeleteCluster(name)
		})

		It("deploys, reaches Ready, and cleans up its namespace on delete", func() {
			By("applying the venado cluster")
			lifecycleApplyCluster(name, lifecycleDevVaultAddr(), name)

			By("waiting for Ready=True")
			Eventually(func() bool {
				return lifecycleClusterReady(name)
			}).WithTimeout(lifecycleClusterTimeout).
				WithPolling(lifecycleClusterPolling).
				Should(BeTrue(), "cluster %s never reached Ready=True", name)

			By("deleting the cluster")
			lifecycleDeleteCluster(name)

			By("waiting for namespace openchami-venado to be removed")
			Eventually(func() bool {
				return lifecycleNamespaceGone(name)
			}).WithTimeout(lifecycleNamespaceTimeout).
				WithPolling(lifecycleClusterPolling).
				Should(BeTrue(), "namespace openchami-%s still present after delete", name)
		})
	})

	Context("E2E-02 dual cluster independence", func() {
		const (
			nameA = "venado"
			nameB = "frontier"
		)

		AfterEach(func() {
			lifecycleDeleteCluster(nameA)
			lifecycleDeleteCluster(nameB)
		})

		It("brings two distinct clusters to Ready independently", func() {
			By("applying both clusters with disjoint DHCP nodeSelectors")
			lifecycleApplyCluster(nameA, lifecycleDevVaultAddr(), nameA)
			lifecycleApplyCluster(nameB, lifecycleDevVaultAddr(), nameB)

			By("waiting for venado Ready=True")
			Eventually(func() bool {
				return lifecycleClusterReady(nameA)
			}).WithTimeout(lifecycleClusterTimeout).
				WithPolling(lifecycleClusterPolling).
				Should(BeTrue(), "cluster %s never reached Ready=True", nameA)

			By("waiting for frontier Ready=True")
			Eventually(func() bool {
				return lifecycleClusterReady(nameB)
			}).WithTimeout(lifecycleClusterTimeout).
				WithPolling(lifecycleClusterPolling).
				Should(BeTrue(), "cluster %s never reached Ready=True", nameB)
		})
	})

	Context("E2E-07 DHCP nodeSelector conflict rejected by webhook", func() {
		const (
			nameA      = "venado"
			nameB      = "frontier"
			sharedDisc = "shared-dhcp-host"
		)

		AfterEach(func() {
			lifecycleDeleteCluster(nameA)
			lifecycleDeleteCluster(nameB)
		})

		It("rejects a second cluster that targets the same DHCP node", func() {
			By("applying the first cluster with a fixed DHCP nodeSelector")
			lifecycleApplyCluster(nameA, lifecycleDevVaultAddr(), sharedDisc)

			By("attempting to apply a second cluster with the same selector")
			yaml := lifecycleClusterYAML(nameB, lifecycleDevVaultAddr(), sharedDisc, "stable", "")
			output, err := lifecycleApplyYAMLExpectError(yaml)
			Expect(err).To(HaveOccurred(),
				"webhook should reject conflicting DHCP nodeSelector; got success: %s", output)

			lower := strings.ToLower(output)
			Expect(
				strings.Contains(lower, "duplicate") ||
					strings.Contains(lower, "nodeselector") ||
					strings.Contains(lower, "dhcp"),
			).To(BeTrue(),
				"webhook rejection message should mention the conflict; got: %s", output)
		})
	})

	Context("E2E-08 Vault unreachable then recovers", func() {
		const name = "venado"

		AfterEach(func() {
			lifecycleDeleteCluster(name)
		})

		It("reports VaultConfigured=False then recovers after the address is fixed", func() {
			By("applying a cluster with an unreachable Vault address")
			lifecycleApplyCluster(name, lifecycleBadVaultAddr, name)

			By("waiting for VaultConfigured=False with Reason=Unreachable")
			Eventually(func() bool {
				return lifecycleConditionStatus(name, "VaultConfigured") == lifecycleConditionFalse &&
					lifecycleConditionReason(name, "VaultConfigured") == "Unreachable"
			}).WithTimeout(lifecycleConditionTimeout).
				WithPolling(lifecycleClusterPolling).
				Should(BeTrue(), "VaultConfigured did not transition to False/Unreachable")

			// The recovery half requires a reachable Vault. The default is
			// the kind dev environment provisioned by `make dev-up`; set
			// E2E_VAULT_ADDR to override for non-kind dev loops.
			By("patching to a reachable Vault address")
			lifecyclePatchVaultAddress(name, lifecycleDevVaultAddr())

			By("waiting for VaultConfigured=True")
			Eventually(func() bool {
				return lifecycleConditionStatus(name, "VaultConfigured") == lifecycleConditionTrue
			}).WithTimeout(lifecycleClusterTimeout).
				WithPolling(lifecycleClusterPolling).
				Should(BeTrue(), "VaultConfigured did not recover to True after address fix")
		})
	})

	Context("E2E-09 version pin gating", func() {
		const (
			name      = "venado"
			badPinned = "0.0.0-mismatch"
		)

		AfterEach(func() {
			lifecycleDeleteCluster(name)
		})

		It("disables reconcile when pinned to a mismatched version, then resumes when unpinned", func() {
			By("applying a cluster pinned to a non-matching operator version")
			lifecycleApplyClusterWithChannel(name, lifecycleDevVaultAddr(), name, "pinned", badPinned)

			By("waiting for ReconcileActive=False with Reason=VersionPinned")
			Eventually(func() bool {
				return lifecycleConditionStatus(name, "ReconcileActive") == lifecycleConditionFalse &&
					lifecycleConditionReason(name, "ReconcileActive") == "VersionPinned"
			}).WithTimeout(lifecycleConditionTimeout).
				WithPolling(lifecycleClusterPolling).
				Should(BeTrue(), "ReconcileActive did not transition to False/VersionPinned")

			By("clearing the version pin")
			lifecycleClearVersionPin(name)

			By("waiting for ReconcileActive=True")
			Eventually(func() bool {
				return lifecycleConditionStatus(name, "ReconcileActive") == lifecycleConditionTrue
			}).WithTimeout(lifecycleConditionTimeout).
				WithPolling(lifecycleClusterPolling).
				Should(BeTrue(), "ReconcileActive did not return to True after unpinning")
		})
	})

	Context("E2E-10 upgrade simulation", func() {
		const (
			pinnedName    = "pinnedcluster"
			unpinnedName  = "frontier"
			upgradeTo     = "0.2.0"
			upgradeScript = "hack/local-dev/rebuild-operator.sh"
		)

		AfterEach(func() {
			lifecycleDeleteCluster(pinnedName)
			lifecycleDeleteCluster(unpinnedName)
		})

		It("leaves a pinned cluster untouched after operator restart with new version", func() {
			lifecycleSkipIfNoRebuildScript(upgradeScript)

			By("applying the unpinned cluster to discover the running operator's version")
			lifecycleApplyCluster(unpinnedName, lifecycleDevVaultAddr(), unpinnedName)
			Eventually(func() bool {
				return lifecycleClusterReady(unpinnedName)
			}).WithTimeout(lifecycleClusterTimeout).
				WithPolling(lifecycleClusterPolling).
				Should(BeTrue(), "unpinned cluster never reached Ready before upgrade")
			beforeVersion := lifecycleClusterManagedByVersion(unpinnedName)
			Expect(beforeVersion).NotTo(BeEmpty(),
				"unable to read operator version from %s/status.managedByVersion", unpinnedName)
			Expect(beforeVersion).NotTo(Equal(upgradeTo),
				"baseline version %q matches upgrade target — pick a different upgradeTo", beforeVersion)

			By("applying the pinned cluster at the current operator version " + beforeVersion)
			lifecycleApplyClusterWithChannel(pinnedName, lifecycleDevVaultAddr(), pinnedName,
				"pinned", beforeVersion)
			Eventually(func() bool {
				return lifecycleConditionStatus(pinnedName, "ReconcileActive") == lifecycleConditionFalse &&
					lifecycleConditionReason(pinnedName, "ReconcileActive") == "VersionPinned"
			}).WithTimeout(lifecycleConditionTimeout).
				WithPolling(lifecycleClusterPolling).
				Should(BeTrue(), "pinned cluster did not settle on ReconcileActive=False/VersionPinned")

			By("rebuilding the operator at version " + upgradeTo)
			cmd := exec.Command(upgradeScript, upgradeTo)
			output, err := cmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(),
				"%s %s failed: %s", upgradeScript, upgradeTo, string(output))

			By("waiting for the unpinned cluster to be re-managed by " + upgradeTo)
			Eventually(func() string {
				return lifecycleClusterManagedByVersion(unpinnedName)
			}).WithTimeout(lifecycleClusterTimeout).
				WithPolling(lifecycleClusterPolling).
				Should(Equal(upgradeTo),
					"unpinned cluster should have been re-managed by the new operator")

			By("verifying the pinned cluster's ManagedByVersion did not advance")
			Consistently(func() string {
				return lifecycleClusterManagedByVersion(pinnedName)
			}).WithTimeout(30*time.Second).
				WithPolling(lifecycleClusterPolling).
				Should(Equal(beforeVersion),
					"pinned cluster's ManagedByVersion changed across upgrade")
			Expect(lifecycleConditionStatus(pinnedName, "ReconcileActive")).
				To(Equal(lifecycleConditionFalse),
					"pinned cluster ReconcileActive should remain False after upgrade")
		})
	})
})

// lifecycleSkipIfNoRebuildScript skips the calling spec when the rebuild
// helper isn't on disk — keeps the test honest in environments without a
// kind cluster wired up.
func lifecycleSkipIfNoRebuildScript(path string) {
	if out, err := exec.Command("test", "-x", path).CombinedOutput(); err != nil {
		Skip(fmt.Sprintf("E2E-10: %s not executable in this environment: %s", path, string(out)))
	}
}

// lifecycleClusterManagedByVersion returns the cluster's
// status.managedByVersion or "" if unset.
func lifecycleClusterManagedByVersion(name string) string {
	cmd := exec.Command("kubectl", "get", "openchamicluster", name,
		"-o", "jsonpath={.status.managedByVersion}")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}
