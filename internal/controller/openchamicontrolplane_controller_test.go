// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
	"github.com/openchami/openchami-operator/internal/conditions"
	"github.com/openchami/openchami-operator/internal/reconcilers"
)

const testNamespace = "default"

var _ = Describe("OpenCHAMIControlPlane Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: testNamespace, // TODO(user):Modify as needed
		}
		openchamicontrolplane := &openchamiv1alpha1.OpenCHAMIControlPlane{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind OpenCHAMIControlPlane")
			err := k8sClient.Get(ctx, typeNamespacedName, openchamicontrolplane)
			if err != nil && errors.IsNotFound(err) {
				resource := &openchamiv1alpha1.OpenCHAMIControlPlane{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: testNamespace,
					},
					Spec: openchamiv1alpha1.OpenCHAMIControlPlaneSpec{
						ClusterName: resourceName,
						Domain:      "test.local",
						Platform: openchamiv1alpha1.PlatformSpec{
							Vault: openchamiv1alpha1.VaultSpec{
								Address: testVaultAddr,
							},
							ObjectStorage: openchamiv1alpha1.ObjectStorageSpec{
								Endpoint: testS3Endpoint,
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &openchamiv1alpha1.OpenCHAMIControlPlane{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance OpenCHAMIControlPlane")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &OpenCHAMIControlPlaneReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(100),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the finalizer was added")
			updated := &openchamiv1alpha1.OpenCHAMIControlPlane{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement("openchami.org/cluster-protection"))
		})
	})

	Context("When a cluster is pinned to a different operator version", func() {
		const pinnedName = "pinned-resource"
		pinnedNamespacedName := types.NamespacedName{Name: pinnedName, Namespace: testNamespace}

		BeforeEach(func() {
			resource := &openchamiv1alpha1.OpenCHAMIControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: pinnedName, Namespace: testNamespace},
				Spec: openchamiv1alpha1.OpenCHAMIControlPlaneSpec{
					ClusterName:     pinnedName,
					Domain:          "pinned.test.local",
					OperatorChannel: "pinned",
					PinnedVersion:   "0.0.0-mismatch",
					Platform: openchamiv1alpha1.PlatformSpec{
						Vault: openchamiv1alpha1.VaultSpec{Address: testVaultAddr},
						ObjectStorage: openchamiv1alpha1.ObjectStorageSpec{
							Endpoint: testS3Endpoint,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(func() {
			resource := &openchamiv1alpha1.OpenCHAMIControlPlane{}
			if err := k8sClient.Get(ctx, pinnedNamespacedName, resource); err == nil {
				resource.Finalizers = nil
				_ = k8sClient.Update(ctx, resource)
				_ = k8sClient.Delete(ctx, resource)
			}
		})

		It("suspends reconciliation and creates no Deployments", func() {
			r := &OpenCHAMIControlPlaneReconciler{
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
			updated := &openchamiv1alpha1.OpenCHAMIControlPlane{}
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

var _ = Describe("Magellan legacy schedule suspension", func() {
	It("preserves legacy SSA-owned fields and Jobs while stopping future runs", func() {
		const namespace = "openchami-magellan-upgrade"
		ns := &corev1.Namespace{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		}
		Expect(k8sClient.Patch(ctx, ns, client.Apply, client.FieldOwner("openchami-operator"))).To(Succeed()) //nolint:staticcheck // Seed legacy SSA ownership.
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, ns)).To(Succeed()) })
		cp := &openchamiv1alpha1.OpenCHAMIControlPlane{}
		cp.Spec.ClusterName = "magellan-upgrade"
		cp.Spec.Services.Magellan.Enabled = true
		cj := &batchv1.CronJob{
			TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
			ObjectMeta: metav1.ObjectMeta{Name: reconcilers.ServiceMagellan, Namespace: namespace,
				Labels: map[string]string{
					"app.kubernetes.io/name":     reconcilers.ServiceMagellan,
					"app.kubernetes.io/instance": namespace,
					"openchami.org/managed-by":   "operator",
				},
				Annotations: map[string]string{"openchami.org/probe-original-suspend": "false"},
			},
			Spec: batchv1.CronJobSpec{
				Schedule: "*/30 * * * *", ConcurrencyPolicy: batchv1.ForbidConcurrent, Suspend: new(false),
				JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyOnFailure,
						Containers:    []corev1.Container{{Name: reconcilers.ServiceMagellan, Image: "ghcr.io/openchami/magellan:v0.5.1"}},
					},
				}}},
			},
		}
		Expect(k8sClient.Patch(ctx, cj, client.Apply, client.FieldOwner("openchami-operator"))).To(Succeed()) //nolint:staticcheck // Seed legacy SSA ownership.
		key := client.ObjectKeyFromObject(cj)
		before := &batchv1.CronJob{}
		Expect(k8sClient.Get(ctx, key, before)).To(Succeed())
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "magellan-previous", Namespace: namespace},
			Spec:       before.Spec.JobTemplate.Spec,
		}
		Expect(k8sClient.Create(ctx, job)).To(Succeed())
		jobBefore := job.DeepCopy()
		r := &reconcilers.MagellanReconciler{Client: k8sClient}
		for range 2 {
			_, err := r.Reconcile(ctx, cp)
			Expect(err).NotTo(HaveOccurred())
			after := &batchv1.CronJob{}
			Expect(k8sClient.Get(ctx, key, after)).To(Succeed())
			expected := before.Spec.DeepCopy()
			expected.Suspend = new(true)
			Expect(after.Spec).To(Equal(*expected))
			Expect(after.Annotations).To(Equal(before.Annotations))
		}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(job), job)).To(Succeed())
		Expect(job.Spec).To(Equal(jobBefore.Spec))
		Expect(job.Status).To(Equal(jobBefore.Status))
	})
})

