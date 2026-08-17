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
	"fmt"

	dexapi "github.com/dexidp/dex/api/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	idpv1alpha1 "groeneslot.nl/dex-idp-operator/api/v1alpha1"
	"groeneslot.nl/dex-idp-operator/internal/dex"
)

const exampleApp string = "example-app"
const exampleCallback string = "https://app.example.com/callback"
const exampleAppName string = "Example App"

var _ = Describe("DexClient Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-client"
			connectionName    = "test-connection"
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		secretValue := "super-secret"
		fakeDex := newFakeDexClient()

		BeforeEach(func() {
			cleanupDexClientTestResources(ctx)
			fakeDex = newFakeDexClient()

			By("creating the DexConnection")
			connection := &idpv1alpha1.DexConnection{
				ObjectMeta: metav1.ObjectMeta{
					Name:      connectionName,
					Namespace: resourceNamespace,
				},
				Spec: idpv1alpha1.DexConnectionSpec{
					Address:   testDexAddress,
					Plaintext: true,
				},
			}
			Expect(k8sClient.Create(ctx, connection)).To(Succeed())

			By("creating the client secret")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      generatedSecretKey,
					Namespace: resourceNamespace,
				},
				Data: map[string][]byte{
					providedSecretKey: []byte(secretValue),
				},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			By("creating the DexClient")
			resource := &idpv1alpha1.DexClient{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: resourceNamespace,
				},
				Spec: idpv1alpha1.DexClientSpec{
					ConnectionRef: &corev1.LocalObjectReference{Name: connectionName},
					ClientID:      exampleApp,
					Secret: &idpv1alpha1.ValueSource{
						ValueFrom: &idpv1alpha1.SecretValueSource{
							SecretKeyRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: generatedSecretKey},
								Key:                  providedSecretKey,
							},
						},
					},
					RedirectURIs: []string{exampleCallback},
					Name:         exampleAppName,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(func() {
			cleanupDexClientTestResources(ctx)
		})

		It("creates a missing Dex client from SecretRef values", func() {
			controllerReconciler := newTestDexClientReconciler(fakeDex)

			By("adding the finalizer")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("creating the Dex client")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			created := fakeDex.clients[exampleApp]
			Expect(created).NotTo(BeNil())
			Expect(created.GetSecret()).To(Equal(secretValue))
			Expect(created.GetRedirectUris()).To(Equal([]string{exampleCallback}))

			updated := &idpv1alpha1.DexClient{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			condition := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionSynced)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
		})

		It("blocks immutable secret changes unless recreation is enabled", func() {
			fakeDex.clients[exampleApp] = &dexapi.Client{
				Id:           exampleApp,
				Secret:       "old-secret",
				RedirectUris: []string{exampleCallback},
				Name:         exampleAppName,
			}
			controllerReconciler := newTestDexClientReconciler(fakeDex)

			By("adding the finalizer")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("reconciling the immutable change")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			updated := &idpv1alpha1.DexClient{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			blocked := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionBlocked)
			Expect(blocked).NotTo(BeNil())
			Expect(blocked.Status).To(Equal(metav1.ConditionTrue))
			Expect(fakeDex.clients[exampleApp].GetSecret()).To(Equal("old-secret"))
		})

		It("recreates the Dex client when recreateOnImmutableChange is set", func() {
			fakeDex.clients[exampleApp] = &dexapi.Client{
				Id:           exampleApp,
				Secret:       "old-secret",
				RedirectUris: []string{exampleCallback},
				Name:         exampleAppName,
			}

			resource := &idpv1alpha1.DexClient{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			resource.Spec.RecreateOnImmutableChange = true
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			controllerReconciler := newTestDexClientReconciler(fakeDex)
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			created := fakeDex.clients[exampleApp]
			Expect(created).NotTo(BeNil())
			Expect(created.GetSecret()).To(Equal(secretValue))

			updated := &idpv1alpha1.DexClient{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			synced := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionSynced)
			Expect(synced).NotTo(BeNil())
			Expect(synced.Status).To(Equal(metav1.ConditionTrue))
			blocked := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionBlocked)
			Expect(blocked).NotTo(BeNil())
			Expect(blocked.Status).To(Equal(metav1.ConditionFalse))
		})

		It("updates mutable Dex client fields", func() {
			controllerReconciler := newTestDexClientReconciler(fakeDex)
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			resource := &idpv1alpha1.DexClient{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			resource.Spec.RedirectURIs = []string{"https://app.example.com/new-callback"}
			resource.Spec.TrustedPeers = []string{"peer-app"}
			resource.Spec.Name = "Renamed App"
			resource.Spec.LogoURL = "https://app.example.com/logo.png"
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			updatedClient := fakeDex.clients[exampleApp]
			Expect(updatedClient).NotTo(BeNil())
			Expect(updatedClient.GetSecret()).To(Equal(secretValue))
			Expect(updatedClient.GetRedirectUris()).To(Equal([]string{"https://app.example.com/new-callback"}))
			Expect(updatedClient.GetTrustedPeers()).To(Equal([]string{"peer-app"}))
			Expect(updatedClient.GetName()).To(Equal("Renamed App"))
			Expect(updatedClient.GetLogoUrl()).To(Equal("https://app.example.com/logo.png"))

			updated := &idpv1alpha1.DexClient{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			synced := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionSynced)
			Expect(synced).NotTo(BeNil())
			Expect(synced.Status).To(Equal(metav1.ConditionTrue))
		})

		It("deletes the Dex client during finalizer cleanup", func() {
			fakeDex.clients[exampleApp] = &dexapi.Client{Id: exampleApp, Secret: secretValue}
			controllerReconciler := newTestDexClientReconciler(fakeDex)

			By("adding the finalizer")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			resource := &idpv1alpha1.DexClient{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("running finalizer cleanup")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(fakeDex.clients).NotTo(HaveKey(exampleApp))
		})
	})
})

