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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	idpv1alpha1 "idp.y.digital/dex-idp-operator/api/v1alpha1"
)

var _ = Describe("Watch mappers", func() {
	const (
		namespace            = "default"
		mapperNamespace      = "watch-mapper-ns"
		mapConnectorName     = "map-connector"
		secretConnectorName  = "secret-connector"
		oidcSecretName       = "oidc-secret"
		mapConnectionName    = "map-connection"
		crossTenantName      = "cross-tenant"
		watchedConnectorName = "watched-connector"
	)

	ctx := context.Background()

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: mapperNamespace}}
		if err := k8sClient.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
		cleanupWatchMapperResources(ctx, namespace)
		cleanupWatchMapperResources(ctx, mapperNamespace)
	})

	AfterEach(func() {
		cleanupWatchMapperResources(ctx, namespace)
		cleanupWatchMapperResources(ctx, mapperNamespace)
	})

	It("maps a DexConnector change to referencing DexTenants", func() {
		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexConnector{
			ObjectMeta: metav1.ObjectMeta{Name: mapConnectorName, Namespace: namespace},
			Spec: idpv1alpha1.DexConnectorSpec{
				ConnectorID: mapConnectorName,
				Type:        oidcType,
				Config:      runtime.RawExtension{Raw: []byte(`{}`)},
			},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexTenant{
			ObjectMeta: metav1.ObjectMeta{Name: "map-tenant", Namespace: namespace},
			Spec: idpv1alpha1.DexTenantSpec{
				Issuer:       "https://map.example.com/dex",
				ConnectorRef: corev1.LocalObjectReference{Name: mapConnectorName},
			},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexTenant{
			ObjectMeta: metav1.ObjectMeta{Name: "other-tenant", Namespace: namespace},
			Spec: idpv1alpha1.DexTenantSpec{
				Issuer:       "https://other.example.com/dex",
				ConnectorRef: corev1.LocalObjectReference{Name: "other-connector"},
			},
		})).To(Succeed())

		reconciler := &DexTenantReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		requests := reconciler.dexTenantsForConnector(ctx, &idpv1alpha1.DexConnector{
			ObjectMeta: metav1.ObjectMeta{Name: mapConnectorName, Namespace: namespace},
		})
		Expect(requests).To(ConsistOf(reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: namespace, Name: "map-tenant"},
		}))
	})

	It("maps a Secret change to DexTenants whose connector interpolates it", func() {
		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexConnector{
			ObjectMeta: metav1.ObjectMeta{Name: secretConnectorName, Namespace: namespace},
			Spec: idpv1alpha1.DexConnectorSpec{
				ConnectorID: secretConnectorName,
				Type:        oidcType,
				Config:      runtime.RawExtension{Raw: []byte(`{"clientSecret":"${oidcSecret}"}`)},
				SecretRefs: []idpv1alpha1.DexConnectorSecretRef{
					{
						Name: oidcSecretRef,
						ValueFrom: idpv1alpha1.SecretValueSource{
							SecretKeyRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: oidcSecretName},
								Key:                  oidcSecretKey,
							},
						},
					},
				},
			},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexTenant{
			ObjectMeta: metav1.ObjectMeta{Name: "secret-tenant", Namespace: namespace},
			Spec: idpv1alpha1.DexTenantSpec{
				Issuer:       "https://secret.example.com/dex",
				ConnectorRef: corev1.LocalObjectReference{Name: secretConnectorName},
			},
		})).To(Succeed())

		reconciler := &DexTenantReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		requests := reconciler.dexTenantsForSecret(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: oidcSecretName, Namespace: namespace},
		})
		Expect(requests).To(ConsistOf(reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: namespace, Name: "secret-tenant"},
		}))
		Expect(reconciler.dexTenantsForSecret(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: namespace},
		})).To(BeEmpty())
	})

	It("maps a cookie encryption Secret change to the DexTenant that references it", func() {
		const cookieSecretName = "session-cookie-key"
		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexTenant{
			ObjectMeta: metav1.ObjectMeta{Name: "session-tenant", Namespace: namespace},
			Spec: idpv1alpha1.DexTenantSpec{
				Issuer:       "https://session.example.com/dex",
				ConnectorRef: corev1.LocalObjectReference{Name: "other-connector"},
				Sessions: &idpv1alpha1.DexTenantSessionConfig{
					CookieEncryptionKeySecretRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: cookieSecretName},
						Key:                  dexSessionCookieKey,
					},
				},
			},
		})).To(Succeed())

		reconciler := &DexTenantReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		Expect(reconciler.dexTenantsForSecret(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: cookieSecretName, Namespace: namespace},
		})).To(ConsistOf(reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: namespace, Name: "session-tenant"},
		}))
	})

	It("maps a DexConnection change to DexClients that reference it", func() {
		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexConnection{
			ObjectMeta: metav1.ObjectMeta{Name: mapConnectionName, Namespace: namespace},
			Spec:       idpv1alpha1.DexConnectionSpec{Address: testDexAddress, Plaintext: true},
		})).To(Succeed())
		secretValue := "s"
		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexClient{
			ObjectMeta: metav1.ObjectMeta{Name: "map-client", Namespace: namespace},
			Spec: idpv1alpha1.DexClientSpec{
				ConnectionRef: &corev1.LocalObjectReference{Name: mapConnectionName},
				ClientID:      "map-app",
				Secret:        &idpv1alpha1.ValueSource{Value: &secretValue},
			},
		})).To(Succeed())

		reconciler := &DexClientReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		requests := reconciler.dexClientsForConnection(ctx, &idpv1alpha1.DexConnection{
			ObjectMeta: metav1.ObjectMeta{Name: mapConnectionName, Namespace: namespace},
		})
		Expect(requests).To(ConsistOf(reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: namespace, Name: "map-client"},
		}))
	})

	It("maps a DexTenant change to DexClients across namespaces", func() {
		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexTenant{
			ObjectMeta: metav1.ObjectMeta{Name: crossTenantName, Namespace: mapperNamespace},
			Spec: idpv1alpha1.DexTenantSpec{
				Issuer:       "https://cross.example.com/dex",
				ConnectorRef: corev1.LocalObjectReference{Name: "cross-connector"},
			},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexClient{
			ObjectMeta: metav1.ObjectMeta{Name: "cross-client", Namespace: namespace},
			Spec: idpv1alpha1.DexClientSpec{
				TenantRefs: []idpv1alpha1.DexTenantReference{
					{Name: crossTenantName, Namespace: mapperNamespace},
				},
				ClientID: "cross-app",
			},
		})).To(Succeed())

		reconciler := &DexClientReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		requests := reconciler.dexClientsForTenant(ctx, &idpv1alpha1.DexTenant{
			ObjectMeta: metav1.ObjectMeta{Name: crossTenantName, Namespace: mapperNamespace},
		})
		Expect(requests).To(ConsistOf(reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: namespace, Name: "cross-client"},
		}))
	})

	It("maps a Secret change to DexClients that use it directly or via TLS", func() {
		secretValue := "s"
		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexClient{
			ObjectMeta: metav1.ObjectMeta{Name: "secret-client", Namespace: namespace},
			Spec: idpv1alpha1.DexClientSpec{
				ConnectionRef: &corev1.LocalObjectReference{Name: "plain-connection"},
				ClientID:      "secret-app",
				Secret: &idpv1alpha1.ValueSource{
					ValueFrom: &idpv1alpha1.SecretValueSource{
						SecretKeyRef: corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: generatedSecretKey},
							Key:                  providedSecretKey,
						},
					},
				},
			},
		})).To(Succeed())
		createTLSDexConnection(ctx, &idpv1alpha1.DexConnection{
			ObjectMeta: metav1.ObjectMeta{Name: "tls-connection", Namespace: namespace},
			Spec: idpv1alpha1.DexConnectionSpec{
				Address: testDexAddress,
				TLS: &idpv1alpha1.DexConnectionTLSConfig{
					CA: &idpv1alpha1.ValueSource{
						ValueFrom: &idpv1alpha1.SecretValueSource{
							SecretKeyRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "dex-ca"},
								Key:                  caCertKey,
							},
						},
					},
				},
			},
		})
		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexClient{
			ObjectMeta: metav1.ObjectMeta{Name: "tls-client", Namespace: namespace},
			Spec: idpv1alpha1.DexClientSpec{
				ConnectionRef: &corev1.LocalObjectReference{Name: "tls-connection"},
				ClientID:      "tls-app",
				Secret:        &idpv1alpha1.ValueSource{Value: &secretValue},
			},
		})).To(Succeed())

		reconciler := &DexClientReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		Expect(reconciler.dexClientsForSecret(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: generatedSecretKey, Namespace: namespace},
		})).To(ConsistOf(reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: namespace, Name: "secret-client"},
		}))
		Expect(reconciler.dexClientsForSecret(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "dex-ca", Namespace: namespace},
		})).To(ConsistOf(reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: namespace, Name: "tls-client"},
		}))
	})

	It("maps a Secret change to DexConnectors that interpolate it", func() {
		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexConnector{
			ObjectMeta: metav1.ObjectMeta{Name: watchedConnectorName, Namespace: namespace},
			Spec: idpv1alpha1.DexConnectorSpec{
				ConnectorID: watchedConnectorName,
				Type:        oidcType,
				Config:      runtime.RawExtension{Raw: []byte(`{"clientSecret":"${oidcSecret}"}`)},
				SecretRefs: []idpv1alpha1.DexConnectorSecretRef{
					{
						Name: oidcSecretRef,
						ValueFrom: idpv1alpha1.SecretValueSource{
							SecretKeyRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: oidcSecretName},
								Key:                  oidcSecretKey,
							},
						},
					},
				},
			},
		})).To(Succeed())

		reconciler := &DexConnectorReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		Expect(reconciler.dexConnectorsForSecret(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: oidcSecretName, Namespace: namespace},
		})).To(ConsistOf(reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: namespace, Name: watchedConnectorName},
		}))
		Expect(reconciler.dexConnectorsForSecret(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: namespace},
		})).To(BeEmpty())
	})
})

