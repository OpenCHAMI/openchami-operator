// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
)

const testNamespace = "default"

var _ = Describe("OpenCHAMICluster Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: testNamespace, // TODO(user):Modify as needed
		}
		openchamicluster := &openchamiv1alpha1.OpenCHAMICluster{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind OpenCHAMICluster")
			err := k8sClient.Get(ctx, typeNamespacedName, openchamicluster)
			if err != nil && errors.IsNotFound(err) {
				resource := &openchamiv1alpha1.OpenCHAMICluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: testNamespace,
					},
					Spec: openchamiv1alpha1.OpenCHAMIClusterSpec{
						ClusterName: resourceName,
						Domain:      "test.local",
						Platform: openchamiv1alpha1.PlatformSpec{
							Vault: openchamiv1alpha1.VaultSpec{
								Address: "http://vault.test:8200",
							},
							ObjectStorage: openchamiv1alpha1.ObjectStorageSpec{
								Endpoint: "http://s3.test:9000",
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &openchamiv1alpha1.OpenCHAMICluster{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance OpenCHAMICluster")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &OpenCHAMIClusterReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(100),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the finalizer was added")
			updated := &openchamiv1alpha1.OpenCHAMICluster{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement("openchami.org/cluster-protection"))
		})
	})

	Context("When a cluster is pinned to a different operator version", func() {
		const pinnedName = "pinned-resource"
		pinnedNamespacedName := types.NamespacedName{Name: pinnedName, Namespace: testNamespace}

		BeforeEach(func() {
			resource := &openchamiv1alpha1.OpenCHAMICluster{
				ObjectMeta: metav1.ObjectMeta{Name: pinnedName, Namespace: testNamespace},
				Spec: openchamiv1alpha1.OpenCHAMIClusterSpec{
					ClusterName:     pinnedName,
					Domain:          "pinned.test.local",
					OperatorChannel: "pinned",
					PinnedVersion:   "0.0.0-mismatch",
					Platform: openchamiv1alpha1.PlatformSpec{
						Vault: openchamiv1alpha1.VaultSpec{Address: "http://vault.test:8200"},
						ObjectStorage: openchamiv1alpha1.ObjectStorageSpec{
							Endpoint: "http://s3.test:9000",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(func() {
			resource := &openchamiv1alpha1.OpenCHAMICluster{}
			if err := k8sClient.Get(ctx, pinnedNamespacedName, resource); err == nil {
				resource.Finalizers = nil
				_ = k8sClient.Update(ctx, resource)
				_ = k8sClient.Delete(ctx, resource)
			}
		})

		It("suspends reconciliation and creates no Deployments", func() {
			r := &OpenCHAMIClusterReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(100),
			}

			By("first reconcile adds the finalizer")
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: pinnedNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("second reconcile hits the version pin and short-circuits")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: pinnedNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("ConditionReconcileActive=False with reason VersionPinned")
			updated := &openchamiv1alpha1.OpenCHAMICluster{}
			Expect(k8sClient.Get(ctx, pinnedNamespacedName, updated)).To(Succeed())
			cond := apimeta.FindStatusCondition(updated.Status.Conditions, conditions.ConditionReconcileActive)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(conditions.ReasonVersionPinned))

			By("no Deployments exist in the cluster namespace")
			deps := &appsv1.DeploymentList{}
			Expect(k8sClient.List(ctx, deps, &client.ListOptions{
				Namespace: "openchami-" + pinnedName,
			})).To(Succeed())
			Expect(deps.Items).To(BeEmpty())
		})
	})
})