// Exercise generated CRD defaults and validation without admission webhooks.
var _ = Describe("TokenSmith claim policy CRD", func() {
	It("defaults omitted policies, persists supported changes, and rejects unknown policies", func() {
		cp := &openchamiv1alpha1.OpenCHAMIControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "claim-policy", Namespace: testNamespace},
			Spec: openchamiv1alpha1.OpenCHAMIControlPlaneSpec{
				ClusterName: "claim-policy",
				Domain:      "claim-policy.test.local",
				Platform: openchamiv1alpha1.PlatformSpec{
					Vault:         openchamiv1alpha1.VaultSpec{Address: testVaultAddr},
					ObjectStorage: openchamiv1alpha1.ObjectStorageSpec{Endpoint: testS3Endpoint},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cp)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, cp)).To(Succeed()) })
		Expect(cp.Spec.Services.Tokensmith.ClaimPolicy).To(Equal(openchamiv1alpha1.TokensmithClaimPolicyEnriched))

		before := cp.DeepCopy()
		cp.Spec.Services.Tokensmith.ClaimPolicy = openchamiv1alpha1.TokensmithClaimPolicyCSMKeycloak
		Expect(k8sClient.Patch(ctx, cp, client.MergeFrom(before))).To(Succeed())
		stored := &openchamiv1alpha1.OpenCHAMIControlPlane{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cp), stored)).To(Succeed())
		Expect(stored.Spec.Services.Tokensmith.ClaimPolicy).To(Equal(openchamiv1alpha1.TokensmithClaimPolicyCSMKeycloak))

		before = stored.DeepCopy()
		stored.Spec.Services.Tokensmith.ClaimPolicy = "unknown"
		err := k8sClient.Patch(ctx, stored, client.MergeFrom(before))
		Expect(errors.IsInvalid(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("claimPolicy"))

		invalid := before.DeepCopy()
		invalid.ObjectMeta = metav1.ObjectMeta{Name: "claim-policy-invalid", Namespace: testNamespace}
		invalid.Spec.ClusterName = invalid.Name
		invalid.Spec.Services.Tokensmith.ClaimPolicy = "unknown"
		err = k8sClient.Create(ctx, invalid)
		if err == nil {
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, invalid)).To(Succeed()) })
		}
		Expect(errors.IsInvalid(err)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring("claimPolicy"))
	})
})
