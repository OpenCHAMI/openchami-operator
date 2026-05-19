// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/logging"
)

const (
	// bootstrapTokenTTL is the lifetime requested when minting a new
	// RFC 8693 bootstrap token. tokensmith caps this at 1h.
	bootstrapTokenTTL = "1h"

	// bootstrapTokenRefreshTTL is the refresh-token lifetime issued at
	// exchange time. Consumers use this refresh token to renew their
	// short-lived access tokens without re-presenting the bootstrap.
	// tokensmith caps this at 30d.
	bootstrapTokenRefreshTTL = "24h"

	// bootstrapTokenRefreshAge is how old the stored token can get
	// before the operator mints a replacement. Set well under
	// bootstrapTokenTTL so a service pod restart after this window
	// still finds a usable token in the Secret.
	bootstrapTokenRefreshAge = 30 * time.Minute

	// bootstrapTokenStorePath is where tokensmith persists its
	// bootstrap-token store inside the running pod — matches
	// TOKENSMITH_RFC8693_BOOTSTRAP_STORE in tokensmith.go.
	bootstrapTokenStorePath = tokensmithDataPath + "/bootstrap-tokens"

	// bootstrapTokenAudience is the `aud` claim each operator-minted
	// bootstrap token will exchange into. SMD is the consumer for both
	// boot-service and metadata-service, so both target `hsm`.
	bootstrapTokenAudience = "hsm"
)

// bootstrapTokenSpec parameterizes a single per-service bootstrap-token
// provisioning operation. Different services need different scopes
// (read-only vs read-write) and different Secret names/labels, but the
// mint/store/refresh pipeline is identical.
type bootstrapTokenSpec struct {
	// secretSuffix is appended to the cluster name to form the Secret
	// name (see SecretName + helpers.go suffix constants).
	secretSuffix string
	// subject becomes the `sub` claim on the minted bootstrap token.
	// tokensmith uses it for per-subject policy lookups and the audit
	// trail. Conventionally matches the canonical service name.
	subject string
	// scopes is the comma-separated scope list embedded in the
	// bootstrap token's server-side policy. boot-service writes to
	// SMD (node:read+node:write); metadata-service is a pure reader
	// (node:read+group:read).
	scopes string
	// appName is stamped onto the bootstrap-token Secret's app.kubernetes.io/name
	// label so operators can `kubectl get secret -l app.kubernetes.io/name=...`.
	appName string
}

// bootServiceBootstrap is the spec for boot-service's bootstrap token —
// it writes inventory snapshots into SMD during background sync.
//
// Subject == ServiceBootService because tokensmith uses the `sub` claim
// for per-subject policy lookups; conventionally that matches the
// canonical service name.
var bootServiceBootstrap = bootstrapTokenSpec{
	secretSuffix: SuffixBootServiceBootstr,
	subject:      ServiceBootService,
	scopes:       "node:read,node:write",
	appName:      ServiceBootService,
}

// metadataServiceBootstrap is the spec for metadata-service's bootstrap
// token. metadata-service reads node + group information from SMD to
// serve cloud-init/metadata lookups; it never writes back, so the
// scopes are read-only.
var metadataServiceBootstrap = bootstrapTokenSpec{
	secretSuffix: SuffixMetadataServiceBootstr,
	subject:      ServiceMetadataService,
	scopes:       "node:read,group:read",
	appName:      ServiceMetadataService,
}

// bootstrapTokenCreateResult mirrors the JSON shape `tokensmith
// bootstrap-token create --output-format json` emits. The CLI is the
// source of truth for this schema; only the fields the operator
// consumes are declared here.
type bootstrapTokenCreateResult struct {
	BootstrapToken string `json:"bootstrap_token"`
}

