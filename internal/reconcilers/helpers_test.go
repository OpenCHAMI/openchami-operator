// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package reconcilers

import (
	"testing"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

const testClusterNameAlpha = "alpha"

// TestServiceURL_HonoursExternalEndpoint confirms ServiceURL returns the
// site-supplied externalEndpoint verbatim when set, and the in-cluster
// Service URL otherwise. This is the single hook used by every consumer
// reconciler to wire upstream URLs into env vars; if it stops honouring
// the override, every per-service test below would still pass while
// production breaks.
func TestServiceURL_HonoursExternalEndpoint(t *testing.T) {
	external := "https://smd.platform.example.com"

	cases := []struct {
		name string
		spec openchamiv1alpha1.OpenCHAMICluster
		svc  string
		want string
	}{
		{
			name: "default — in-cluster URL for SMD",
			spec: testCluster(openchamiv1alpha1.ServicesSpec{
				SMD: openchamiv1alpha1.SMDSpec{ServiceDefaults: openchamiv1alpha1.ServiceDefaults{Enabled: true}},
			}),
			svc:  ServiceSMD,
			want: "http://smd.openchami-alpha.svc.cluster.local:27779",
		},
		{
			name: "externalEndpoint set — passthrough",
			spec: testCluster(openchamiv1alpha1.ServicesSpec{
				SMD: openchamiv1alpha1.SMDSpec{ServiceDefaults: openchamiv1alpha1.ServiceDefaults{
					Enabled: false, ExternalEndpoint: &external,
				}},
			}),
			svc:  ServiceSMD,
			want: external,
		},
		{
			name: "tokensmith default — in-cluster URL",
			spec: testCluster(openchamiv1alpha1.ServicesSpec{
				Tokensmith: openchamiv1alpha1.TokensmithSpec{ServiceDefaults: openchamiv1alpha1.ServiceDefaults{Enabled: true}},
			}),
			svc:  ServiceTokensmith,
			want: "http://tokensmith.openchami-alpha.svc.cluster.local:8080",
		},
		{
			name: "boot-service default — in-cluster URL",
			spec: testCluster(openchamiv1alpha1.ServicesSpec{
				BootService: openchamiv1alpha1.BootServiceSpec{ServiceDefaults: openchamiv1alpha1.ServiceDefaults{Enabled: true}},
			}),
			svc:  ServiceBootService,
			want: "http://boot-service.openchami-alpha.svc.cluster.local:27778",
		},
		{
			name: "metadata-service default — in-cluster URL",
			spec: testCluster(openchamiv1alpha1.ServicesSpec{
				MetadataService: openchamiv1alpha1.MetadataServiceSpec{ServiceDefaults: openchamiv1alpha1.ServiceDefaults{Enabled: true}},
			}),
			svc:  ServiceMetadataService,
			want: "http://metadata-service.openchami-alpha.svc.cluster.local:27770",
		},
		{
			name: "unknown service — empty string",
			spec: testCluster(openchamiv1alpha1.ServicesSpec{}),
			svc:  "no-such-service",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ServiceURL(&tc.spec, tc.svc)
			if got != tc.want {
				t.Errorf("ServiceURL(%q) = %q, want %q", tc.svc, got, tc.want)
			}
		})
	}
}

// TestServiceDeployedInCluster maps the four supported services through
// each combination of Enabled / ExternalEndpoint to confirm the operator
// only deploys in-cluster objects when both Enabled=true and no external
// endpoint is set.
func TestServiceDeployedInCluster(t *testing.T) {
	external := "https://example.com"
	cases := []struct {
		name    string
		enabled bool
		ext     *string
		want    bool
	}{
		{"enabled, no external — deploy", true, nil, true},
		{"enabled, external set — do not deploy (external wins)", true, &external, false},
		{"disabled, no external — do not deploy", false, nil, false},
		{"disabled, external set — do not deploy", false, &external, false},
	}
	for _, svc := range []string{ServiceSMD, ServiceTokensmith, ServiceBootService, ServiceMetadataService} {
		for _, tc := range cases {
			t.Run(svc+"/"+tc.name, func(t *testing.T) {
				cluster := testCluster(servicesSpecFor(svc, tc.enabled, tc.ext))
				if got := ServiceDeployedInCluster(&cluster, svc); got != tc.want {
					t.Errorf("ServiceDeployedInCluster(%s) = %v, want %v", svc, got, tc.want)
				}
			})
		}
	}
}

func testCluster(services openchamiv1alpha1.ServicesSpec) openchamiv1alpha1.OpenCHAMICluster {
	return openchamiv1alpha1.OpenCHAMICluster{
		Spec: openchamiv1alpha1.OpenCHAMIClusterSpec{
			ClusterName: testClusterNameAlpha,
			Services:    services,
		},
	}
}

func servicesSpecFor(svc string, enabled bool, ext *string) openchamiv1alpha1.ServicesSpec {
	d := openchamiv1alpha1.ServiceDefaults{Enabled: enabled, ExternalEndpoint: ext}
	switch svc {
	case ServiceSMD:
		return openchamiv1alpha1.ServicesSpec{SMD: openchamiv1alpha1.SMDSpec{ServiceDefaults: d}}
	case ServiceTokensmith:
		return openchamiv1alpha1.ServicesSpec{Tokensmith: openchamiv1alpha1.TokensmithSpec{ServiceDefaults: d}}
	case ServiceBootService:
		return openchamiv1alpha1.ServicesSpec{BootService: openchamiv1alpha1.BootServiceSpec{ServiceDefaults: d}}
	case ServiceMetadataService:
		return openchamiv1alpha1.ServicesSpec{MetadataService: openchamiv1alpha1.MetadataServiceSpec{ServiceDefaults: d}}
	}
	return openchamiv1alpha1.ServicesSpec{}
}
