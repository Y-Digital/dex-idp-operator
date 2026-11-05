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
	"errors"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	idpv1alpha1 "idp.y.digital/dex-idp-operator/api/v1alpha1"
	"idp.y.digital/dex-idp-operator/internal/dex"
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
			role := &rbacv1.Role{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: resourceNamespace, Name: resourceName2},
				role)).To(Succeed())
			Expect(role.Rules[0].Resources).To(ContainElements("useridentities", "authsessions"))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: resourceNamespace, Name: resourceName2},
				&rbacv1.RoleBinding{})).To(Succeed())
			configMap := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: resourceNamespace, Name: resourceName2},
				configMap)).To(Succeed())
			Expect(configMap.Data["config.yaml"]).NotTo(ContainSubstring("oauth2:"))
			Expect(configMap.Data["config.yaml"]).NotTo(ContainSubstring("sessions:"))
			deployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Namespace: resourceNamespace, Name: resourceName2}, deployment)).To(Succeed())
			Expect(deployment.Spec.Template.Annotations).To(HaveKey(dexConfigChecksumKey))
			Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(deployment.Spec.Template.Spec.SecurityContext).NotTo(BeNil())
			Expect(deployment.Spec.Template.Spec.SecurityContext.RunAsUser).To(HaveValue(Equal(dexContainerUserID)))
			Expect(deployment.Spec.Template.Spec.SecurityContext.RunAsGroup).To(HaveValue(Equal(dexContainerUserID)))
			Expect(deployment.Spec.Template.Spec.Containers[0].Env).To(ContainElement(corev1.EnvVar{
				Name:  "DEX_API_CONNECTORS_CRUD",
				Value: strconv.FormatBool(true),
			}))
			Expect(deployment.Spec.Template.Spec.Containers[0].Env).NotTo(ContainElement(
				HaveField("Name", dexSessionsEnabledEnv),
			))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: resourceNamespace, Name: resourceName2},
				&corev1.Service{})).To(Succeed())

			By("creating an owned DexConnection")
			connection := &idpv1alpha1.DexConnection{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, connection)).To(Succeed())
			Expect(connection.Spec.Plaintext).To(BeTrue())
			Expect(connection.Spec.Address).To(Equal(resourceName2 + "." + resourceNamespace + ".svc.cluster.local:5557"))

			By("ensuring Dex Kubernetes storage CRDs")
			for _, resource := range dexStorageResources {
				crd := &apiextensionsv1.CustomResourceDefinition{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: resource.Plural + "." + dexStorageAPIGroup,
				}, crd)).To(Succeed())
			}

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
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(dexUnavailableRequeueInterval))

			updated := &idpv1alpha1.DexTenant{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			condition := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal("DexUnavailable"))
		})

		It("reports NotReady when the Dex client factory fails to dial", func() {
			reconciler := &DexTenantReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				ClientFactory: func(context.Context, dex.DialConfig) (dex.Client, error) {
					return nil, errors.New("dial refused")
				},
			}
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(dexUnavailableRequeueInterval))

			updated := &idpv1alpha1.DexTenant{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			condition := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal("DexUnavailable"))
		})

		It("reports ConnectorNotFound when the referenced connector is missing", func() {
			Expect(k8sClient.DeleteAllOf(ctx, &idpv1alpha1.DexConnector{}, client.InNamespace(resourceNamespace))).To(Succeed())
			Eventually(func(g Gomega) {
				connectors := &idpv1alpha1.DexConnectorList{}
				g.Expect(k8sClient.List(ctx, connectors, client.InNamespace(resourceNamespace))).To(Succeed())
				g.Expect(connectors.Items).To(BeEmpty())
			}).Should(Succeed())

			reconciler := newTestDexTenantReconciler(fakeDex)
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).To(HaveOccurred())

			updated := &idpv1alpha1.DexTenant{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			condition := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal("ConnectorNotFound"))
		})

		It("reports ConnectorInvalid when the connector secretRef cannot be resolved", func() {
			connector := &idpv1alpha1.DexConnector{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: connectorName, Namespace: resourceNamespace,
			}, connector)).To(Succeed())
			connector.Spec.Config = runtime.RawExtension{Raw: []byte(`{"clientSecret":"${missing}"}`)}
			connector.Spec.SecretRefs = []idpv1alpha1.DexConnectorSecretRef{
				{
					Name: "missing",
					ValueFrom: idpv1alpha1.SecretValueSource{
						SecretKeyRef: corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "does-not-exist"},
							Key:                  "value",
						},
					},
				},
			}
			Expect(k8sClient.Update(ctx, connector)).To(Succeed())

			reconciler := newTestDexTenantReconciler(fakeDex)
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).To(HaveOccurred())

			updated := &idpv1alpha1.DexTenant{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			condition := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal("ConnectorInvalid"))
		})

		It("reports ConnectorSyncFailed when Dex connector gRPC sync fails", func() {
			fakeDex.updateConnectorErr = errors.New("sync boom")
			reconciler := newTestDexTenantReconciler(fakeDex)
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("sync boom"))

			updated := &idpv1alpha1.DexTenant{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			condition := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal("ConnectorSyncFailed"))
		})

		It("applies a custom Dex image and replica count", func() {
			customImage := "example.com/dex:testdev"
			replicas := int32(3)
			tenant := &idpv1alpha1.DexTenant{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tenant)).To(Succeed())
			tenant.Spec.Image = customImage
			tenant.Spec.Replicas = &replicas
			Expect(k8sClient.Update(ctx, tenant)).To(Succeed())

			reconciler := newTestDexTenantReconciler(fakeDex)
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			deployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: resourceNamespace, Name: resourceName2,
			}, deployment)).To(Succeed())
			Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal(customImage))
			Expect(deployment.Spec.Replicas).To(HaveValue(Equal(replicas)))
		})

		It("configures oauth2 grantTypes and rolls Dex when they change", func() {
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
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("oauth2:\n  skipApprovalScreen: false\n"))
			Expect(configMap.Data["config.yaml"]).NotTo(ContainSubstring("grantTypes:"))

			deployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, resourceKey, deployment)).To(Succeed())
			approvalRequiredChecksum := deployment.Spec.Template.Annotations[dexConfigChecksumKey]
			Expect(deployment.Spec.Template.Spec.Containers[0].Env).To(ContainElement(corev1.EnvVar{
				Name:  "DEX_CLIENT_CREDENTIAL_GRANT_ENABLED_BY_DEFAULT",
				Value: strconv.FormatBool(true),
			}))

			Expect(k8sClient.Get(ctx, typeNamespacedName, tenant)).To(Succeed())
			tenant.Spec.OAuth2.SkipApprovalScreen = true
			tenant.Spec.OAuth2.GrantTypes = []string{
				"authorization_code",
				"refresh_token",
				"client_credentials",
				"urn:ietf:params:oauth:grant-type:token-exchange",
			}
			Expect(k8sClient.Update(ctx, tenant)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, resourceKey, configMap)).To(Succeed())
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("skipApprovalScreen: true"))
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("grantTypes:"))
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("authorization_code"))
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("refresh_token"))
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("client_credentials"))
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("urn:ietf:params:oauth:grant-type:token-exchange"))
			Expect(k8sClient.Get(ctx, resourceKey, deployment)).To(Succeed())
			Expect(deployment.Spec.Template.Annotations[dexConfigChecksumKey]).NotTo(Equal(approvalRequiredChecksum))
			Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal(idpv1alpha1.DefaultDexImage))
		})

		It("enables browser sessions and stores the cookie encryption key in a Secret", func() {
			tenant := &idpv1alpha1.DexTenant{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tenant)).To(Succeed())
			tenant.Spec.Sessions = &idpv1alpha1.DexTenantSessionConfig{}
			Expect(k8sClient.Update(ctx, tenant)).To(Succeed())

			reconciler := newTestDexTenantReconciler(fakeDex)
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			secret := &corev1.Secret{}
			secretKey := types.NamespacedName{Namespace: resourceNamespace, Name: dexSessionSecretName(tenant)}
			Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
			Expect(secret.OwnerReferences).NotTo(BeEmpty())
			cookieKey := string(secret.Data[dexSessionCookieKey])
			Expect(cookieKey).To(HaveLen(32))

			resourceKey := types.NamespacedName{Namespace: resourceNamespace, Name: resourceName2}
			configMap := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, resourceKey, configMap)).To(Succeed())
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("sessions:\n"))
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("cookieEncryptionKey: " + cookieKey))

			deployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, resourceKey, deployment)).To(Succeed())
			Expect(deployment.Spec.Template.Spec.Containers[0].Env).To(ContainElement(corev1.EnvVar{
				Name:  dexSessionsEnabledEnv,
				Value: strconv.FormatBool(true),
			}))

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
			Expect(string(secret.Data[dexSessionCookieKey])).To(Equal(cookieKey))
		})

		It("rejects a cookie encryption key of the wrong length without writing session config", func() {
			reconciler := newTestDexTenantReconciler(fakeDex)
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			const cookieSecretName = "tenant-a-cookie"
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: cookieSecretName, Namespace: resourceNamespace},
				Data:       map[string][]byte{dexSessionCookieKey: []byte("short")},
			})).To(Succeed())

			tenant := &idpv1alpha1.DexTenant{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tenant)).To(Succeed())
			tenant.Spec.Sessions = &idpv1alpha1.DexTenantSessionConfig{
				CookieEncryptionKeySecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cookieSecretName},
					Key:                  dexSessionCookieKey,
				},
			}
			Expect(k8sClient.Update(ctx, tenant)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("16, 24, or 32 bytes"))

			updated := &idpv1alpha1.DexTenant{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			condition := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(reasonSessionKeyInvalid))

			configMap := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: resourceNamespace, Name: resourceName2}, configMap)).To(Succeed())
			Expect(configMap.Data["config.yaml"]).NotTo(ContainSubstring("sessions:"))
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: resourceNamespace, Name: dexSessionSecretName(tenant),
			}, &corev1.Secret{})).NotTo(Succeed())
		})

		It("rolls Dex when the referenced cookie encryption key changes", func() {
			const cookieSecretName = "tenant-a-cookie"
			originalKey := "abcdefghijklmnopqrstuvwxyz012345"
			rotatedKey := "ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: cookieSecretName, Namespace: resourceNamespace},
				Data:       map[string][]byte{dexSessionCookieKey: []byte(originalKey)},
			})).To(Succeed())

			tenant := &idpv1alpha1.DexTenant{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, tenant)).To(Succeed())
			lifetime := metav1.Duration{Duration: 12 * time.Hour}
			idle := metav1.Duration{Duration: 30 * time.Minute}
			rememberMe := false
			tenant.Spec.Sessions = &idpv1alpha1.DexTenantSessionConfig{
				CookieName:                 "tenant_session",
				AbsoluteLifetime:           &lifetime,
				ValidIfNotUsedFor:          &idle,
				RememberMeCheckedByDefault: &rememberMe,
				SSOSharedWithDefault:       "all",
				CookieEncryptionKeySecretRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: cookieSecretName},
					Key:                  dexSessionCookieKey,
				},
			}
			Expect(k8sClient.Update(ctx, tenant)).To(Succeed())

			reconciler := newTestDexTenantReconciler(fakeDex)
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			resourceKey := types.NamespacedName{Namespace: resourceNamespace, Name: resourceName2}
			configMap := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, resourceKey, configMap)).To(Succeed())
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("cookieEncryptionKey: " + originalKey))
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("cookieName: tenant_session"))
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("absoluteLifetime: 12h0m0s"))
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("validIfNotUsedFor: 30m0s"))
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("rememberMeCheckedByDefault: false"))
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("ssoSharedWithDefault: all"))

			deployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, resourceKey, deployment)).To(Succeed())
			originalChecksum := deployment.Spec.Template.Annotations[dexConfigChecksumKey]

			cookieSecret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: resourceNamespace, Name: cookieSecretName}, cookieSecret)).To(Succeed())
			cookieSecret.Data[dexSessionCookieKey] = []byte(rotatedKey)
			Expect(k8sClient.Update(ctx, cookieSecret)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, resourceKey, configMap)).To(Succeed())
			Expect(configMap.Data["config.yaml"]).To(ContainSubstring("cookieEncryptionKey: " + rotatedKey))
			Expect(configMap.Data["config.yaml"]).NotTo(ContainSubstring(originalKey))
			Expect(k8sClient.Get(ctx, resourceKey, deployment)).To(Succeed())
			Expect(deployment.Spec.Template.Annotations[dexConfigChecksumKey]).NotTo(Equal(originalChecksum))
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
	Expect(k8sClient.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace(namespace))).To(Succeed())

	Eventually(func(g Gomega) {
		tenants := &idpv1alpha1.DexTenantList{}
		g.Expect(k8sClient.List(ctx, tenants, client.InNamespace(namespace))).To(Succeed())
		g.Expect(tenants.Items).To(BeEmpty())
	}).Should(Succeed())
}