var _ = Describe("DexClient Controller public client", func() {
	const (
		resourceName      = "public-client"
		connectionName    = "public-connection"
		resourceNamespace = "default"
	)

	ctx := context.Background()
	typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}
	fakeDex := newFakeDexClient()

	BeforeEach(func() {
		cleanupDexClientTestResources(ctx)
		fakeDex = newFakeDexClient()

		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexConnection{
			ObjectMeta: metav1.ObjectMeta{Name: connectionName, Namespace: resourceNamespace},
			Spec:       idpv1alpha1.DexConnectionSpec{Address: testDexAddress, Plaintext: true},
		})).To(Succeed())

		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexClient{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
			Spec: idpv1alpha1.DexClientSpec{
				ConnectionRef: &corev1.LocalObjectReference{Name: connectionName},
				ClientID:      "public-app",
				Public:        true,
				RedirectURIs:  []string{exampleCallback},
				Name:          "Public App",
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		cleanupDexClientTestResources(ctx)
	})

	It("syncs a public client without a client secret", func() {
		controllerReconciler := newTestDexClientReconciler(fakeDex)
		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())
		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())

		created := fakeDex.clients["public-app"]
		Expect(created).NotTo(BeNil())
		Expect(created.GetPublic()).To(BeTrue())
		Expect(created.GetSecret()).To(BeEmpty())

		secrets := &corev1.SecretList{}
		Expect(k8sClient.List(ctx, secrets, client.InNamespace(resourceNamespace))).To(Succeed())
		Expect(secrets.Items).To(BeEmpty())

		updated := &idpv1alpha1.DexClient{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
		synced := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionSynced)
		Expect(synced).NotTo(BeNil())
		Expect(synced.Status).To(Equal(metav1.ConditionTrue))
	})
})

