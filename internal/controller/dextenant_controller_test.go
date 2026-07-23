/*
Copyright 2026.

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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	idpv1alpha1 "groeneslot.nl/dex-idp-operator/api/v1alpha1"
	"groeneslot.nl/dex-idp-operator/internal/dex"
)

var _ = Describe("DexTenant Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "tenant-a"
			connectorName     = "tenant-a-connector"
			resourceNamespace = "default"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}
		resourceName2 := dexTenantResourceName(&idpv1alpha1.DexTenant{ObjectMeta: metav1.ObjectMeta{Name: resourceName}})
		fakeDex := newFakeDexClient()

		BeforeEach(func() {
			cleanupDexTenantTestResources(ctx, resourceNamespace)
			fakeDex = newFakeDexClient()

			connector := &idpv1alpha1.DexConnector{
				ObjectMeta: metav1.ObjectMeta{Name: connectorName, Namespace: resourceNamespace},
				Spec: idpv1alpha1.DexConnectorSpec{
					ConnectorID: "mock",
					Type:        "mockCallback",
					DisplayName: "Mock",
					Config:      runtime.RawExtension{Raw: []byte(`{}`)},
				},
			}
			Expect(k8sClient.Create(ctx, connector)).To(Succeed())

			tenant := &idpv1alpha1.DexTenant{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec: idpv1alpha1.DexTenantSpec{
					Issuer:       "https://tenant-a.example.com/dex",
					ConnectorRef: corev1.LocalObjectReference{Name: connectorName},
				},
			}
			Expect(k8sClient.Create(ctx, tenant)).To(Succeed())
		})

		AfterEach(func() {
			cleanupDexTenantTestResources(ctx, resourceNamespace)
		})

		It("provisions the managed Dex runtime and pre-seeds the connector", func() {
			reconciler := newTestDexTenantReconciler(fakeDex)
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("creating the ServiceAccount, RBAC, ConfigMap, Deployment, and Service")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: resourceNamespace, Name: resourceName2},
				&corev1.ServiceAccount{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: resourceNamespace, Name: resourceName2},
				&rbacv1.Role{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: resourceNamespace, Name: resourceName2},
				&rbacv1.RoleBinding{})).To(Succeed())
			configMap := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: resourceNamespace, Name: resourceName2},
				configMap)).To(Succeed())
			Expect(configMap.Data["config.yaml"]).NotTo(ContainSubstring("oauth2:"))
			deployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Namespace: resourceNamespace, Name: resourceName2}, deployment)).To(Succeed())
			Expect(deployment.Spec.Template.Annotations).To(HaveKey(dexConfigChecksumKey))
			Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(deployment.Spec.Template.Spec.Containers[0].Env).To(ContainElement(corev1.EnvVar{
				Name:  "DEX_API_CONNECTORS_CRUD",
				Value: "true",
			}))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: resourceNamespace, Name: resourceName2},
				&corev1.Service{})).To(Succeed())

			By("creating an owned DexConnection")
			connection := &idpv1alpha1.DexConnection{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, connection)).To(Succeed())
			Expect(connection.Spec.Plaintext).To(BeTrue())
			Expect(connection.Spec.Address).To(Equal(resourceName2 + "." + resourceNamespace + ".svc.cluster.local:5557"))

			By("pre-seeding the connector into Dex Kubernetes storage")
			Eventually(func(g Gomega) {
				seeded := &unstructured.Unstructured{}
				seeded.SetGroupVersionKind(dexStorageConnectorGVK)
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: resourceNamespace, Name: "mock"}, seeded)).To(Succeed())
			}, "10s").Should(Succeed())

			By("syncing the connector to the live Dex instance over gRPC")
			Expect(fakeDex.connectors).To(HaveKey("mock"))
			Expect(fakeDex.connectors["mock"].GetType()).To(Equal("mockCallback"))

			By("reporting Ready and the DexConnection/Service names on status")
			updated := &idpv1alpha1.DexTenant{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.ConnectionRef).To(Equal(resourceName))
			Expect(updated.Status.ServiceName).To(Equal(resourceName2))
			condition := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
		})

		It("reports NotReady while the managed Dex instance is unreachable", func() {
			fakeDex.unreachable = true
			reconciler := newTestDexTenantReconciler(fakeDex)
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			updated := &idpv1alpha1.DexTenant{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			condition := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal("DexUnavailable"))
		})

		It("configures the approval screen per tenant and rolls Dex when it changes", func() {
			reconciler := newTestDexTenantReconciler(fakeDex)
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			tenant := &idpv1alpha1.DexTenant{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tenant)).To(Succeed())
			tenant.Spec.OAuth2 = &idpv1alpha1.DexTenantOAuth2Config{SkipApprovalScreen: false}
			Expect(k8sClient.Update(ctx, tenant)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			configMap := &corev1.ConfigMap{}
			resourceKey := types.NamespacedName{Namespace: resourceNamespace, Name: resourceName2}
			Expect(k8sClient.Get(ctx, resourceKey, configMap)).To(Succeed())
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("oauth2:\n  skipApprovalScreen: false"))

			deployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, resourceKey, deployment)).To(Succeed())
			approvalRequiredChecksum := deployment.Spec.Template.Annotations[dexConfigChecksumKey]

			Expect(k8sClient.Get(ctx, typeNamespacedName, tenant)).To(Succeed())
			tenant.Spec.OAuth2.SkipApprovalScreen = true
			Expect(k8sClient.Update(ctx, tenant)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, resourceKey, configMap)).To(Succeed())
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("oauth2:\n  skipApprovalScreen: true"))
			Expect(k8sClient.Get(ctx, resourceKey, deployment)).To(Succeed())
			Expect(deployment.Spec.Template.Annotations[dexConfigChecksumKey]).NotTo(Equal(approvalRequiredChecksum))
		})
	})
})

func newTestDexTenantReconciler(fakeDex *fakeDexClient) *DexTenantReconciler {
	return &DexTenantReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		ClientFactory: func(context.Context, dex.DialConfig) (dex.Client, error) {
			return fakeDex, nil
		},
	}
}

func cleanupDexTenantTestResources(ctx context.Context, namespace string) {
	Expect(k8sClient.DeleteAllOf(ctx, &idpv1alpha1.DexTenant{}, client.InNamespace(namespace))).To(Succeed())
	Expect(k8sClient.DeleteAllOf(ctx, &idpv1alpha1.DexConnector{}, client.InNamespace(namespace))).To(Succeed())
	Expect(k8sClient.DeleteAllOf(ctx, &idpv1alpha1.DexConnection{}, client.InNamespace(namespace))).To(Succeed())
	Expect(k8sClient.DeleteAllOf(ctx, &appsv1.Deployment{}, client.InNamespace(namespace))).To(Succeed())
	Expect(k8sClient.DeleteAllOf(ctx, &corev1.Service{}, client.InNamespace(namespace))).To(Succeed())
	Expect(k8sClient.DeleteAllOf(ctx, &corev1.ConfigMap{}, client.InNamespace(namespace))).To(Succeed())
	Expect(k8sClient.DeleteAllOf(ctx, &corev1.ServiceAccount{}, client.InNamespace(namespace))).To(Succeed())
	Expect(k8sClient.DeleteAllOf(ctx, &rbacv1.Role{}, client.InNamespace(namespace))).To(Succeed())
	Expect(k8sClient.DeleteAllOf(ctx, &rbacv1.RoleBinding{}, client.InNamespace(namespace))).To(Succeed())

	Eventually(func(g Gomega) {
		tenants := &idpv1alpha1.DexTenantList{}
		g.Expect(k8sClient.List(ctx, tenants, client.InNamespace(namespace))).To(Succeed())
		g.Expect(tenants.Items).To(BeEmpty())
	}).Should(Succeed())
}
