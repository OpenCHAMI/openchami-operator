// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package admin

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

// init.go-scoped constants. Prefixed with `init` to avoid collision with the
// other `internal/admin` files written by parallel sub-agents.
const (
	initClusterNamePattern    = `^[a-z][a-z0-9-]{0,31}$`
	initDefaultNamespace      = "default"
	initDefaultVaultAuth      = "kubernetes"
	initVaultAuthAppRole      = "appRole"
	initAPIVersion            = "openchami.openchami.org/v1alpha1"
	initKind                  = "OpenCHAMIControlPlane"
	initStdoutSentinel        = "-"
	initDHCPClusterLabel      = "openchami.org/dhcp-cluster"
	initOIDCProviderVault     = "vault"
	initVaultSchemeHTTPS      = "https://"
	initVaultSchemeHTTPPrefix = "http://"
)

var initClusterNameRegexp = regexp.MustCompile(initClusterNamePattern)

// initOpts holds every flag value for the `init` sub-command. Kept
// unexported so it cannot collide with peer files.
type initOpts struct {
	clusterName string
	domain      string

	vaultAddr          string
	vaultAuth          string
	allowInsecureVault bool

	s3Endpoint string

	provisionSubnet       string
	provisionValidateHost string
	provisionValidatePort int32

	bmcSubnet       string
	bmcValidateHost string
	bmcValidatePort int32

	dhcpRange string

	skipChecks bool

	output string
}

// InitCmd returns the `ochami-admin init` command. The command emits a
// ready-to-apply OpenCHAMIControlPlane YAML to stdout (or `--output` file) without
// contacting Kubernetes.
func InitCmd() *cobra.Command {
	o := &initOpts{}
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Generate a ready-to-apply OpenCHAMIControlPlane manifest",
		Long: "Generate a ready-to-apply OpenCHAMIControlPlane manifest from flags. " +
			"Does not contact Kubernetes. The output may be piped to `kubectl apply -f -` " +
			"or written to a file with --output.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	o.bindFlags(cmd.Flags())
	return cmd
}

// bindFlags wires flag values into o. Flag names follow the spec in
// docs/phases/phase-14-cli.md.
func (o *initOpts) bindFlags(f *pflag.FlagSet) {
	f.StringVar(&o.clusterName, "cluster-name", "", "Cluster name (required, lowercase letters/digits/hyphens, must start with a letter, max 32 chars)")
	f.StringVar(&o.domain, "domain", "", "External FQDN for the cluster gateway (required)")

	f.StringVar(&o.vaultAddr, "vault-addr", "", "Vault address (required, https:// unless --allow-insecure-vault is set)")
	f.StringVar(&o.vaultAuth, "vault-auth", initDefaultVaultAuth, "Vault auth method (kubernetes|appRole)")
	f.BoolVar(&o.allowInsecureVault, "allow-insecure-vault", false, "Allow http:// vault address (dev/test only; the validating webhook still enforces https://)")

	f.StringVar(&o.s3Endpoint, "s3-endpoint", "", "VersityGW S3 endpoint URL (required)")

	f.StringVar(&o.provisionSubnet, "provision-subnet", "", "CIDR for the provision/PXE network probe; enables networkProbe when set")
	f.StringVar(&o.provisionValidateHost, "provision-validate-host", "", "Host for the provision-network reachability check")
	f.Int32Var(&o.provisionValidatePort, "provision-validate-port", 0, "TCP port for the provision-network reachability check (0 = unset)")

	f.StringVar(&o.bmcSubnet, "bmc-subnet", "", "CIDR for the BMC network probe")
	f.StringVar(&o.bmcValidateHost, "bmc-validate-host", "", "Host for the BMC-network reachability check")
	f.Int32Var(&o.bmcValidatePort, "bmc-validate-port", 0, "TCP port for the BMC-network reachability check (0 = unset)")

	f.StringVar(&o.dhcpRange, "dhcp-range", "", `DHCP lease range "start-end" e.g. "192.168.1.100-192.168.1.200"`)

	f.BoolVar(&o.skipChecks, "skip-checks", false, "Skip pre-flight connectivity checks (advisory only at this stage)")

	f.StringVarP(&o.output, "output", "o", initStdoutSentinel, "Write manifest to a file path (default '-' = stdout)")
}

// run validates flags, builds the cluster object, marshals it, and writes
// the result to either stdout or the configured output file.
func (o *initOpts) run(stdout, stderr io.Writer) error {
	if err := o.validate(); err != nil {
		return err
	}

	if o.allowInsecureVault && strings.HasPrefix(o.vaultAddr, initVaultSchemeHTTPPrefix) {
		_, _ = fmt.Fprintln(stderr, "warning: --allow-insecure-vault is set; the operator's validating webhook will still reject this manifest.")
	}
	if o.skipChecks {
		_, _ = fmt.Fprintln(stderr, "note: --skip-checks is set; pre-flight connectivity validation is not performed.")
	}

	cp := o.buildCluster()
	out, err := yaml.Marshal(cp)
	if err != nil {
		return fmt.Errorf("marshalling cluster manifest: %w", err)
	}

	if o.output == initStdoutSentinel {
		if _, err := stdout.Write(out); err != nil {
			return fmt.Errorf("writing manifest to stdout: %w", err)
		}
	} else {
		if err := os.WriteFile(o.output, out, 0o644); err != nil {
			return fmt.Errorf("writing manifest to %s: %w", o.output, err)
		}
	}

	o.printNextSteps(stderr)
	return nil
}

