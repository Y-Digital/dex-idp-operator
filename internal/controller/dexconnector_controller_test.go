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
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	idpv1alpha1 "groeneslot.nl/dex-idp-operator/api/v1alpha1"
)

var _ = Describe("DexConnector Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-oidc-connector"
			resourceNamespace = "default"
			secretName        = "connector-secret"
		)

		ctx := context.Background()
		typeNamespacedName := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}

		BeforeEach(func() {
			cleanupDexConnectorTestResources(ctx, resourceNamespace)

			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: resourceNamespace},
				Data:       map[string][]byte{"clientSecret": []byte("shhh")},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		})

		AfterEach(func() {
			cleanupDexConnectorTestResources(ctx, resourceNamespace)
		})

		It("resolves secretRefs and reports Ready", func() {
			raw := `{"issuer":"https://idp.example.com","clientID":"abc","clientSecret":"${oidcSecret}"}`
			connector := &idpv1alpha1.DexConnector{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec: idpv1alpha1.DexConnectorSpec{
					ConnectorID: "test-oidc",
					Type:        "oidc",
					DisplayName: "Test OIDC",
					Config:      runtime.RawExtension{Raw: []byte(raw)},
					SecretRefs: []idpv1alpha1.DexConnectorSecretRef{
						{
							Name: "oidcSecret",
							ValueFrom: idpv1alpha1.SecretValueSource{
								SecretKeyRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
									Key:                  "clientSecret",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, connector)).To(Succeed())

			controllerReconciler := &DexConnectorReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			updated := &idpv1alpha1.DexConnector{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			condition := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))

			resolved, err := resolveDexConnector(ctx, k8sClient, resourceNamespace, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved.GetId()).To(Equal("test-oidc"))
			Expect(resolved.GetType()).To(Equal("oidc"))
			Expect(resolved.GetName()).To(Equal("Test OIDC"))
			Expect(string(resolved.GetConfig())).To(ContainSubstring(`"clientSecret":"shhh"`))
			Expect(string(resolved.GetConfig())).NotTo(ContainSubstring("oidcSecret"))
		})

		It("rejects resolved config that is not a JSON object", func() {
			connector := &idpv1alpha1.DexConnector{
				Spec: idpv1alpha1.DexConnectorSpec{
					ConnectorID: "broken-json",
					Type:        "oidc",
					DisplayName: "Broken",
					Config:      runtime.RawExtension{Raw: []byte(`not-a-json-object`)},
				},
			}
			_, err := resolveDexConnector(ctx, k8sClient, resourceNamespace, connector)
			Expect(err).To(MatchError(ContainSubstring("not a valid JSON object")))
		})

		It("falls back to connectorID when displayName is empty", func() {
			connector := &idpv1alpha1.DexConnector{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec: idpv1alpha1.DexConnectorSpec{
					ConnectorID: "fallback-id",
					Type:        "oidc",
					Config:      runtime.RawExtension{Raw: []byte(`{"issuer":"https://idp.example.com"}`)},
				},
			}
			Expect(k8sClient.Create(ctx, connector)).To(Succeed())

			controllerReconciler := &DexConnectorReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			updated := &idpv1alpha1.DexConnector{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			resolved, err := resolveDexConnector(ctx, k8sClient, resourceNamespace, updated)
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved.GetName()).To(Equal("fallback-id"))
		})

		It("escapes quotes and newlines for embedding inside JSON strings", func() {
			Expect(string(jsonEscapeStringContent("say \"hello\"\nnext"))).To(Equal(`say \"hello\"\nnext`))
		})

		It("JSON-escapes secret values so interpolated config stays valid", func() {
			special := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: secretName, Namespace: resourceNamespace,
			}, special)).To(Succeed())
			special.Data["clientSecret"] = []byte("say \"hello\"\nnext")
			Expect(k8sClient.Update(ctx, special)).To(Succeed())

			raw := `{"issuer":"https://idp.example.com","clientSecret":"${oidcSecret}"}`
			connector := &idpv1alpha1.DexConnector{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec: idpv1alpha1.DexConnectorSpec{
					ConnectorID: "escaped-oidc",
					Type:        "oidc",
					DisplayName: "Escaped OIDC",
					Config:      runtime.RawExtension{Raw: []byte(raw)},
					SecretRefs: []idpv1alpha1.DexConnectorSecretRef{
						{
							Name: "oidcSecret",
							ValueFrom: idpv1alpha1.SecretValueSource{
								SecretKeyRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
									Key:                  "clientSecret",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, connector)).To(Succeed())

			controllerReconciler := &DexConnectorReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			updated := &idpv1alpha1.DexConnector{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			condition := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))

			resolved, err := resolveDexConnector(ctx, k8sClient, resourceNamespace, updated)
			Expect(err).NotTo(HaveOccurred())
			parsed := map[string]any{}
			Expect(json.Unmarshal(resolved.GetConfig(), &parsed)).To(Succeed())
			Expect(parsed["clientSecret"]).To(Equal("say \"hello\"\nnext"))
		})

		It("reports NotReady when a secretRef cannot be resolved", func() {
			raw := `{"issuer":"https://idp.example.com","clientSecret":"${missingSecret}"}`
			connector := &idpv1alpha1.DexConnector{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec: idpv1alpha1.DexConnectorSpec{
					ConnectorID: "broken",
					Type:        "oidc",
					Config:      runtime.RawExtension{Raw: []byte(raw)},
					SecretRefs: []idpv1alpha1.DexConnectorSecretRef{
						{
							Name: "missingSecret",
							ValueFrom: idpv1alpha1.SecretValueSource{
								SecretKeyRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "does-not-exist"},
									Key:                  "value",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, connector)).To(Succeed())

			controllerReconciler := &DexConnectorReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			updated := &idpv1alpha1.DexConnector{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			condition := meta.FindStatusCondition(updated.Status.Conditions, idpv1alpha1.ConditionReady)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal("ConfigInvalid"))
		})
	})
})

func cleanupDexConnectorTestResources(ctx context.Context, namespace string) {
	Expect(k8sClient.DeleteAllOf(ctx, &idpv1alpha1.DexConnector{}, client.InNamespace(namespace))).To(Succeed())
	Expect(k8sClient.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace(namespace))).To(Succeed())

	Eventually(func(g Gomega) {
		connectors := &idpv1alpha1.DexConnectorList{}
		g.Expect(k8sClient.List(ctx, connectors, client.InNamespace(namespace))).To(Succeed())
		g.Expect(connectors.Items).To(BeEmpty())
	}).Should(Succeed())
}
