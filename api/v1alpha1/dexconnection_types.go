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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// ConditionReady reports whether the resource is configured and usable.
	ConditionReady = "Ready"
)

// ValueSource contains either an inline value or a Secret key reference.
// +kubebuilder:validation:XValidation:rule="has(self.value) != has(self.valueFrom)",message="exactly one of value or valueFrom must be set"
type ValueSource struct {
	// value is the inline value.
	// +optional
	Value *string `json:"value,omitempty"`

	// valueFrom references a Kubernetes Secret key containing the value.
	// +optional
	ValueFrom *SecretValueSource `json:"valueFrom,omitempty"`
}

// SecretValueSource selects a value from a Secret in the resource namespace.
type SecretValueSource struct {
	// secretKeyRef selects the Secret key containing the value.
	// +required
	SecretKeyRef corev1.SecretKeySelector `json:"secretKeyRef"`
}

// DexConnectionTLSConfig defines TLS settings for the Dex gRPC connection.
type DexConnectionTLSConfig struct {
	// ca contains PEM-encoded CA certificate data used to verify Dex.
	// +optional
	CA *ValueSource `json:"ca,omitempty"`

	// clientCertificate contains PEM-encoded mTLS client certificate material.
	// +optional
	ClientCertificate *DexConnectionClientCertificate `json:"clientCertificate,omitempty"`

	// insecureSkipVerify skips Dex server certificate verification.
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`

	// serverName overrides the TLS server name used to verify Dex.
	// +optional
	ServerName string `json:"serverName,omitempty"`
}

// DexConnectionClientCertificate contains mTLS client certificate material.
type DexConnectionClientCertificate struct {
	// certificate contains PEM-encoded client certificate data.
	// +required
	Certificate ValueSource `json:"certificate"`

	// privateKey contains PEM-encoded client private key data.
	// +required
	PrivateKey ValueSource `json:"privateKey"`
}

// DexConnectionSpec defines the desired Dex gRPC endpoint.
// +kubebuilder:validation:XValidation:rule="!self.plaintext || !has(self.tls)",message="tls must not be set when plaintext is true"
type DexConnectionSpec struct {
	// address is the Dex gRPC host:port endpoint.
	// +kubebuilder:validation:MinLength=1
	// +required
	Address string `json:"address"`

	// plaintext disables TLS for local development or trusted in-cluster links.
	// +optional
	Plaintext bool `json:"plaintext,omitempty"`

	// tls defines TLS configuration for the Dex gRPC connection.
	// +optional
	TLS *DexConnectionTLSConfig `json:"tls,omitempty"`
}

// DexConnectionStatus defines the observed state of DexConnection.
type DexConnectionStatus struct {
	// observedGeneration is the last generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current state of the DexConnection resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// DexConnection is the Schema for the dexconnections API
type DexConnection struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of DexConnection
	// +required
	Spec DexConnectionSpec `json:"spec"`

	// status defines the observed state of DexConnection
	// +optional
	Status DexConnectionStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DexConnectionList contains a list of DexConnection
type DexConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DexConnection `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DexConnection{}, &DexConnectionList{})
		return nil
	})
}
