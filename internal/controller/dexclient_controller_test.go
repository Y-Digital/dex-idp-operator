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

	dexapi "github.com/dexidp/dex/api/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	idpv1alpha1 "groeneslot.nl/dex-idp-operator/api/v1alpha1"
	"groeneslot.nl/dex-idp-operator/internal/dex"
)

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
			cleanupDexClientTestResources(ctx, resourceNamespace)
			fakeDex = newFakeDexClient()

			By("creating the DexConnection")
			connection := &idpv1alpha1.DexConnection{
				ObjectMeta: metav1.ObjectMeta{
					Name:      connectionName,
					Namespace: resourceNamespace,
				},
				Spec: idpv1alpha1.DexConnectionSpec{
					Address:   "dex.example.com:5557",
					Plaintext: true,
				},
			}
			Expect(k8sClient.Create(ctx, connection)).To(Succeed())

			By("creating the client secret")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "client-secret",
					Namespace: resourceNamespace,
				},
				Data: map[string][]byte{
					"secret": []byte(secretValue),
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
					ConnectionRef: corev1.LocalObjectReference{Name: connectionName},
					ClientID:      "example-app",
					Secret: &idpv1alpha1.ValueSource{
						ValueFrom: &idpv1alpha1.SecretValueSource{
							SecretKeyRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "client-secret"},
								Key:                  "secret",
							},
						},
					},
					RedirectURIs: []string{"https://app.example.com/callback"},
					Name:         "Example App",
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(func() {
			cleanupDexClientTestResources(ctx, resourceNamespace)
		})

		It("creates a missing Dex client from SecretRef values", func() {
			controllerReconciler := newTestDexClientReconciler(fakeDex)

			By("adding the finalizer")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("creating the Dex client")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			created := fakeDex.clients["example-app"]
			Expect(created).NotTo(BeNil())
			Expect(created.GetSecret()).To(Equal(secretValue))
			Expect(created.GetRedirectUris()).To(Equal([]string{"https://app.example.com/callback"}))

			updated := &idpv1alpha1.DexClient{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			condition := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionSynced)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
		})

		It("blocks immutable secret changes unless recreation is enabled", func() {
			fakeDex.clients["example-app"] = &dexapi.Client{
				Id:           "example-app",
				Secret:       "old-secret",
				RedirectUris: []string{"https://app.example.com/callback"},
				Name:         "Example App",
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
			Expect(fakeDex.clients["example-app"].GetSecret()).To(Equal("old-secret"))
		})

		It("deletes the Dex client during finalizer cleanup", func() {
			fakeDex.clients["example-app"] = &dexapi.Client{Id: "example-app", Secret: secretValue}
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
			Expect(fakeDex.clients).NotTo(HaveKey("example-app"))
		})
	})
})

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
	clients map[string]*dexapi.Client
	closed  bool
}

func newFakeDexClient() *fakeDexClient {
	return &fakeDexClient{clients: map[string]*dexapi.Client{}}
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

func cleanupDexClientTestResources(ctx context.Context, namespace string) {
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

	Expect(k8sClient.DeleteAllOf(ctx, &idpv1alpha1.DexConnection{}, client.InNamespace(namespace))).To(Succeed())
	Expect(k8sClient.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace(namespace))).To(Succeed())

	Eventually(func(g Gomega) {
		dexClient := &idpv1alpha1.DexClient{}
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "test-client"}, dexClient)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	}).Should(Succeed())
}