var _ = Describe("DexClient Controller with inline secret value", func() {
	const (
		resourceName      = "inline-secret-client"
		connectionName    = "inline-connection"
		resourceNamespace = "default"
		inlineSecret      = "inline-super-secret"
	)

	ctx := context.Background()
	typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}
	fakeDex := newFakeDexClient()

	BeforeEach(func() {
		cleanupDexClientTestResources(ctx)
		fakeDex = newFakeDexClient()

		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexConnection{
			ObjectMeta: metav1.ObjectMeta{Name: connectionName, Namespace: resourceNamespace},
			Spec:       idpv1alpha1.DexConnectionSpec{Address: testDexAddress, Plaintext: true},
		})).To(Succeed())

		secretValue := inlineSecret
		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexClient{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
			Spec: idpv1alpha1.DexClientSpec{
				ConnectionRef: &corev1.LocalObjectReference{Name: connectionName},
				ClientID:      "inline-app",
				Secret:        &idpv1alpha1.ValueSource{Value: &secretValue},
				RedirectURIs:  []string{exampleCallback},
				Name:          "Inline App",
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		cleanupDexClientTestResources(ctx)
	})

	It("creates a Dex client from an inline ValueSource", func() {
		controllerReconciler := newTestDexClientReconciler(fakeDex)
		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())
		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())

		created := fakeDex.clients["inline-app"]
		Expect(created).NotTo(BeNil())
		Expect(created.GetSecret()).To(Equal(inlineSecret))
	})
})

var _ = Describe("DexClient Controller resolve failures", func() {
	const (
		resourceName      = "unready-tenant-app"
		resourceNamespace = "default"
		tenantName        = "unready-tenant"
	)

	ctx := context.Background()
	typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}

	BeforeEach(func() {
		cleanupDexClientTestResources(ctx)

		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexTenant{
			ObjectMeta: metav1.ObjectMeta{Name: tenantName, Namespace: resourceNamespace},
			Spec: idpv1alpha1.DexTenantSpec{
				Issuer:       "https://unready.example.com/dex",
				ConnectorRef: corev1.LocalObjectReference{Name: tenantName + "-connector"},
			},
		})).To(Succeed())

		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexClient{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
			Spec: idpv1alpha1.DexClientSpec{
				TenantRefs:   []idpv1alpha1.DexTenantReference{{Name: tenantName}},
				ClientID:     "unready-app",
				RedirectURIs: []string{exampleCallback},
				Name:         "Unready App",
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		cleanupDexClientTestResources(ctx)
	})

	It("reports ResolveFailed when a tenant has no Dex connection yet", func() {
		controllerReconciler := newTestDexClientReconciler(newFakeDexClient())
		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())
		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("does not have a ready Dex connection yet"))

		updated := &idpv1alpha1.DexClient{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Mounts).To(HaveLen(1))
		Expect(updated.Status.Mounts[0].Ready).To(BeFalse())
		Expect(updated.Status.Mounts[0].Reason).To(Equal("ResolveFailed"))
		synced := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionSynced)
		Expect(synced).NotTo(BeNil())
		Expect(synced.Status).To(Equal(metav1.ConditionFalse))
	})
})

