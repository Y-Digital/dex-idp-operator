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
	// ConditionSynced reports whether the desired client has been applied to Dex.
	ConditionSynced = "Synced"
	// ConditionBlocked reports whether reconciliation is blocked by an unsafe requested change.
	ConditionBlocked = "Blocked"
)

// DexClientSpec defines the desired Dex OAuth2 client.
// +kubebuilder:validation:XValidation:rule="(has(self.public) && self.public) || has(self.secret)",message="secret is required when public is false"
// +kubebuilder:validation:XValidation:rule="!(has(self.public) && self.public) || !has(self.secret)",message="secret must not be set when public is true"
type DexClientSpec struct {
	// connectionRef references the DexConnection used to reach Dex.
	// +required
	ConnectionRef corev1.LocalObjectReference `json:"connectionRef"`

	// clientID is the Dex OAuth2 client identifier.
	// +kubebuilder:validation:MinLength=1
	// +required
	ClientID string `json:"clientID"`

	// secret contains the client secret for confidential clients.
	// +optional
	Secret *ValueSource `json:"secret,omitempty"`

	// public marks the client as public and disables client secret usage.
	// +optional
	Public bool `json:"public,omitempty"`

	// redirectURIs are OAuth2 redirect URIs accepted by Dex.
	// +optional
	RedirectURIs []string `json:"redirectURIs,omitempty"`

	// trustedPeers are Dex client IDs trusted by this client.
	// +optional
	TrustedPeers []string `json:"trustedPeers,omitempty"`

	// name is the human-readable client name shown by Dex.
	// +optional
	Name string `json:"name,omitempty"`

	// logoURL is the URL to a client logo shown by Dex.
	// +optional
	LogoURL string `json:"logoURL,omitempty"`

	// recreateOnImmutableChange allows the controller to delete and recreate the
	// Dex client when immutable Dex fields such as secret or public change.
	// +optional
	RecreateOnImmutableChange bool `json:"recreateOnImmutableChange,omitempty"`
}

// DexClientStatus defines the observed state of DexClient.
type DexClientStatus struct {
	// observedGeneration is the last generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// clientID is the Dex client identifier last observed by the controller.
	// +optional
	ClientID string `json:"clientID,omitempty"`

	// conditions represent the current state of the DexClient resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// DexClient is the Schema for the dexclients API
type DexClient struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of DexClient
	// +required
	Spec DexClientSpec `json:"spec"`

	// status defines the observed state of DexClient
	// +optional
	Status DexClientStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DexClientList contains a list of DexClient
type DexClientList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DexClient `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DexClient{}, &DexClientList{})
		return nil
	})
}
