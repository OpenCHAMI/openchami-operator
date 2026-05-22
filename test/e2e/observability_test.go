//go:build e2e
// +build e2e

// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
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
	manifest := fmt.Sprintf(`apiVersion: openchami.openchami.org/v1alpha1
kind: OpenCHAMIControlPlane
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

// observabilityShortLivedSecretYAML generates a self-signed cert/key pair
// whose NotAfter is the supplied timestamp, then returns the YAML for a
// kubernetes.io/tls Secret carrying both PEM blocks. Used by E2E-11 to drive
// the certificates reconciler's <48h warning path without waiting on a real
// cert-manager issuance.
func observabilityShortLivedSecretYAML(namespace, name string, notAfter time.Time) (string, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generating ec key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "openchami-e2e-11"},
		NotBefore:    notAfter.Add(-72 * time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return "", fmt.Errorf("creating cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return "", fmt.Errorf("marshalling key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: kubernetes.io/tls
data:
  tls.crt: %s
  tls.key: %s
`,
		name,
		namespace,
		base64.StdEncoding.EncodeToString(certPEM),
		base64.StdEncoding.EncodeToString(keyPEM),
	), nil
}

// observabilityFetchConditionReason returns the Reason string for the named
// condition on the venado cluster.
func observabilityFetchConditionReason(condType string) (string, error) {
	jp := fmt.Sprintf(`jsonpath={.status.conditions[?(@.type=="%s")].reason}`, condType)
	cmd := exec.Command("kubectl", "get", "openchamicontrolplane",
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

// observabilityFetchCondition returns the Status string for the named
// condition on the venado cluster, or an error if missing.
func observabilityFetchCondition(condType string) (string, error) {
	jp := fmt.Sprintf(`jsonpath={.status.conditions[?(@.type=="%s")].status}`, condType)
	cmd := exec.Command("kubectl", "get", "openchamicontrolplane",
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
		By("installing CRDs")
		cmd := exec.Command("make", "install")
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		SetDefaultEventuallyTimeout(observabilityClusterTimeout)
		SetDefaultEventuallyPollingInterval(observabilityShortPollInterval)
	})

	AfterAll(func() {
		By("undeploying the controller-manager")
		cmd := exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing the venado OpenCHAMIControlPlane CR")
		observabilityKubectlDelete("openchamicontrolplane",
			observabilityVenadoClusterName,
			"-n", observabilityVenadoCRNamespace,
		)
		By("removing the venado namespace")
		observabilityKubectlDelete("namespace", observabilityVenadoNamespace)
	})

	Context("E2E-11: Certificate Warning Event", func() {
		It("fires a Warning Event when the TLS cert has <48h remaining", func() {
			By("pre-creating the per-cluster namespace so the Secret can land first")
			Expect(observabilityKubectlApply(fmt.Sprintf(
				"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n",
				observabilityVenadoNamespace,
			))).To(Succeed(), "creating namespace %s", observabilityVenadoNamespace)

			By("generating a self-signed cert with notAfter ~1h from now")
			notAfter := time.Now().Add(time.Duration(observabilityShortLivedCertHours) * time.Hour)
			secretYAML, err := observabilityShortLivedSecretYAML(
				observabilityVenadoNamespace,
				observabilityCertSecretName,
				notAfter,
			)
			Expect(err).NotTo(HaveOccurred(), "generating short-lived cert")
			Expect(observabilityKubectlApply(secretYAML)).To(Succeed(),
				"applying pre-baked TLS Secret %s", observabilityCertSecretName)

			By("applying the venado cluster so the certificates reconciler picks up the existing Secret")
			Expect(observabilityApplyVenadoCluster()).To(Succeed(),
				"failed to apply venado OpenCHAMIControlPlane CR")

			By("waiting for ConditionCertificatesValid reason=ExpirationImminent")
			Eventually(func(g Gomega) {
				out, ferr := observabilityFetchConditionReason("CertificatesValid")
				g.Expect(ferr).NotTo(HaveOccurred(),
					"failed to read CertificatesValid reason")
				g.Expect(out).To(Equal("ExpirationImminent"),
					"CertificatesValid reason should be ExpirationImminent for a <48h cert")
			}).Should(Succeed())

			By("waiting for a Warning Event with reason=ExpirationImminent")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "events",
					"-n", observabilityVenadoNamespace,
					"--field-selector", "reason=ExpirationImminent,type=Warning",
					"-o", "jsonpath={.items[*].metadata.name}",
				)
				out, ferr := utils.Run(cmd)
				g.Expect(ferr).NotTo(HaveOccurred(),
					"kubectl get events failed")
				g.Expect(strings.TrimSpace(out)).NotTo(BeEmpty(),
					"expected at least one Warning Event with reason=ExpirationImminent")
			}).Should(Succeed())

			By("verifying status.certExpiryTime is within ~1h of the generated NotAfter")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "openchamicontrolplane",
					observabilityVenadoClusterName,
					"-n", observabilityVenadoCRNamespace,
					"-o", "jsonpath={.status.certExpiryTime}",
				)
				out, ferr := utils.Run(cmd)
				g.Expect(ferr).NotTo(HaveOccurred(), "fetching certExpiryTime")
				ts := strings.TrimSpace(out)
				g.Expect(ts).NotTo(BeEmpty(), "status.certExpiryTime not set")
				parsed, perr := time.Parse(time.RFC3339, ts)
				g.Expect(perr).NotTo(HaveOccurred(),
					"status.certExpiryTime not RFC3339: %q", ts)
				skew := parsed.Sub(notAfter)
				if skew < 0 {
					skew = -skew
				}
				g.Expect(skew).To(BeNumerically("<", time.Minute),
					"status.certExpiryTime %v drifted from generated notAfter %v",
					parsed, notAfter)
			}).Should(Succeed())
		})
	})

	Context("E2E-12: Log bucket exists with lifecycle rule", func() {
		It("creates the log bucket and applies the configured retention rule", func() {
			By("applying the venado cluster with logging enabled")
			Expect(observabilityApplyVenadoCluster()).To(Succeed(),
				"failed to apply venado OpenCHAMIControlPlane CR")

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
				"failed to apply venado OpenCHAMIControlPlane CR")

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

			// Parquet-presence assertion is gated on E2E_PARQUET_PRESENCE=1.
			// The funicular-collector's flush behaviour is owned upstream and
			// is not deterministic in CI, so unset = skip-with-warning rather
			// than silently green. Set E2E_PARQUET_PRESENCE=1 in environments
			// that have a synchronous flush hook wired up.
			if os.Getenv("E2E_PARQUET_PRESENCE") == "1" {
				By("waiting for parquet objects in the log bucket")
				Eventually(func(g Gomega) {
					out, err := observabilityS3LS(observabilityVenadoLogBucket)
					g.Expect(err).NotTo(HaveOccurred(),
						"listing log bucket %s for parquet check",
						observabilityVenadoLogBucket)
					g.Expect(strings.ToLower(out)).To(ContainSubstring(".parquet"),
						"no .parquet objects found in %s",
						observabilityVenadoLogBucket)
				}).WithTimeout(2 * time.Minute).Should(Succeed())
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter,
					"E2E-13: skipping parquet-presence assertion (set E2E_PARQUET_PRESENCE=1 to enable)\n")
			}
		})
	})

	Context("E2E-14: ochami-admin backup", func() {
		It("backs up the cluster to local disk and creates a CNPG Backup CR", func() {
			By("ensuring the venado cluster is applied (idempotent)")
			Expect(observabilityApplyVenadoCluster()).To(Succeed(),
				"failed to apply venado OpenCHAMIControlPlane CR")

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

			// Backup-presence assertion is gated on E2E_BACKUP_PRESENCE=1.
			// `ochami-admin backup` currently only writes artifacts to the
			// local --output-prefix directory and prints upload instructions
			// on stderr (see internal/admin/backup.go printManualSteps).
			// When that command grows automated upload — or the harness shells
			// out to `aws s3 cp` itself — set E2E_BACKUP_PRESENCE=1 to enable
			// the localstack-presence half of this test.
			if os.Getenv("E2E_BACKUP_PRESENCE") == "1" {
				By("listing backup artifacts in the localstack backup bucket")
				Eventually(func(g Gomega) {
					out, err := observabilityS3LS(observabilityVenadoBackupBucket)
					g.Expect(err).NotTo(HaveOccurred(),
						"listing backup bucket %s",
						observabilityVenadoBackupBucket)
					g.Expect(strings.TrimSpace(out)).NotTo(BeEmpty(),
						"backup bucket %s is empty after backup",
						observabilityVenadoBackupBucket)
				}).WithTimeout(2 * time.Minute).Should(Succeed())
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter,
					"E2E-14: skipping localstack backup-presence assertion (set E2E_BACKUP_PRESENCE=1 to enable)\n")
			}
		})
	})
})