var _ = Describe("DexClient Controller TLS dialConfig", func() {
	const (
		resourceName      = "tls-client"
		connectionName    = "tls-connection"
		resourceNamespace = "default"
		caSecretName      = "dex-ca"
		certSecretName    = "dex-client-tls"
	)

	ctx := context.Background()
	typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}
	fakeDex := newFakeDexClient()
	caPEM := []byte("-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----")
	certPEM := []byte("-----BEGIN CERTIFICATE-----\nCERT\n-----END CERTIFICATE-----")
	keyPEM := []byte("-----BEGIN PRIVATE KEY-----\nKEY\n-----END PRIVATE KEY-----")

	BeforeEach(func() {
		cleanupDexClientTestResources(ctx)
		fakeDex = newFakeDexClient()

		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: caSecretName, Namespace: resourceNamespace},
			Data:       map[string][]byte{caCertKey: caPEM},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: certSecretName, Namespace: resourceNamespace},
			Data: map[string][]byte{
				"tls.crt": certPEM,
				"tls.key": keyPEM,
			},
		})).To(Succeed())

		createTLSDexConnection(ctx, &idpv1alpha1.DexConnection{
			ObjectMeta: metav1.ObjectMeta{Name: connectionName, Namespace: resourceNamespace},
			Spec: idpv1alpha1.DexConnectionSpec{
				Address: testDexAddress,
				TLS: &idpv1alpha1.DexConnectionTLSConfig{
					CA: &idpv1alpha1.ValueSource{
						ValueFrom: &idpv1alpha1.SecretValueSource{
							SecretKeyRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: caSecretName},
								Key:                  caCertKey,
							},
						},
					},
					ClientCertificate: &idpv1alpha1.DexConnectionClientCertificate{
						Certificate: idpv1alpha1.ValueSource{
							ValueFrom: &idpv1alpha1.SecretValueSource{
								SecretKeyRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: certSecretName},
									Key:                  "tls.crt",
								},
							},
						},
						PrivateKey: idpv1alpha1.ValueSource{
							ValueFrom: &idpv1alpha1.SecretValueSource{
								SecretKeyRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: certSecretName},
									Key:                  "tls.key",
								},
							},
						},
					},
					InsecureSkipVerify: true,
					ServerName:         "dex.example.com",
				},
			},
		})

		secretValue := "tls-client-secret"
		Expect(k8sClient.Create(ctx, &idpv1alpha1.DexClient{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
			Spec: idpv1alpha1.DexClientSpec{
				ConnectionRef: &corev1.LocalObjectReference{Name: connectionName},
				ClientID:      "tls-app",
				Secret:        &idpv1alpha1.ValueSource{Value: &secretValue},
				RedirectURIs:  []string{exampleCallback},
				Name:          "TLS App",
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		cleanupDexClientTestResources(ctx)
	})

	It("builds TLS dial config from DexConnection secrets", func() {
		var captured dex.DialConfig
		controllerReconciler := &DexClientReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			ClientFactory: func(_ context.Context, cfg dex.DialConfig) (dex.Client, error) {
				captured = cfg
				return fakeDex, nil
			},
		}

		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())
		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())

		Expect(captured.Address).To(Equal(testDexAddress))
		Expect(captured.Plaintext).To(BeFalse())
		Expect(captured.TLS.CACertPEM).To(Equal(caPEM))
		Expect(captured.TLS.ClientCertPEM).To(Equal(certPEM))
		Expect(captured.TLS.ClientKeyPEM).To(Equal(keyPEM))
		Expect(captured.TLS.InsecureSkipVerify).To(BeTrue())
		Expect(captured.TLS.ServerName).To(Equal("dex.example.com"))
		Expect(fakeDex.clients).To(HaveKey("tls-app"))
	})
})

var _ = Describe("DexClient Controller with tenantRefs", func() {
	const (
		resourceName      = "multi-tenant-app"
		resourceNamespace = "default"
		tenantAName       = "tenant-a"
		tenantBName       = "tenant-b"
	)

	ctx := context.Background()
	typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}
	fakeDexA := newFakeDexClient()
	fakeDexB := newFakeDexClient()

	BeforeEach(func() {
		cleanupDexClientTestResources(ctx)
		fakeDexA = newFakeDexClient()
		fakeDexB = newFakeDexClient()

		By("creating owned DexConnections and ready DexTenants")
		createReadyTenant(ctx, resourceNamespace, tenantAName)
		createReadyTenant(ctx, resourceNamespace, tenantBName)

		By("creating the multi-tenant DexClient")
		resource := &idpv1alpha1.DexClient{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
			Spec: idpv1alpha1.DexClientSpec{
				TenantRefs: []idpv1alpha1.DexTenantReference{
					{Name: tenantAName},
					{Name: tenantBName},
				},
				ClientID:     "backend-app",
				RedirectURIs: []string{"https://backend.example.com/callback"},
				Name:         "Backend App",
			},
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	})

	AfterEach(func() {
		cleanupDexClientTestResources(ctx)
	})

	It("mounts the client onto every tenant with a distinct generated secret", func() {
		controllerReconciler := newTestMultiDexClientReconciler(tenantAName, fakeDexA, tenantBName, fakeDexB)

		By("adding the finalizer")
		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())

		By("creating the client in both tenants")
		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())

		clientA := fakeDexA.clients["backend-app"]
		clientB := fakeDexB.clients["backend-app"]
		Expect(clientA).NotTo(BeNil())
		Expect(clientB).NotTo(BeNil())
		Expect(clientA.GetSecret()).NotTo(BeEmpty())
		Expect(clientB.GetSecret()).NotTo(BeEmpty())
		Expect(clientA.GetSecret()).NotTo(Equal(clientB.GetSecret()))

		By("generating one Secret per tenant mount")
		secretA := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: resourceNamespace, Name: resourceName + "-" + tenantAName + "-secret",
		}, secretA)).To(Succeed())
		Expect(string(secretA.Data[generatedSecretKey])).To(Equal(clientA.GetSecret()))

		By("reporting per-tenant mount status")
		updated := &idpv1alpha1.DexClient{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Mounts).To(HaveLen(2))
		for _, mount := range updated.Status.Mounts {
			Expect(mount.Ready).To(BeTrue())
		}
		condition := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionSynced)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionTrue))
	})

	It("deletes the client from every mounted tenant during finalizer cleanup", func() {
		controllerReconciler := newTestMultiDexClientReconciler(tenantAName, fakeDexA, tenantBName, fakeDexB)

		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())
		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())
		Expect(fakeDexA.clients).To(HaveKey("backend-app"))
		Expect(fakeDexB.clients).To(HaveKey("backend-app"))

		resource := &idpv1alpha1.DexClient{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(Succeed())
		Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())
		Expect(fakeDexA.clients).NotTo(HaveKey("backend-app"))
		Expect(fakeDexB.clients).NotTo(HaveKey("backend-app"))
	})
})