func cleanupWatchMapperResources(ctx context.Context, namespace string) {
	Expect(client.IgnoreNotFound(k8sClient.DeleteAllOf(ctx, &idpv1alpha1.DexClient{}, client.InNamespace(namespace)))).To(Succeed())
	Expect(client.IgnoreNotFound(k8sClient.DeleteAllOf(ctx, &idpv1alpha1.DexTenant{}, client.InNamespace(namespace)))).To(Succeed())
	Expect(client.IgnoreNotFound(k8sClient.DeleteAllOf(ctx, &idpv1alpha1.DexConnector{}, client.InNamespace(namespace)))).To(Succeed())
	Expect(client.IgnoreNotFound(k8sClient.DeleteAllOf(ctx, &idpv1alpha1.DexConnection{}, client.InNamespace(namespace)))).To(Succeed())
	Expect(client.IgnoreNotFound(k8sClient.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace(namespace)))).To(Succeed())

	Eventually(func(g Gomega) {
		clients := &idpv1alpha1.DexClientList{}
		g.Expect(k8sClient.List(ctx, clients, client.InNamespace(namespace))).To(Succeed())
		for i := range clients.Items {
			item := &clients.Items[i]
			if len(item.Finalizers) > 0 {
				item.Finalizers = nil
				g.Expect(client.IgnoreNotFound(k8sClient.Update(ctx, item))).To(Succeed())
			}
		}
		g.Expect(client.IgnoreNotFound(k8sClient.DeleteAllOf(ctx, &idpv1alpha1.DexClient{}, client.InNamespace(namespace)))).To(Succeed())
		g.Expect(k8sClient.List(ctx, clients, client.InNamespace(namespace))).To(Succeed())
		g.Expect(clients.Items).To(BeEmpty())
	}).Should(Succeed())
}
