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

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	openchamiv1alpha1 "github.com/openchami/openchami-operator/api/v1alpha1"
)

var _ = Describe("OpenCHAMICluster Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		openchamicluster := &openchamiv1alpha1.OpenCHAMICluster{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind OpenCHAMICluster")
			err := k8sClient.Get(ctx, typeNamespacedName, openchamicluster)
			if err != nil && errors.IsNotFound(err) {
				resource := &openchamiv1alpha1.OpenCHAMICluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
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
})
