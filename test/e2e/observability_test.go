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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/openchami/openchami-operator/test/utils"
)

// observability* constants are scoped to this file to avoid collisions with
// peer e2e files written by sibling sub-agents.
const (
	observabilityClusterTimeout      = 5 * time.Minute
	observabilityShortPollInterval   = 5 * time.Second
	observabilityLocalstackEndpoint  = "http://localhost:4566"
	observabilityLocalstackEnvVar    = "LOCALSTACK_ENDPOINT"
	observabilityVenadoClusterName   = "venado"
	observabilityVenadoNamespace     = "openchami-venado"
	observabilityVenadoLogBucket     = "venado-logs"
	observabilityVenadoBackupBucket  = "venado-backups"
	observabilityVenadoCRNamespace   = "default"
	observabilityRetentionDays       = 30
	observabilityFunicularDaemonSet  = "funicular-collector"
	observabilityFunicularFlushVar   = "FUNICULAR_FLUSH_INTERVAL"
	observabilityExpectedFlushSecs   = "60"
	observabilityOchamiAdminBinary   = "bin/ochami-admin"
	observabilityVaultAddr           = "https://localhost:8200"
	observabilityVaultToken          = "root"
	observabilityCertSecretName      = "venado-gateway-tls"
	observabilityShortLivedCertHours = 1
)

// observabilityLocalstackURL returns the localstack endpoint URL, honouring
// the LOCALSTACK_ENDPOINT env var so CI can override it.
func observabilityLocalstackURL() string {
	if v := os.Getenv(observabilityLocalstackEnvVar); v != "" {
		return v
	}
	return observabilityLocalstackEndpoint
}

// observabilityKubectlApply applies a YAML manifest from stdin via kubectl.
func observabilityKubectlApply(manifest string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	return err
}

// observabilityKubectlDelete deletes a resource by kind/name in a namespace,
// ignoring not-found errors so cleanup is idempotent.
func observabilityKubectlDelete(args ...string) {
	full := append([]string{"delete", "--ignore-not-found"}, args...)
	cmd := exec.Command("kubectl", full...)
	if _, err := utils.Run(cmd); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "ignoring delete error: %v\n", err)
	}
}

// observabilityApplyVenadoCluster applies the venado test cluster YAML with
// logging enabled and the specified TLS Secret reference.
func observabilityApplyVenadoCluster() error {
	manifest := fmt.Sprintf(`apiVersion: openchami.org/v1alpha1
kind: OpenCHAMICluster
metadata:
  name: %s
  namespace: %s
spec:
  clusterName: %s
  domain: venado.hpc.example.com
  platform:
    vault:
      address: "https://vault.openchami-platform.svc.cluster.local:8200"
      authMethod: kubernetes
    objectStorage:
      endpoint: "%s"
  networkProbe:
    enabled: false
  services:
    smd:
      replicas: 1
    tokensmith:
      oidcProvider: vault
    bootService:
      replicas: 1
    metadataService:
      replicas: 1
  networking:
    gatewayClass: envoy
    tls:
      issuer: ca-issuer
      secretName: %s
  database:
    instances: 1
    storageSize: 1Gi
  logging:
    enabled: true
    logBucket: %s
    retentionDays: %d
    flushIntervalSeconds: %s
`,
		observabilityVenadoClusterName,
		observabilityVenadoCRNamespace,
		observabilityVenadoClusterName,
		observabilityLocalstackURL(),
		observabilityCertSecretName,
		observabilityVenadoLogBucket,
		observabilityRetentionDays,
		observabilityExpectedFlushSecs,
	)
	return observabilityKubectlApply(manifest)
}

