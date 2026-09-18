// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"
	"strings"

	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	egv1alpha1 "github.com/envoyproxy/gateway/api/v1alpha1"
	vsov1beta1 "github.com/hashicorp/vault-secrets-operator/api/v1beta1"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/controller"
	s3client "github.com/openchami/openchami-operator/internal/s3"
	"github.com/openchami/openchami-operator/internal/status"
	"github.com/openchami/openchami-operator/internal/vault"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(openchamiv1alpha1.AddToScheme(scheme))
	utilruntime.Must(vsov1beta1.AddToScheme(scheme))
	utilruntime.Must(cnpgv1.AddToScheme(scheme))
	utilruntime.Must(gwapiv1.Install(scheme))
	utilruntime.Must(cmv1.AddToScheme(scheme))
	utilruntime.Must(egv1alpha1.AddToScheme(scheme))
	utilruntime.Must(monitoringv1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "3f42eabc.openchami.org",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	vaultClient, err := buildVaultClient()
	if err != nil {
		setupLog.Error(err, "Failed to build vault client; vault sub-reconciler will report Unreachable")
	}

	s3c, err := buildS3Client()
	if err != nil {
		setupLog.Error(err, "Failed to build s3 client; bucket sub-reconcilers will report Error")
	}

	//nolint:staticcheck // legacy events API; migration to events.EventRecorder is a future cleanup
	reporter := &status.Reporter{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorderFor("openchami-operator"),
	}

	if err := (&controller.OpenCHAMIControlPlaneReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		//nolint:staticcheck // legacy events API; migration to events.EventRecorder is a future cleanup
		Recorder:    mgr.GetEventRecorderFor("openchamicontrolplane-controller"),
		VaultClient: vaultClient,
		S3Client:    s3c,
		DryRun:      os.Getenv("OPENCHAMI_DRY_RUN") == "true",
		Reporter:    reporter,
		RESTConfig:  mgr.GetConfig(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "openchamicontrolplane")
		os.Exit(1)
	}
	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := openchamiv1alpha1.SetupOpenCHAMIControlPlaneWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "Failed to create webhook", "webhook", "OpenCHAMIControlPlane")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// buildVaultClient constructs a vault.Client from environment variables.
// Returns nil with no error when VAULT_ADDR is unset, allowing the operator
// to start in dev environments without Vault. The vault sub-reconciler will
// then report VaultConfigured=False/Error until config is provided.
//
// VAULT_AUTH_METHOD selects the login method (case-insensitive; defaults to
// "kubernetes"):
//   - kubernetes — VAULT_KUBERNETES_ROLE names the Vault k8s auth role.
//   - approle    — VAULT_ROLE_ID and VAULT_SECRET_ID carry the AppRole
//     credentials. (VAULT_APPROLE_ROLE_ID / VAULT_APPROLE_SECRET_ID are
//     accepted as fallbacks for backwards compatibility.)
//   - token      — VAULT_TOKEN carries a bearer token. Dev/bootstrap only.
func buildVaultClient() (vault.Client, error) {
	cfg, ok := vaultConfigFromEnv(os.Getenv)
	if !ok {
		return nil, nil
	}
	return vault.NewClient(context.Background(), cfg)
}

// vaultConfigFromEnv assembles a vault.Config from the given environment
// lookup. ok is false when VAULT_ADDR is unset (the operator then starts
// without Vault). Factored out of buildVaultClient so the env-to-Config
// mapping is unit-testable without a live Vault login.
func vaultConfigFromEnv(getenv func(string) string) (cfg vault.Config, ok bool) {
	addr := getenv("VAULT_ADDR")
	if addr == "" {
		return vault.Config{}, false
	}
	cfg = vault.Config{
		Address:    addr,
		AuthMethod: getenv("VAULT_AUTH_METHOD"),
		K8sRole:    getenv("VAULT_KUBERNETES_ROLE"),
	}
	if cfg.AuthMethod == "" {
		cfg.AuthMethod = "kubernetes"
	}
	// Match case-insensitively so operators can pass "approle", "appRole",
	// or "app-role"; vault.NewClient normalizes the same way internally.
	switch normalizeVaultAuthMethod(cfg.AuthMethod) {
	case "appRole":
		cfg.AppRoleID = firstNonEmpty(
			getenv("VAULT_ROLE_ID"),
			getenv("VAULT_APPROLE_ROLE_ID"),
		)
		cfg.AppRoleSecretID = firstNonEmpty(
			getenv("VAULT_SECRET_ID"),
			getenv("VAULT_APPROLE_SECRET_ID"),
		)
	case "token":
		cfg.Token = getenv("VAULT_TOKEN")
	}
	return cfg, true
}

// normalizeVaultAuthMethod mirrors the normalization vault.NewClient applies,
// so buildVaultClient can decide which credential env vars to read. Returns
// the canonical spelling ("kubernetes", "appRole", "token") or the lower-cased
// input for anything unrecognized.
func normalizeVaultAuthMethod(m string) string {
	switch strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(m)) {
	case "kubernetes", "k8s":
		return "kubernetes"
	case "approle":
		return "appRole"
	case "token":
		return "token"
	default:
		return strings.ToLower(m)
	}
}

// firstNonEmpty returns the first non-empty string in vals, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// buildS3Client constructs an s3.Client from environment variables.
// Returns nil with no error when AWS_ENDPOINT_URL is unset, so the operator
// can start in environments without VersityGW; the bucket sub-reconcilers
// will then report `s3 client not configured` until config is provided.
//
// AWS_ENDPOINT_URL points at the VersityGW (or LocalStack/MinIO) gateway.
// Credentials come from the standard AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
// pair, region from AWS_REGION (defaulting to us-east-1 inside NewClient).
// AWS_S3_TLS_INSECURE=true disables certificate verification — dev only.
func buildS3Client() (s3client.Client, error) {
	endpoint := os.Getenv("AWS_ENDPOINT_URL")
	if endpoint == "" {
		return nil, nil
	}
	cfg := s3client.Config{
		Endpoint:    endpoint,
		AccessKey:   os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey:   os.Getenv("AWS_SECRET_ACCESS_KEY"),
		Region:      os.Getenv("AWS_REGION"),
		TLSInsecure: os.Getenv("AWS_S3_TLS_INSECURE") == "true",
	}
	return s3client.NewClient(context.Background(), cfg)
}
