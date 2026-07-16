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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DexConnectorSecretRef resolves a Secret value that can be interpolated into
// a DexConnector config using a "${name}" placeholder.
type DexConnectorSecretRef struct {
	// name is the placeholder referenced from config as "${name}".
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// valueFrom selects the Secret key containing the value.
	// +required
	ValueFrom SecretValueSource `json:"valueFrom"`
}

// DexConnectorSpec defines the desired Dex connector configuration.
type DexConnectorSpec struct {
	// connectorID is the Dex connector ID. Dex's Kubernetes storage backend uses this value
	// directly as the name of the underlying Kubernetes object, so it must be a valid
	// Kubernetes object name.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=253
	// +required
	ConnectorID string `json:"connectorID"`

	// type is the Dex connector type, for example "oidc", "ldap", "github", or "saml".
	// +kubebuilder:validation:MinLength=1
	// +required
	Type string `json:"type"`

	// displayName is the human-readable connector name shown by Dex. Defaults to connectorID
	// when unset.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// config is the raw Dex connector configuration, using the connector type's native JSON
	// schema. String values may embed "${name}" placeholders that reference secretRefs
	// entries; the controller substitutes the resolved Secret values before the
	// configuration is applied to Dex.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	// +required
	Config runtime.RawExtension `json:"config"`

	// secretRefs resolve Secret values that can be interpolated into config using "${name}"
	// placeholders.
	// +optional
	SecretRefs []DexConnectorSecretRef `json:"secretRefs,omitempty"`
}

// DexConnectorStatus defines the observed state of DexConnector.
type DexConnectorStatus struct {
	// observedGeneration is the last generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current state of the DexConnector resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// DexConnector is the Schema for the dexconnectors API
type DexConnector struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of DexConnector
	// +required
	Spec DexConnectorSpec `json:"spec"`

	// status defines the observed state of DexConnector
	// +optional
	Status DexConnectorStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DexConnectorList contains a list of DexConnector
type DexConnectorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DexConnector `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DexConnector{}, &DexConnectorList{})
		return nil
	})
}
