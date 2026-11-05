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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	idpv1alpha1 "idp.y.digital/dex-idp-operator/api/v1alpha1"
)

const secretKey = "token"

var _ = Describe("ValueSource", func() {
	const (
		namespace  = "default"
		secretName = "value-source-secret"
	)

	ctx := context.Background()

	BeforeEach(func() {
		Expect(k8sClient.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace(namespace))).To(Succeed())
		Eventually(func(g Gomega) {
			secrets := &corev1.SecretList{}
			g.Expect(k8sClient.List(ctx, secrets, client.InNamespace(namespace))).To(Succeed())
			g.Expect(secrets.Items).To(BeEmpty())
		}).Should(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.DeleteAllOf(ctx, &corev1.Secret{}, client.InNamespace(namespace))).To(Succeed())
	})

	It("rejects a nil source", func() {
		_, err := resolveValueSource(ctx, k8sClient, namespace, nil)
		Expect(err).To(MatchError(ContainSubstring("value source is required")))
	})

	It("returns an inline value", func() {
		value := "inline-token"
		got, err := resolveValueSource(ctx, k8sClient, namespace, &idpv1alpha1.ValueSource{Value: &value})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal(value))
	})

	It("rejects a source with neither value nor valueFrom", func() {
		_, err := resolveValueSource(ctx, k8sClient, namespace, &idpv1alpha1.ValueSource{})
		Expect(err).To(MatchError(ContainSubstring("value or valueFrom is required")))
	})

	It("returns nil for an optional Secret that does not exist", func() {
		optional := true
		got, err := resolveValueSource(ctx, k8sClient, namespace, secretKeySource(secretName, &optional))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeNil())
	})

	It("returns nil for an optional missing key", func() {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace},
			Data:       map[string][]byte{"other": []byte("nope")},
		})).To(Succeed())

		optional := true
		got, err := resolveValueSource(ctx, k8sClient, namespace, secretKeySource(secretName, &optional))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeNil())
	})

	It("errors when a required Secret key is missing", func() {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace},
			Data:       map[string][]byte{"other": []byte("nope")},
		})).To(Succeed())

		_, err := resolveValueSource(ctx, k8sClient, namespace, secretKeySource(secretName, nil))
		Expect(err).To(MatchError(ContainSubstring("does not contain key")))
	})

	It("matches Secret references by name", func() {
		Expect(valueSourceReferencesSecret(nil, secretName)).To(BeFalse())
		Expect(valueSourceReferencesSecret(&idpv1alpha1.ValueSource{}, secretName)).To(BeFalse())
		value := "inline"
		Expect(valueSourceReferencesSecret(&idpv1alpha1.ValueSource{Value: &value}, secretName)).To(BeFalse())
		Expect(valueSourceReferencesSecret(secretKeySource(secretName, nil), secretName)).To(BeTrue())
		Expect(valueSourceReferencesSecret(secretKeySource("other-secret", nil), secretName)).To(BeFalse())
	})
})

func secretKeySource(name string, optional *bool) *idpv1alpha1.ValueSource {
	return &idpv1alpha1.ValueSource{
		ValueFrom: &idpv1alpha1.SecretValueSource{
			SecretKeyRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: name},
				Key:                  secretKey,
				Optional:             optional,
			},
		},
	}
}