// provisionServiceBootstrapTokens is the post-Ready step of the
// tokensmith reconciler. It ensures every operator-managed consumer
// that needs an RFC 8693 bootstrap token to talk to SMD has a fresh
// Secret in the cluster namespace.
//
// Today the consumers are boot-service (read/write — pushes inventory
// during sync) and metadata-service (read-only — pulls node/group
// state to serve cloud-init lookups). Services that are disabled or
// externally provided are skipped — provisioning a Secret no Deployment
// will mount is just noise.
//
// Returns nil on success or "step not yet applicable" (e.g. exec
// support not wired in tests). Returns an error only for real
// failures the controller should surface as a reconcile error.
func (r *TokensmithReconciler) provisionServiceBootstrapTokens(
	ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane,
) error {
	if r.RESTConfig == nil {
		// Unit-test reconciler — pods/exec is unavailable. Skip
		// silently; provisioning is a runtime concern only.
		return nil
	}

	if ServiceDeployedInCluster(cp, ServiceBootService) {
		if err := r.provisionBootstrapToken(ctx, cp, bootServiceBootstrap); err != nil {
			return err
		}
	}
	if ServiceDeployedInCluster(cp, ServiceMetadataService) {
		if err := r.provisionBootstrapToken(ctx, cp, metadataServiceBootstrap); err != nil {
			return err
		}
	}
	return nil
}

// provisionBootstrapToken ensures `<cluster>-<spec.secretSuffix>`
// contains a non-stale bootstrap token minted for spec.subject with
// spec.scopes. Mint pipeline matches the original boot-service flow:
// exec into a Ready tokensmith pod, capture JSON stdout, parse the
// `bootstrap_token` field, and SSA-apply the Secret.
func (r *TokensmithReconciler) provisionBootstrapToken(
	ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane, spec bootstrapTokenSpec,
) error {
	log := logging.Enrich(ctx, cp, "tokensmith-bootstrap")
	log = log.WithValues("subject", spec.subject)
	ns := ControlPlaneNamespace(cp)
	secretName := SecretName(cp, spec.secretSuffix)

	// Mint-once-per-cluster model: provision the Secret if missing or
	// if its mintedAt timestamp is older than bootstrapTokenRefreshAge.
	// The operator deliberately does NOT try to detect downstream
	// consumption — tokensmith bootstrap tokens are single-use, so a
	// rolling restart will wedge consumers in CrashLoopBackOff with
	// "already consumed" until either the Secret ages out or an admin
	// deletes it. That visible failure is correct: it surfaces the
	// upstream single-use + no-persistence composition (see
	// docs/portable-service-identity-prompt.md in the tokensmith /
	// boot-service / metadata-service repos) instead of hiding it
	// behind operator-side heuristics. When the consumer services adopt
	// mTLS service-identity certs (tokensmith PR-24), this Secret
	// becomes vestigial and gets removed in a follow-on change.
	existing := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, existing)
	switch {
	case apierrors.IsNotFound(err):
		// Fall through to minting below.
	case err != nil:
		return fmt.Errorf("reading %s bootstrap-token Secret: %w", spec.subject, err)
	default:
		if isBootstrapTokenFresh(existing) {
			log.Info("bootstrap token still fresh, skipping mint",
				"mintedAt", existing.Annotations[BootstrapTokenMintedAtAnnotation])
			return nil
		}
		log.Info("bootstrap token stale, re-minting",
			"mintedAt", existing.Annotations[BootstrapTokenMintedAtAnnotation])
	}

	token, err := r.mintBootstrapTokenViaExec(ctx, cp, spec)
	if err != nil {
		return fmt.Errorf("minting %s bootstrap token: %w", spec.subject, err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: coreAPIVersion, Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        secretName,
			Namespace:   ns,
			Annotations: map[string]string{BootstrapTokenMintedAtAnnotation: now},
			Labels: map[string]string{
				labelManagedBy: managedByValue,
				labelAppName:   spec.appName,
			},
		},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{BootstrapTokenKey: token},
	}
	if err := r.Client.Patch(ctx, secret, client.Apply, //nolint:staticcheck // SSA via Patch
		client.ForceOwnership, client.FieldOwner(fieldManager)); err != nil {
		return fmt.Errorf("applying %s bootstrap-token Secret: %w", spec.subject, err)
	}
	log.Info("bootstrap token Secret applied", "name", secretName)
	return nil
}

// isBootstrapTokenFresh inspects the openchami.org/bootstrap-token-minted-at
// annotation. Returns true only when the timestamp parses cleanly AND is
// within bootstrapTokenRefreshAge of now. An unparseable or missing
// timestamp triggers a re-mint, so corrupt Secrets recover automatically.
func isBootstrapTokenFresh(s *corev1.Secret) bool {
	if _, ok := s.Data[BootstrapTokenKey]; !ok {
		return false
	}
	stamp := s.Annotations[BootstrapTokenMintedAtAnnotation]
	if stamp == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return false
	}
	return time.Since(t) < bootstrapTokenRefreshAge
}

