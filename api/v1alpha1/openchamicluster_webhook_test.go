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

package v1alpha1

import (
	"context"
	"maps"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Shared test constants extracted to keep goconst happy.
const (
	wTestVaultHTTPS = "https://vault.test:8200"
	wTestS3         = "https://s3.test:9000"
	wTestNodeKey    = "node-role"
	wTestProvision  = "10.0.0.0/24"
	wTestBMC        = "10.1.0.0/24"
)

func newWebhookScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	return scheme
}

// newFixtureCluster returns a minimally-valid OpenCHAMICluster suitable for
// validation tests. It uses networkProbe.enabled=true so DHCP/Magellan
// nodeSelector requirements don't apply. Tests can override fields freely.
func newFixtureCluster(name string) *OpenCHAMICluster {
	return &OpenCHAMICluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID(name + "-uid"),
		},
		Spec: OpenCHAMIClusterSpec{
			ClusterName: name,
			Domain:      name + ".test.local",
			Platform: PlatformSpec{
				Vault: VaultSpec{
					Address:    wTestVaultHTTPS,
					AuthMethod: VaultAuthMethodKubernetes,
				},
				ObjectStorage: ObjectStorageSpec{Endpoint: wTestS3},
			},
			Services: ServicesSpec{
				Tokensmith: TokensmithSpec{OIDCProvider: "vault"},
				CoreDHCP:   CoreDHCPSpec{Enabled: true},
				Magellan:   MagellanSpec{Enabled: true},
			},
			NetworkProbe: NetworkProbeSpec{
				Enabled:          true,
				ProvisionNetwork: &NetworkProbeTarget{Subnet: wTestProvision},
				BMCNetwork:       &NetworkProbeTarget{Subnet: wTestBMC},
			},
		},
	}
}

func newWebhook(t *testing.T, existing ...client.Object) *OpenCHAMIClusterWebhook {
	t.Helper()
	scheme := newWebhookScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing...).Build()
	return &OpenCHAMIClusterWebhook{Client: c}
}

// ---------- Defaulting ---------------------------------------------------

func TestDefault_AppliesAllDerivedDefaults(t *testing.T) {
	w := &OpenCHAMIClusterWebhook{}
	c := &OpenCHAMICluster{
		Spec: OpenCHAMIClusterSpec{ClusterName: "alpha"},
	}
	if err := w.Default(context.Background(), c); err != nil {
		t.Fatalf("Default: %v", err)
	}

	checks := map[string]string{
		"objectStorage.bucket": c.Spec.Platform.ObjectStorage.Bucket,
		"logging.logBucket":    c.Spec.Logging.LogBucket,
		"tls.secretName":       c.Spec.Networking.TLS.SecretName,
		"networking.gwClass":   c.Spec.Networking.GatewayClass,
		"tls.issuer":           c.Spec.Networking.TLS.Issuer,
		"coreDHCP.unknown":     c.Spec.Services.CoreDHCP.UnknownLeaseDuration,
		"coreDHCP.known":       c.Spec.Services.CoreDHCP.KnownLeaseDuration,
		"magellan.schedule":    c.Spec.Services.Magellan.Schedule,
	}
	for label, got := range checks {
		if got == "" {
			t.Errorf("%s: expected non-empty default, got empty", label)
		}
	}

	if c.Spec.Platform.ObjectStorage.Bucket != "alpha-boot-images" {
		t.Errorf("bucket = %q, want alpha-boot-images", c.Spec.Platform.ObjectStorage.Bucket)
	}
	if c.Spec.Logging.LogBucket != "alpha-logs" {
		t.Errorf("logBucket = %q, want alpha-logs", c.Spec.Logging.LogBucket)
	}
	if c.Spec.Networking.TLS.SecretName != "alpha-gateway-tls" {
		t.Errorf("secretName = %q, want alpha-gateway-tls", c.Spec.Networking.TLS.SecretName)
	}
	if c.Spec.Database.Instances != 3 {
		t.Errorf("database.instances = %d, want 3", c.Spec.Database.Instances)
	}
	if c.Spec.NetworkProbe.IntervalSeconds != 300 {
		t.Errorf("intervalSeconds = %d, want 300", c.Spec.NetworkProbe.IntervalSeconds)
	}
	if c.Spec.Logging.RetentionDays != 90 {
		t.Errorf("retentionDays = %d, want 90", c.Spec.Logging.RetentionDays)
	}
	if c.Spec.Logging.FlushIntervalSeconds != 60 {
		t.Errorf("flushIntervalSeconds = %d, want 60", c.Spec.Logging.FlushIntervalSeconds)
	}
	if c.Spec.Services.SMD.Replicas != 2 {
		t.Errorf("smd.replicas = %d, want 2", c.Spec.Services.SMD.Replicas)
	}
	if c.Spec.Services.Tokensmith.Replicas != 1 {
		t.Errorf("tokensmith.replicas = %d, want 1", c.Spec.Services.Tokensmith.Replicas)
	}
	if c.Spec.Services.BootService.Replicas != 2 {
		t.Errorf("bootService.replicas = %d, want 2", c.Spec.Services.BootService.Replicas)
	}
	if c.Spec.Services.MetadataService.Replicas != 2 {
		t.Errorf("metadataService.replicas = %d, want 2", c.Spec.Services.MetadataService.Replicas)
	}
	if c.Spec.Services.Magellan.ConcurrencyPolicy != batchv1.ForbidConcurrent {
		t.Errorf("magellan.concurrencyPolicy = %q, want Forbid", c.Spec.Services.Magellan.ConcurrencyPolicy)
	}
}

