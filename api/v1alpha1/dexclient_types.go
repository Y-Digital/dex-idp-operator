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
// +kubebuilder:validation:XValidation:rule="(has(self.public) && self.public) || has(self.secret) || (has(self.tenantRefs) && size(self.tenantRefs) > 0)",message="secret is required when public is false, unless tenantRefs is set"
// +kubebuilder:validation:XValidation:rule="!(has(self.public) && self.public) || !has(self.secret)",message="secret must not be set when public is true"
// +kubebuilder:validation:XValidation:rule="has(self.connectionRef) != (has(self.tenantRefs) && size(self.tenantRefs) > 0)",message="exactly one of connectionRef or tenantRefs must be set"
// +kubebuilder:validation:XValidation:rule="!(has(self.tenantRefs) && size(self.tenantRefs) > 0) || !has(self.secret)",message="secret must not be set when tenantRefs is set; the operator generates a distinct secret per tenant"
type DexClientSpec struct {
	// connectionRef references the DexConnection used to reach Dex. Mutually exclusive with
	// tenantRefs.
	// +optional
	ConnectionRef *corev1.LocalObjectReference `json:"connectionRef,omitempty"`

	// tenantRefs mount this client onto multiple tenant-scoped Dex contexts. Each referenced
	// DexTenant runs its own Dex instance with its own upstream IDP connector, so the same
	// OAuth2 client can authenticate against different IDPs depending on which tenant issuer
	// a request targets. Mutually exclusive with connectionRef. Confidential clients
	// (public=false) mounted this way receive a distinct, operator-generated client secret
	// per tenant.
	// +optional
	TenantRefs []DexTenantReference `json:"tenantRefs,omitempty"`

	// clientID is the Dex OAuth2 client identifier.
	// +kubebuilder:validation:MinLength=1
	// +required
	ClientID string `json:"clientID"`

	// secret contains the client secret for confidential clients. Only valid together with
	// connectionRef; when tenantRefs is set, the operator generates a distinct secret per
	// tenant instead.
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

// DexTenantReference identifies a DexTenant that a DexClient mounts onto. Dex's Kubernetes
// storage backend scopes all state, including which connectors are offered for login, to the
// Kubernetes namespace a Dex instance runs in. Isolating one tenant's upstream IDP from
// another's therefore requires running each DexTenant in its own namespace, so unlike most
// same-object references in this API, tenantRefs must be able to cross namespaces.
type DexTenantReference struct {
	// name is the name of the referenced DexTenant.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// namespace is the namespace of the referenced DexTenant. Defaults to the DexClient's own
	// namespace when unset.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// DexClientMountStatus reports the sync status of a DexClient against one mount target,
// either the single connectionRef target or one entry from tenantRefs.
type DexClientMountStatus struct {
	// target identifies the mount target, formatted as "connection:<name>" or
	// "tenant:<namespace>/<name>".
	// +required
	Target string `json:"target"`

	// secretRef is the name of the Secret holding the client secret used for this target,
	// when applicable.
	// +optional
	SecretRef string `json:"secretRef,omitempty"`

	// ready reports whether this target is synced with Dex.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// reason is a short machine-readable reason for the current status.
	// +optional
	Reason string `json:"reason,omitempty"`

	// message is a human-readable message describing the current status.
	// +optional
	Message string `json:"message,omitempty"`
}

// DexClientStatus defines the observed state of DexClient.
type DexClientStatus struct {
	// observedGeneration is the last generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// clientID is the Dex client identifier last observed by the controller.
	// +optional
	ClientID string `json:"clientID,omitempty"`

	// mounts reports the sync status of this client against each mount target.
	// +listType=map
	// +listMapKey=target
	// +optional
	Mounts []DexClientMountStatus `json:"mounts,omitempty"`

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