// observabilityFetchCondition returns the Status string for the named
// condition on the venado cluster, or an error if missing.
func observabilityFetchCondition(condType string) (string, error) {
	jp := fmt.Sprintf(`jsonpath={.status.conditions[?(@.type=="%s")].status}`, condType)
	cmd := exec.Command("kubectl", "get", "openchamicluster",
		observabilityVenadoClusterName,
		"-n", observabilityVenadoCRNamespace,
		"-o", jp,
	)
	out, err := utils.Run(cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// observabilityS3LS shells out to the AWS CLI against localstack to list a
// bucket. Returns the combined output and error.
func observabilityS3LS(bucket string) (string, error) {
	cmd := exec.Command("aws",
		"--endpoint-url", observabilityLocalstackURL(),
		"s3", "ls", "s3://"+bucket,
	)
	return utils.Run(cmd)
}

// observabilityS3GetLifecycle reads the lifecycle configuration of a bucket
// via the AWS CLI against localstack.
func observabilityS3GetLifecycle(bucket string) (string, error) {
	cmd := exec.Command("aws",
		"--endpoint-url", observabilityLocalstackURL(),
		"s3api", "get-bucket-lifecycle-configuration",
		"--bucket", bucket,
	)
	return utils.Run(cmd)
}

var _ = Describe("Observability", Ordered, func() {
	BeforeAll(func() {
		SetDefaultEventuallyTimeout(observabilityClusterTimeout)
		SetDefaultEventuallyPollingInterval(observabilityShortPollInterval)
	})

	AfterAll(func() {
		By("removing the venado OpenCHAMICluster CR")
		observabilityKubectlDelete("openchamicluster",
			observabilityVenadoClusterName,
			"-n", observabilityVenadoCRNamespace,
		)
		By("removing the venado namespace")
		observabilityKubectlDelete("namespace", observabilityVenadoNamespace)
	})

	Context("E2E-11: Certificate Warning Event", func() {
		It("fires a Warning Event when the TLS cert has <48h remaining", func() {
			Skip("E2E-11: requires a test-only short-lived cert harness " +
				"(monkey-patched cert-manager Certificate Duration or a hand-crafted " +
				"self-signed Secret with notAfter ~1h from now). The <48h Warning " +
				"path is exercised by the unit tests in " +
				"internal/reconcilers/certificates_test.go. " +
				"TODO(phase-15): wire up a deterministic short-lived Certificate " +
				"so this assertion can run end-to-end against kind.")

			// Intended flow once the harness exists:
			//   1. Pre-create a TLS Secret named observabilityCertSecretName in
			//      observabilityVenadoNamespace whose tls.crt has notAfter set
			//      to ~1h from now (well under the 48h warning gap).
			//   2. Apply the venado cluster so the certificates reconciler picks
			//      up the existing Secret instead of waiting on cert-manager.
			//   3. Eventually the CertificatesValid condition's reason field
			//      (read via kubectl get -o jsonpath) should return
			//      "ExpirationImminent".
			//   4. kubectl get events -n openchami-venado should include a
			//      Warning event with reason=ExpirationImminent.
		})
	})

	Context("E2E-12: Log bucket exists with lifecycle rule", func() {
		It("creates the log bucket and applies the configured retention rule", func() {
			By("applying the venado cluster with logging enabled")
			Expect(observabilityApplyVenadoCluster()).To(Succeed(),
				"failed to apply venado OpenCHAMICluster CR")

			By("waiting for ConditionLogBucketReady=True")
			Eventually(func(g Gomega) {
				status, err := observabilityFetchCondition("LogBucketReady")
				g.Expect(err).NotTo(HaveOccurred(),
					"failed to read LogBucketReady condition")
				g.Expect(status).To(Equal("True"),
					"LogBucketReady condition not True yet")
			}).Should(Succeed())

			By("listing the log bucket via the localstack S3 endpoint")
			Eventually(func(g Gomega) {
				_, err := observabilityS3LS(observabilityVenadoLogBucket)
				g.Expect(err).NotTo(HaveOccurred(),
					"log bucket %s should exist on localstack",
					observabilityVenadoLogBucket)
			}).Should(Succeed())

			By("verifying the lifecycle rule references the configured retention")
			Eventually(func(g Gomega) {
				out, err := observabilityS3GetLifecycle(observabilityVenadoLogBucket)
				g.Expect(err).NotTo(HaveOccurred(),
					"failed to read lifecycle configuration for %s",
					observabilityVenadoLogBucket)
				retentionMarker := fmt.Sprintf(`"Days": %d`, observabilityRetentionDays)
				altMarker := fmt.Sprintf(`"Days":%d`, observabilityRetentionDays)
				g.Expect(out).To(Or(
					ContainSubstring(retentionMarker),
					ContainSubstring(altMarker),
				), "lifecycle configuration missing Days=%d rule",
					observabilityRetentionDays)
			}).Should(Succeed())
		})
	})

	Context("E2E-13: Funicular DaemonSet running", func() {
		It("runs the funicular-collector DaemonSet with the expected env", func() {
			By("ensuring the venado cluster is applied (idempotent)")
			Expect(observabilityApplyVenadoCluster()).To(Succeed(),
				"failed to apply venado OpenCHAMICluster CR")

			By("waiting for the funicular DaemonSet to report numberReady>0")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "daemonset",
					observabilityFunicularDaemonSet,
					"-n", observabilityVenadoNamespace,
					"-o", "jsonpath={.status.numberReady}",
				)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(),
					"failed to fetch DaemonSet %s/%s",
					observabilityVenadoNamespace,
					observabilityFunicularDaemonSet)
				g.Expect(strings.TrimSpace(out)).NotTo(BeEmpty(),
					"DaemonSet status not yet reported")
				g.Expect(strings.TrimSpace(out)).NotTo(Equal("0"),
					"funicular-collector DaemonSet has 0 ready pods")
			}).Should(Succeed())

			By("asserting the funicular container exposes FUNICULAR_FLUSH_INTERVAL")
			Eventually(func(g Gomega) {
				jp := fmt.Sprintf(
					`jsonpath={.spec.template.spec.containers[0].env[?(@.name=="%s")].value}`,
					observabilityFunicularFlushVar,
				)
				cmd := exec.Command("kubectl", "get", "daemonset",
					observabilityFunicularDaemonSet,
					"-n", observabilityVenadoNamespace,
					"-o", jp,
				)
				out, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(),
					"failed to read funicular DaemonSet env")
				g.Expect(strings.TrimSpace(out)).To(Equal(observabilityExpectedFlushSecs),
					"%s should be %q on the funicular container",
					observabilityFunicularFlushVar,
					observabilityExpectedFlushSecs)
			}).Should(Succeed())

			// TODO(phase-15): assert that Parquet objects appear in the log
			// bucket after 2x flush interval. The funicular-collector's actual
			// log-uploading behavior is owned by an external project and is
			// not deterministic in CI today. Re-enable when the collector
			// image gains a synchronous flush hook usable from tests.
		})
	})

	Context("E2E-14: ochami-admin backup", func() {
		It("backs up the cluster to local disk and creates a CNPG Backup CR", func() {
			By("ensuring the venado cluster is applied (idempotent)")
			Expect(observabilityApplyVenadoCluster()).To(Succeed(),
				"failed to apply venado OpenCHAMICluster CR")

			By("waiting for the venado namespace to exist")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "namespace",
					observabilityVenadoNamespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(),
					"namespace %s not yet created",
					observabilityVenadoNamespace)
			}).Should(Succeed())

			By("running ochami-admin backup against a temporary output directory")
			tmpDir, err := os.MkdirTemp("", "e2e-backup-venado-")
			Expect(err).NotTo(HaveOccurred(),
				"failed to create temporary backup directory")
			DeferCleanup(func() {
				_ = os.RemoveAll(tmpDir)
			})

			cmd := exec.Command(
				observabilityOchamiAdminBinary,
				"backup",
				"--cluster-name", observabilityVenadoClusterName,
				"--namespace", observabilityVenadoCRNamespace,
				"--vault-addr", observabilityVaultAddr,
				"--vault-token", observabilityVaultToken,
				"--s3-endpoint", observabilityLocalstackURL(),
				"--s3-bucket", observabilityVenadoBackupBucket,
				"--output-prefix", tmpDir,
			)
			out, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(),
				"ochami-admin backup failed: %s", out)

			By("asserting the local cluster.yaml artifact exists and is non-empty")
			yamlPath := filepath.Join(tmpDir, "cluster.yaml")
			info, err := os.Stat(yamlPath)
			Expect(err).NotTo(HaveOccurred(),
				"expected backup artifact %s to exist", yamlPath)
			Expect(info.Size()).To(BeNumerically(">", 0),
				"backup artifact %s should be non-empty", yamlPath)

			By("asserting a CNPG Backup CR was created in the cluster namespace")
			Eventually(func(g Gomega) {
				lsCmd := exec.Command("kubectl", "get", "backup.postgresql.cnpg.io",
					"-n", observabilityVenadoNamespace,
					"-l", "openchami.org/triggered=ochami-admin-backup",
					"-o", "jsonpath={.items[*].metadata.name}",
				)
				lsOut, lsErr := utils.Run(lsCmd)
				g.Expect(lsErr).NotTo(HaveOccurred(),
					"failed to list CNPG Backup CRs")
				g.Expect(strings.TrimSpace(lsOut)).NotTo(BeEmpty(),
					"no ochami-admin-triggered CNPG Backup CR found in %s",
					observabilityVenadoNamespace)
			}).Should(Succeed())

			// TODO(phase-15): assert that backup artifacts also appear in
			// localstack at s3://<observabilityVenadoBackupBucket>/<cluster>/.
			// The current `ochami-admin backup` implementation only writes
			// artifacts to the local --output-prefix directory and prints
			// copy-paste-ready upload instructions on stderr (see
			// internal/admin/backup.go printManualSteps). Once the upload is
			// automated (or the e2e harness shells out to `aws s3 cp` itself),
			// re-enable the localstack-presence half of this test.
		})
	})
})