func TestDefault_PreservesUserValues(t *testing.T) {
	w := &OpenCHAMIClusterWebhook{}
	c := &OpenCHAMICluster{
		Spec: OpenCHAMIClusterSpec{
			ClusterName: "beta",
			Platform: PlatformSpec{
				ObjectStorage: ObjectStorageSpec{Bucket: "user-bucket"},
			},
			Logging: LoggingSpec{
				LogBucket:            "user-logs",
				RetentionDays:        7,
				FlushIntervalSeconds: 5,
			},
			Networking: NetworkingSpec{
				GatewayClass: "custom-gw",
				TLS: TLSSpec{
					Issuer:     "custom-issuer",
					SecretName: "custom-tls",
				},
			},
			Database: DatabaseSpec{Instances: 1},
			Services: ServicesSpec{
				SMD: SMDSpec{ServiceDefaults: ServiceDefaults{Replicas: 5}},
				CoreDHCP: CoreDHCPSpec{
					UnknownLeaseDuration: "30m",
					KnownLeaseDuration:   "12h",
				},
				Magellan: MagellanSpec{
					Schedule:          "0 * * * *",
					ConcurrencyPolicy: batchv1.AllowConcurrent,
				},
			},
		},
	}
	if err := w.Default(context.Background(), c); err != nil {
		t.Fatalf("Default: %v", err)
	}
	if c.Spec.Platform.ObjectStorage.Bucket != "user-bucket" {
		t.Errorf("bucket overwritten: %q", c.Spec.Platform.ObjectStorage.Bucket)
	}
	if c.Spec.Logging.LogBucket != "user-logs" {
		t.Errorf("logBucket overwritten: %q", c.Spec.Logging.LogBucket)
	}
	if c.Spec.Networking.GatewayClass != "custom-gw" {
		t.Errorf("gatewayClass overwritten: %q", c.Spec.Networking.GatewayClass)
	}
	if c.Spec.Networking.TLS.SecretName != "custom-tls" {
		t.Errorf("secretName overwritten: %q", c.Spec.Networking.TLS.SecretName)
	}
	if c.Spec.Database.Instances != 1 {
		t.Errorf("database.instances overwritten: %d", c.Spec.Database.Instances)
	}
	if c.Spec.Services.SMD.Replicas != 5 {
		t.Errorf("smd.replicas overwritten: %d", c.Spec.Services.SMD.Replicas)
	}
	if c.Spec.Services.Magellan.ConcurrencyPolicy != batchv1.AllowConcurrent {
		t.Errorf("concurrencyPolicy overwritten: %q", c.Spec.Services.Magellan.ConcurrencyPolicy)
	}
	if c.Spec.Logging.RetentionDays != 7 {
		t.Errorf("retentionDays overwritten: %d", c.Spec.Logging.RetentionDays)
	}
}