// validate enforces the documented flag constraints. It is exposed via run()
// rather than as a cobra PreRunE so unit tests can call it on populated opts.
func (o *initOpts) validate() error {
	if o.clusterName == "" {
		return fmt.Errorf("--cluster-name is required")
	}
	if !initClusterNameRegexp.MatchString(o.clusterName) {
		return fmt.Errorf("--cluster-name %q is invalid; must match %s", o.clusterName, initClusterNamePattern)
	}

	if o.domain == "" {
		return fmt.Errorf("--domain is required")
	}
	if !strings.Contains(o.domain, ".") {
		return fmt.Errorf("--domain %q is invalid; must contain a dot", o.domain)
	}
	if u, err := url.Parse(o.domain); err == nil && u.Scheme != "" {
		return fmt.Errorf("--domain %q must be a bare FQDN, not a URL with a scheme", o.domain)
	}
	if strings.ContainsAny(o.domain, "/:") {
		return fmt.Errorf("--domain %q must be a bare FQDN, not a URL with a path or port", o.domain)
	}

	if o.vaultAddr == "" {
		return fmt.Errorf("--vault-addr is required")
	}
	if !strings.HasPrefix(o.vaultAddr, initVaultSchemeHTTPS) {
		if !o.allowInsecureVault || !strings.HasPrefix(o.vaultAddr, initVaultSchemeHTTPPrefix) {
			return fmt.Errorf("--vault-addr %q must start with https:// (or pass --allow-insecure-vault to allow http://)", o.vaultAddr)
		}
	}

	switch o.vaultAuth {
	case initDefaultVaultAuth, initVaultAuthAppRole:
	default:
		return fmt.Errorf("--vault-auth %q is invalid; must be one of: kubernetes, appRole", o.vaultAuth)
	}

	if o.s3Endpoint == "" {
		return fmt.Errorf("--s3-endpoint is required")
	}

	if o.provisionSubnet != "" {
		if _, _, err := net.ParseCIDR(o.provisionSubnet); err != nil {
			return fmt.Errorf("--provision-subnet %q is not a valid CIDR: %w", o.provisionSubnet, err)
		}
	}
	if o.bmcSubnet != "" {
		if _, _, err := net.ParseCIDR(o.bmcSubnet); err != nil {
			return fmt.Errorf("--bmc-subnet %q is not a valid CIDR: %w", o.bmcSubnet, err)
		}
	}

	if o.dhcpRange != "" {
		if _, _, err := initSplitDHCPRange(o.dhcpRange); err != nil {
			return err
		}
	}

	return nil
}