var _ = Describe("DexClient Controller with cross-namespace tenantRefs", func() {
	const (
		resourceName      = "cross-ns-app"
		resourceNamespace = "default"
		tenantName        = "tenant-x"
		tenantNamespace   = "tenant-x-ns"
	)

	ctx := context.Background()
	typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}
	fakeDexX := newFakeDexClient()

	BeforeEach(func() {
		cleanupDexClientTestResources(ctx)
		fakeDexX = newFakeDexClient()

		By("creating the tenant namespace")
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: tenantNamespace}}
		if err := k8sClient.Create(ctx, namespace); err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}

		By("creating a ready DexTenant in its own namespace")
		createReadyTenant(ctx, tenantNamespace, tenantName)

		By("creating a DexClient in a different namespace referencing that tenant")
		resource := &idpv1alpha1.DexClient{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
			Spec: idpv1alpha1.DexClientSpec{
				TenantRefs: []idpv1alpha1.DexTenantReference{
					{Name: tenantName, Namespace: tenantNamespace},
				},
				ClientID:     "cross-ns-backend",
				RedirectURIs: []string{"https://cross-ns.example.com/callback"},
				Name:         "Cross Namespace App",
			},
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	})

	AfterEach(func() {
		cleanupDexClientTestResources(ctx)
		Expect(k8sClient.DeleteAllOf(ctx, &idpv1alpha1.DexTenant{}, client.InNamespace(tenantNamespace))).To(Succeed())
		Expect(k8sClient.DeleteAllOf(ctx, &idpv1alpha1.DexConnection{}, client.InNamespace(tenantNamespace))).To(Succeed())
	})

	It("mounts the client onto a DexTenant in a different namespace", func() {
		controllerReconciler := &DexClientReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			ClientFactory: func(_ context.Context, cfg dex.DialConfig) (dex.Client, error) {
				if cfg.Address == tenantName+".example.com:5557" {
					return fakeDexX, nil
				}
				return nil, fmt.Errorf("unexpected dial address %q", cfg.Address)
			},
		}

		_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())
		_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
		Expect(err).NotTo(HaveOccurred())

		created := fakeDexX.clients["cross-ns-backend"]
		Expect(created).NotTo(BeNil())
		Expect(created.GetSecret()).NotTo(BeEmpty())

		By("generating the client secret in the DexClient's own namespace")
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: resourceNamespace,
			Name:      resourceName + "-" + tenantNamespace + "-" + tenantName + "-secret",
		}, secret)).To(Succeed())
		Expect(string(secret.Data[generatedSecretKey])).To(Equal(created.GetSecret()))

		updated := &idpv1alpha1.DexClient{}
		Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
		Expect(updated.Status.Mounts).To(HaveLen(1))
		Expect(updated.Status.Mounts[0].Target).To(Equal("tenant:" + tenantNamespace + "/" + tenantName))
		condition := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionSynced)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionTrue))
	})
})

