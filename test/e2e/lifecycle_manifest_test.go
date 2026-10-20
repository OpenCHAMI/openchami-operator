//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestLifecycleClusterYAML(t *testing.T) {
	for _, pinnedVersion := range []string{"", "v1.2.3"} {
		t.Run("pinnedVersion="+pinnedVersion, func(t *testing.T) {
			manifest := lifecycleClusterYAML("venado", lifecycleDevVaultAddrDefault, "venado", "stable", pinnedVersion)
			if strings.Contains(manifest, "\t") {
				t.Fatal("cluster manifest contains a tab")
			}

			var cluster struct {
				Spec struct {
					Services struct {
						CoreDHCP struct {
							Enabled      bool              `json:"enabled"`
							NodeSelector map[string]string `json:"nodeSelector"`
						} `json:"coreDHCP"`
					} `json:"services"`
					PinnedVersion string `json:"pinnedVersion"`
				} `json:"spec"`
			}
			if err := yaml.Unmarshal([]byte(manifest), &cluster); err != nil {
				t.Fatalf("parse cluster manifest: %v", err)
			}
			if !cluster.Spec.Services.CoreDHCP.Enabled {
				t.Error("spec.services.coreDHCP.enabled must be true")
			}
			if cluster.Spec.Services.CoreDHCP.NodeSelector["openchami.org/dhcp-venado"] != "true" {
				t.Error("spec.services.coreDHCP.nodeSelector is missing the cluster discriminator")
			}
			if cluster.Spec.PinnedVersion != pinnedVersion {
				t.Errorf("pinnedVersion = %q, want %q", cluster.Spec.PinnedVersion, pinnedVersion)
			}
		})
	}
}
