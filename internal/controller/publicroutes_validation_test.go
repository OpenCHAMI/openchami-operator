// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

const (
	testVaultAddr  = "http://vault.test:8200"
	testS3Endpoint = "http://s3.test:9000"
)

// These specs exercise the CRD-level (OpenAPI pattern + CEL) validation on
// spec.services.{smd,bootService}.publicRoutes against a real API server.
// The gateway reconciler also filters defensively, but admission is the
// user-facing contract: a public (unauthenticated) path outside the
// service's prefix, or one with path-traversal segments, must be rejected
// outright rather than silently ignored.
var _ = Describe("publicRoutes CRD validation", func() {
	ctx := context.Background()
	n := 0

	newCP := func(smdPaths, bootPaths []string) *openchamiv1alpha1.OpenCHAMIControlPlane {
		n++
		name := fmt.Sprintf("publicroutes-%d", n)
		cp := &openchamiv1alpha1.OpenCHAMIControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: openchamiv1alpha1.OpenCHAMIControlPlaneSpec{
				ClusterName: name,
				Domain:      name + ".test.local",
				Platform: openchamiv1alpha1.PlatformSpec{
					Vault:         openchamiv1alpha1.VaultSpec{Address: testVaultAddr},
					ObjectStorage: openchamiv1alpha1.ObjectStorageSpec{Endpoint: testS3Endpoint},
				},
			},
		}
		cp.Spec.Services.SMD.PublicRoutes.Paths = smdPaths
		cp.Spec.Services.BootService.PublicRoutes.Paths = bootPaths
		return cp
	}

	cleanup := func(cp *openchamiv1alpha1.OpenCHAMIControlPlane) {
		_ = k8sClient.Delete(ctx, cp)
	}

	It("accepts in-prefix exact paths and defaults enabled=true", func() {
		cp := newCP([]string{"/hsm/v2/State/Components"}, []string{"/boot/v1/bootscript"})
		Expect(k8sClient.Create(ctx, cp)).To(Succeed())
		DeferCleanup(cleanup, cp)
		Expect(cp.Spec.Services.SMD.PublicRoutes.Enabled).NotTo(BeNil())
		Expect(*cp.Spec.Services.SMD.PublicRoutes.Enabled).To(BeTrue())
	})

	DescribeTable("rejects invalid paths",
		func(smdPaths, bootPaths []string, wantMsg string) {
			cp := newCP(smdPaths, bootPaths)
			err := k8sClient.Create(ctx, cp)
			if err == nil {
				DeferCleanup(cleanup, cp)
			}
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(wantMsg))
		},
		Entry("smd path outside /hsm/", []string{"/boot/v1/bootscript"}, nil, "must start with /hsm/"),
		Entry("boot path outside /boot/", nil, []string{"/hsm/v2/State/Components"}, "must start with /boot/"),
		Entry("bare prefix", []string{"/hsm"}, nil, "must start with /hsm/"),
		Entry("dot-dot traversal", []string{"/hsm/../boot/v1/bootscript"}, nil, "must be normalized"),
		Entry("empty segment", []string{"/hsm//v2"}, nil, "must be normalized"),
		Entry("wildcard", []string{"/hsm/v2/*"}, nil, "should match"),
		Entry("query string", []string{"/hsm/v2/State/Components?type=Node"}, nil, "should match"),
	)
})