// createReadyTenant creates a DexConnection and a DexTenant whose status already points at it,
// simulating a DexTenant whose managed Dex instance is up and running.
// createTLSDexConnection creates a DexConnection with TLS enabled. The CRD CEL rule
// evaluates self.plaintext, so the field must be present as false; the Go omitempty
// tag would otherwise omit it and admission would reject the object.
func createTLSDexConnection(ctx context.Context, connection *idpv1alpha1.DexConnection) {
	connection.SetGroupVersionKind(idpv1alpha1.GroupVersion.WithKind("DexConnection"))
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(connection)
	Expect(err).NotTo(HaveOccurred())
	spec, ok := raw["spec"].(map[string]any)
	Expect(ok).To(BeTrue())
	spec["plaintext"] = false
	Expect(k8sClient.Create(ctx, &unstructured.Unstructured{Object: raw})).To(Succeed())
}

func createReadyTenant(ctx context.Context, namespace, tenantName string) {
	connection := &idpv1alpha1.DexConnection{
		ObjectMeta: metav1.ObjectMeta{Name: tenantName, Namespace: namespace},
		Spec: idpv1alpha1.DexConnectionSpec{
			Address:   tenantName + ".example.com:5557",
			Plaintext: true,
		},
	}
	Expect(k8sClient.Create(ctx, connection)).To(Succeed())

	tenant := &idpv1alpha1.DexTenant{
		ObjectMeta: metav1.ObjectMeta{Name: tenantName, Namespace: namespace},
		Spec: idpv1alpha1.DexTenantSpec{
			Issuer:       "https://" + tenantName + ".example.com/dex",
			ConnectorRef: corev1.LocalObjectReference{Name: tenantName + "-connector"},
		},
	}
	Expect(k8sClient.Create(ctx, tenant)).To(Succeed())
	tenant.Status.ConnectionRef = connection.Name
	Expect(k8sClient.Status().Update(ctx, tenant)).To(Succeed())
}

func newTestMultiDexClientReconciler(
	tenantA string, fakeDexA *fakeDexClient,
	tenantB string, fakeDexB *fakeDexClient,
) *DexClientReconciler {
	return &DexClientReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		ClientFactory: func(_ context.Context, cfg dex.DialConfig) (dex.Client, error) {
			switch cfg.Address {
			case tenantA + ".example.com:5557":
				return fakeDexA, nil
			case tenantB + ".example.com:5557":
				return fakeDexB, nil
			default:
				return nil, fmt.Errorf("unexpected dial address %q", cfg.Address)
			}
		},
	}
}

func newTestDexClientReconciler(fakeDex *fakeDexClient) *DexClientReconciler {
	return &DexClientReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		ClientFactory: func(context.Context, dex.DialConfig) (dex.Client, error) {
			return fakeDex, nil
		},
	}
}

type fakeDexClient struct {
	clients            map[string]*dexapi.Client
	connectors         map[string]*dexapi.Connector
	unreachable        bool
	closed             bool
	updateConnectorErr error
}

func newFakeDexClient() *fakeDexClient {
	return &fakeDexClient{
		clients:    map[string]*dexapi.Client{},
		connectors: map[string]*dexapi.Connector{},
	}
}

func (f *fakeDexClient) GetClient(_ context.Context, id string) (*dexapi.Client, error) {
	dexClient, ok := f.clients[id]
	if !ok {
		return nil, dex.ErrNotFound
	}
	return cloneDexClient(dexClient), nil
}

func (f *fakeDexClient) CreateClient(_ context.Context, dexClient *dexapi.Client) (*dexapi.Client, error) {
	if _, ok := f.clients[dexClient.GetId()]; ok {
		return nil, dex.ErrAlreadyExists
	}
	f.clients[dexClient.GetId()] = cloneDexClient(dexClient)
	return cloneDexClient(dexClient), nil
}