// ---------- Validation ---------------------------------------------------

// expectInvalid asserts the error is an apierrors.Invalid containing
// the substring on at least one field.
func expectInvalid(t *testing.T, err error, fieldSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", fieldSubstr)
	}
	statusErr, ok := err.(*apierrors.StatusError)
	if !ok {
		t.Fatalf("expected *apierrors.StatusError, got %T: %v", err, err)
	}
	if statusErr.ErrStatus.Details == nil {
		t.Fatalf("expected Details on status error, got nil; err=%v", err)
	}
	for _, c := range statusErr.ErrStatus.Details.Causes {
		if strings.Contains(c.Field, fieldSubstr) {
			return
		}
	}
	t.Fatalf("no cause matched field substring %q; causes=%+v", fieldSubstr, statusErr.ErrStatus.Details.Causes)
}

func TestValidateCreate_VaultAddressMustBeHTTPS(t *testing.T) {
	w := newWebhook(t)
	c := newFixtureCluster("a")
	c.Spec.Platform.Vault.Address = "http://vault.test:8200"
	if _, err := w.ValidateCreate(context.Background(), c); err == nil {
		t.Fatal("expected error for non-https vault address")
	} else {
		expectInvalid(t, err, "vault.address")
	}
}

func TestValidateCreate_AppRoleRequiresSecretRef(t *testing.T) {
	w := newWebhook(t)
	c := newFixtureCluster("a")
	c.Spec.Platform.Vault.AuthMethod = VaultAuthMethodAppRole
	c.Spec.Platform.Vault.AppRoleSecretRef = nil
	_, err := w.ValidateCreate(context.Background(), c)
	expectInvalid(t, err, "appRoleSecretRef")
}

func TestValidateCreate_TokensmithExternalRequiresIssuer(t *testing.T) {
	w := newWebhook(t)
	c := newFixtureCluster("a")
	c.Spec.Services.Tokensmith.OIDCProvider = "external"
	c.Spec.Services.Tokensmith.OIDCIssuerURL = ""
	_, err := w.ValidateCreate(context.Background(), c)
	expectInvalid(t, err, "oidcIssuerURL")
}

func TestValidateCreate_DuplicateClusterName(t *testing.T) {
	existing1 := newFixtureCluster("alpha")
	existing1.Name = "first"
	existing1.UID = "first-uid"
	existing2 := newFixtureCluster("beta")
	existing2.Name = "second"
	existing2.UID = "second-uid"

	w := newWebhook(t, existing1, existing2)

	candidate := newFixtureCluster("alpha")
	candidate.Name = "third"
	candidate.UID = "third-uid"
	_, err := w.ValidateCreate(context.Background(), candidate)
	expectInvalid(t, err, "clusterName")
}

func TestValidateCreate_ProbeOff_RequiresNodeSelector(t *testing.T) {
	w := newWebhook(t)
	c := newFixtureCluster("a")
	c.Spec.NetworkProbe.Enabled = false
	c.Spec.NetworkProbe.ProvisionNetwork = nil
	c.Spec.NetworkProbe.BMCNetwork = nil
	c.Spec.Services.CoreDHCP.NodeSelector = nil
	_, err := w.ValidateCreate(context.Background(), c)
	expectInvalid(t, err, "coreDHCP.nodeSelector")
}

func TestValidateCreate_ProbeOff_NodeSelectorWithoutDiscriminator(t *testing.T) {
	w := newWebhook(t)
	c := newFixtureCluster("alpha")
	c.Spec.NetworkProbe.Enabled = false
	c.Spec.NetworkProbe.ProvisionNetwork = nil
	c.Spec.NetworkProbe.BMCNetwork = nil
	// value does not contain "alpha"
	c.Spec.Services.CoreDHCP.NodeSelector = map[string]string{wTestNodeKey: "dhcp"}
	_, err := w.ValidateCreate(context.Background(), c)
	expectInvalid(t, err, "coreDHCP.nodeSelector")
}