// mintBootstrapTokenViaExec runs `tokensmith bootstrap-token create`
// inside a Ready tokensmith pod, captures stdout (the JSON
// description of the new token, per --output-format json), parses out
// the opaque token string and returns it.
//
// Implementation notes:
//   - The CLI writes to the bootstrap store atomically; concurrent
//     reconciles minting at the same time would each get their own
//     valid token. We tolerate the dup write — the most recent token
//     is what we land in the Secret.
//   - The bootstrap-store path is passed explicitly so the result is
//     reproducible regardless of which tokensmith pod we land in.
func (r *TokensmithReconciler) mintBootstrapTokenViaExec(
	ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane, spec bootstrapTokenSpec,
) (string, error) {
	pod, err := r.pickTokensmithPod(ctx, cp)
	if err != nil {
		return "", err
	}

	args := []string{
		"/usr/local/bin/tokensmith", "bootstrap-token", "create",
		"--subject", spec.subject,
		"--audience", bootstrapTokenAudience,
		"--scopes", spec.scopes,
		"--ttl", bootstrapTokenTTL,
		"--refresh-ttl", bootstrapTokenRefreshTTL,
		"--output-format", "json",
		"--bootstrap-store", bootstrapTokenStorePath,
	}

	stdout, stderr, err := r.execInPod(ctx, ControlPlaneNamespace(cp), pod, args)
	if err != nil {
		return "", fmt.Errorf("exec %q in tokensmith pod %s: %w (stderr=%q)",
			args, pod, err, stderr)
	}

	var result bootstrapTokenCreateResult
	// The CLI's JSON output is the last well-formed JSON object on
	// stdout. Some tokensmith builds prepend banner text; skip past
	// anything before the first '{' to be robust to that.
	jsonStart := bytes.IndexByte(stdout, '{')
	if jsonStart < 0 {
		return "", fmt.Errorf("no JSON in tokensmith bootstrap-token create output: %q", string(stdout))
	}
	if err := json.Unmarshal(stdout[jsonStart:], &result); err != nil {
		return "", fmt.Errorf("parsing bootstrap-token create JSON: %w (raw=%q)", err, string(stdout))
	}
	if result.BootstrapToken == "" {
		return "", fmt.Errorf("bootstrap-token create returned empty bootstrap_token field: %q", string(stdout))
	}
	return result.BootstrapToken, nil
}

// pickTokensmithPod returns the name of a Running tokensmith pod, or
// an error if none are available. The bootstrap-token CLI persists
// its writes to the PVC tokensmith mounts, so any Running pod will
// see them after the call returns.
func (r *TokensmithReconciler) pickTokensmithPod(
	ctx context.Context, cp *openchamiv1alpha1.OpenCHAMIControlPlane,
) (string, error) {
	pods := &corev1.PodList{}
	if err := r.Client.List(ctx, pods,
		client.InNamespace(ControlPlaneNamespace(cp)),
		client.MatchingLabels{labelAppName: ServiceTokensmith}); err != nil {
		return "", fmt.Errorf("listing tokensmith pods: %w", err)
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("no Running tokensmith pod found in %s", ControlPlaneNamespace(cp))
}

// execInPod runs cmd inside namespace/pod/ServiceTokensmith container,
// captures stdout and stderr separately, and returns them. The
// remotecommand API streams; we buffer to memory because tokensmith's
// output is small (a few hundred bytes).
func (r *TokensmithReconciler) execInPod(
	ctx context.Context, namespace, pod string, cmd []string,
) (stdout, stderr []byte, err error) {
	cs, err := kubernetes.NewForConfig(r.RESTConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("building kubernetes clientset: %w", err)
	}
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: ServiceTokensmith,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(r.RESTConfig, "POST", req.URL())
	if err != nil {
		return nil, nil, fmt.Errorf("building executor: %w", err)
	}
	var outBuf, errBuf bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &outBuf,
		Stderr: &errBuf,
	}); err != nil {
		return outBuf.Bytes(), errBuf.Bytes(), fmt.Errorf("streaming exec: %w", err)
	}
	return outBuf.Bytes(), errBuf.Bytes(), nil
}