func (f *fakeDexClient) UpdateClient(_ context.Context, req *dexapi.UpdateClientReq) error {
	dexClient, ok := f.clients[req.GetId()]
	if !ok {
		return dex.ErrNotFound
	}
	dexClient.RedirectUris = append([]string(nil), req.GetRedirectUris()...)
	dexClient.TrustedPeers = append([]string(nil), req.GetTrustedPeers()...)
	dexClient.Name = req.GetName()
	dexClient.LogoUrl = req.GetLogoUrl()
	return nil
}

func (f *fakeDexClient) DeleteClient(_ context.Context, id string) error {
	if _, ok := f.clients[id]; !ok {
		return dex.ErrNotFound
	}
	delete(f.clients, id)
	return nil
}

func (f *fakeDexClient) GetVersion(_ context.Context) (*dexapi.VersionResp, error) {
	if f.unreachable {
		return nil, errors.New("fake dex unreachable")
	}
	return &dexapi.VersionResp{Server: "fake"}, nil
}

func (f *fakeDexClient) CreateConnector(_ context.Context, connector *dexapi.Connector) error {
	if _, ok := f.connectors[connector.GetId()]; ok {
		return dex.ErrAlreadyExists
	}
	f.connectors[connector.GetId()] = connector
	return nil
}

func (f *fakeDexClient) UpdateConnector(_ context.Context, req *dexapi.UpdateConnectorReq) error {
	if f.updateConnectorErr != nil {
		return f.updateConnectorErr
	}
	connector, ok := f.connectors[req.GetId()]
	if !ok {
		return dex.ErrNotFound
	}
	connector.Type = req.GetNewType()
	connector.Name = req.GetNewName()
	connector.Config = req.GetNewConfig()
	return nil
}

func (f *fakeDexClient) DeleteConnector(_ context.Context, id string) error {
	if _, ok := f.connectors[id]; !ok {
		return dex.ErrNotFound
	}
	delete(f.connectors, id)
	return nil
}

func (f *fakeDexClient) Close() error {
	if f.closed {
		return errors.New("fake dex client already closed")
	}
	f.closed = true
	return nil
}

func cloneDexClient(dexClient *dexapi.Client) *dexapi.Client {
	return &dexapi.Client{
		Id:           dexClient.GetId(),
		Secret:       dexClient.GetSecret(),
		RedirectUris: append([]string(nil), dexClient.GetRedirectUris()...),
		TrustedPeers: append([]string(nil), dexClient.GetTrustedPeers()...),
		Public:       dexClient.GetPublic(),
		Name:         dexClient.GetName(),
		LogoUrl:      dexClient.GetLogoUrl(),
	}
}

func cleanupDexClientTestResources(ctx context.Context) {
	const namespace = "default"

	dexClients := &idpv1alpha1.DexClientList{}
	Expect(k8sClient.List(ctx, dexClients, client.InNamespace(namespace))).To(Succeed())
	for i := range dexClients.Items {
		dexClient := &dexClients.Items[i]
		if len(dexClient.Finalizers) > 0 {
			dexClient.Finalizers = nil
			Expect(client.IgnoreNotFound(k8sClient.Update(ctx, dexClient))).To(Succeed())
		}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, dexClient))).To(Succeed())
	}

	Expect(k8sClient.DeleteAllOf(ctx, &idpv1alpha1.DexTenant{}, client.InNamespace(namespace))).To(Succeed())
	Expect(k8sClient.DeleteAllOf(ctx, &idpv1alpha1.DexConnection{}, client.InNamespace(namespace))).To(Succeed())
	Expect(k8sClient.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace(namespace))).To(Succeed())

	Eventually(func(g Gomega) {
		dexClients := &idpv1alpha1.DexClientList{}
		g.Expect(k8sClient.List(ctx, dexClients, client.InNamespace(namespace))).To(Succeed())
		g.Expect(dexClients.Items).To(BeEmpty())
	}).Should(Succeed())
}