func TestValidateCreate_ProbeOff_DuplicateNodeSelector(t *testing.T) {
	selector := map[string]string{wTestNodeKey: "alpha-dhcp"}

	existing := newFixtureCluster("first")
	existing.Spec.ClusterName = "first"
	existing.Spec.NetworkProbe.Enabled = false
	existing.Spec.NetworkProbe.ProvisionNetwork = nil
	existing.Spec.NetworkProbe.BMCNetwork = nil
	existing.Spec.Services.CoreDHCP.NodeSelector = copySelector(selector)

	w := newWebhook(t, existing)

	candidate := newFixtureCluster("alpha")
	candidate.Spec.NetworkProbe.Enabled = false
	candidate.Spec.NetworkProbe.ProvisionNetwork = nil
	candidate.Spec.NetworkProbe.BMCNetwork = nil
	candidate.Spec.Services.CoreDHCP.NodeSelector = copySelector(selector)

	_, err := w.ValidateCreate(context.Background(), candidate)
	expectInvalid(t, err, "coreDHCP.nodeSelector")
}

func TestValidateCreate_ProbeOn_DHCPRequiresProvisionNetwork(t *testing.T) {
	w := newWebhook(t)
	c := newFixtureCluster("a")
	c.Spec.Services.CoreDHCP.Enabled = true
	c.Spec.NetworkProbe.ProvisionNetwork = nil
	_, err := w.ValidateCreate(context.Background(), c)
	expectInvalid(t, err, "provisionNetwork")
}

func TestValidateCreate_ProbeOn_MagellanRequiresBMCNetwork(t *testing.T) {
	w := newWebhook(t)
	c := newFixtureCluster("a")
	c.Spec.Services.Magellan.Enabled = true
	c.Spec.NetworkProbe.BMCNetwork = nil
	_, err := w.ValidateCreate(context.Background(), c)
	expectInvalid(t, err, "bmcNetwork")
}

func TestValidateCreate_ProbeOn_NodeSelectorEmitsWarning(t *testing.T) {
	w := newWebhook(t)
	c := newFixtureCluster("a")
	c.Spec.Services.CoreDHCP.NodeSelector = map[string]string{wTestNodeKey: "dhcp"}
	warnings, err := w.ValidateCreate(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("expected at least one warning")
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "ignored") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected an 'ignored' warning, got %v", warnings)
	}
}

func TestValidateUpdate_ImmutableClusterName(t *testing.T) {
	w := newWebhook(t)
	oldObj := newFixtureCluster("alpha")
	newObj := newFixtureCluster("alpha")
	newObj.Spec.ClusterName = "beta"
	_, err := w.ValidateUpdate(context.Background(), oldObj, newObj)
	expectInvalid(t, err, "clusterName")
}

func TestValidateCreate_HappyPath_ProbeOn(t *testing.T) {
	w := newWebhook(t)
	c := newFixtureCluster("alpha")
	warnings, err := w.ValidateCreate(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got %v", warnings)
	}
}

func TestValidateCreate_HappyPath_ProbeOff(t *testing.T) {
	w := newWebhook(t)
	c := newFixtureCluster("alpha")
	c.Spec.NetworkProbe.Enabled = false
	c.Spec.NetworkProbe.ProvisionNetwork = nil
	c.Spec.NetworkProbe.BMCNetwork = nil
	c.Spec.Services.CoreDHCP.NodeSelector = map[string]string{
		wTestNodeKey: "alpha-dhcp",
	}
	warnings, err := w.ValidateCreate(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got %v", warnings)
	}
}

func TestValidateDelete_AlwaysAllowed(t *testing.T) {
	w := newWebhook(t)
	c := newFixtureCluster("alpha")
	warnings, err := w.ValidateDelete(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if warnings != nil {
		t.Errorf("expected nil warnings, got %v", warnings)
	}
}

// copySelector defends against fake-client / spec map aliasing in tests.
func copySelector(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

// Touch corev1 and explicit imports so static analysis doesn't trip on
// unused-import false positives if tests are pruned later.
var _ = corev1.LocalObjectReference{}