// buildCluster materialises the OpenCHAMIControlPlane object. Empty/zero fields
// are dropped at marshal time by the JSON `omitempty` tags so they don't
// clobber kubebuilder defaults.
func (o *initOpts) buildCluster() *openchamiv1alpha1.OpenCHAMIControlPlane {
	probeEnabled := o.provisionSubnet != "" || o.bmcSubnet != ""
	dhcpEnabled := o.provisionSubnet != "" || o.dhcpRange != ""
	magellanEnabled := o.bmcSubnet != ""

	c := &openchamiv1alpha1.OpenCHAMIControlPlane{
		TypeMeta: metav1.TypeMeta{
			APIVersion: initAPIVersion,
			Kind:       initKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      o.clusterName,
			Namespace: initDefaultNamespace,
		},
		Spec: openchamiv1alpha1.OpenCHAMIControlPlaneSpec{
			ClusterName: o.clusterName,
			Domain:      o.domain,
			Platform: openchamiv1alpha1.PlatformSpec{
				Vault: openchamiv1alpha1.VaultSpec{
					Address:    o.vaultAddr,
					AuthMethod: openchamiv1alpha1.VaultAuthMethod(o.vaultAuth),
				},
				ObjectStorage: openchamiv1alpha1.ObjectStorageSpec{
					Endpoint: o.s3Endpoint,
				},
			},
			Services: openchamiv1alpha1.ServicesSpec{
				SMD: openchamiv1alpha1.SMDSpec{
					ServiceDefaults: openchamiv1alpha1.ServiceDefaults{Enabled: true},
				},
				Tokensmith: openchamiv1alpha1.TokensmithSpec{
					ServiceDefaults: openchamiv1alpha1.ServiceDefaults{Enabled: true},
					OIDCProvider:    initOIDCProviderVault,
				},
				BootService: openchamiv1alpha1.BootServiceSpec{
					ServiceDefaults: openchamiv1alpha1.ServiceDefaults{Enabled: true},
				},
				MetadataService: openchamiv1alpha1.MetadataServiceSpec{
					ServiceDefaults: openchamiv1alpha1.ServiceDefaults{Enabled: true},
				},
				CoreDHCP: openchamiv1alpha1.CoreDHCPSpec{
					Enabled: dhcpEnabled,
				},
				Magellan: openchamiv1alpha1.MagellanSpec{
					Enabled: magellanEnabled,
				},
			},
		},
	}

	// AppRole auth: a Secret containing role_id/secret_id must be created
	// out-of-band (invariant #7: no secrets in the spec). The webhook
	// requires AppRoleSecretRef; we pre-populate it with a conventional
	// name so the manifest applies cleanly once the Secret exists.
	if o.vaultAuth == initVaultAuthAppRole {
		c.Spec.Platform.Vault.AppRoleSecretRef = &corev1.LocalObjectReference{
			Name: o.clusterName + "-vault-approle",
		}
	}

	// Network probe wiring.
	if probeEnabled {
		c.Spec.NetworkProbe.Enabled = true
		if o.provisionSubnet != "" {
			c.Spec.NetworkProbe.ProvisionNetwork = &openchamiv1alpha1.NetworkProbeTarget{
				Subnet:       o.provisionSubnet,
				ValidateHost: o.provisionValidateHost,
				ValidatePort: o.provisionValidatePort,
			}
		}
		if o.bmcSubnet != "" {
			c.Spec.NetworkProbe.BMCNetwork = &openchamiv1alpha1.NetworkProbeTarget{
				Subnet:       o.bmcSubnet,
				ValidateHost: o.bmcValidateHost,
				ValidatePort: o.bmcValidatePort,
			}
		}
	} else {
		// Probing off → coreDHCP.nodeSelector required AND must contain a
		// discriminator with the cluster name (webhook rules 5 + 6).
		c.Spec.Services.CoreDHCP.NodeSelector = map[string]string{
			initDHCPClusterLabel: o.clusterName,
		}
		if magellanEnabled {
			// Magellan is currently disabled when --bmc-subnet is empty,
			// but if a future flag enables it without probing we mirror
			// the same discriminator-style label.
			c.Spec.Services.Magellan.NodeSelector = map[string]string{
				initDHCPClusterLabel: o.clusterName,
			}
		}
	}

	// DHCP lease range.
	if o.dhcpRange != "" {
		start, end, _ := initSplitDHCPRange(o.dhcpRange) // already validated
		// Each DHCPLeaseRange requires Subnet+Start+End. We only have a
		// start/end pair here; reuse the provision subnet when present,
		// otherwise leave Subnet empty and let the user fill it in or rely
		// on a future spec evolution.
		subnet := o.provisionSubnet
		c.Spec.Services.CoreDHCP.LeaseRanges = []openchamiv1alpha1.DHCPLeaseRange{
			{
				Subnet: subnet,
				Start:  start,
				End:    end,
			},
		}
	}

	// BMC subnet for Magellan.
	if magellanEnabled {
		c.Spec.Services.Magellan.BMCSubnet = o.bmcSubnet
	}

	return c
}

// printNextSteps writes the post-output usage block to stderr.
func (o *initOpts) printNextSteps(stderr io.Writer) {
	target := o.output
	if target == initStdoutSentinel {
		target = "<output>"
	}
	_, _ = fmt.Fprintf(stderr, "Wrote OpenCHAMIControlPlane manifest for %q.\n", o.clusterName)
	_, _ = fmt.Fprintln(stderr, "Next steps:")
	_, _ = fmt.Fprintf(stderr, "  1. Verify the spec: cat %s | yq\n", target)
	_, _ = fmt.Fprintf(stderr, "  2. Apply: kubectl apply -f %s\n", target)
	_, _ = fmt.Fprintf(stderr, "  3. Watch: kubectl get openchamicontrolplane %s -w\n", o.clusterName)
}

// initSplitDHCPRange parses "start-end" into (start, end). Both halves must
// parse as IP addresses.
func initSplitDHCPRange(s string) (string, string, error) {
	idx := strings.Index(s, "-")
	if idx <= 0 || idx == len(s)-1 {
		return "", "", fmt.Errorf("--dhcp-range %q must be of the form start-end", s)
	}
	start := strings.TrimSpace(s[:idx])
	end := strings.TrimSpace(s[idx+1:])
	if net.ParseIP(start) == nil {
		return "", "", fmt.Errorf("--dhcp-range start %q is not a valid IP address", start)
	}
	if net.ParseIP(end) == nil {
		return "", "", fmt.Errorf("--dhcp-range end %q is not a valid IP address", end)
	}
	return start, end, nil
}
