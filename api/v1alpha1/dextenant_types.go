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

// DefaultDexImage is the container image used for a managed tenant Dex deployment when
// spec.image is not set.
const DefaultDexImage = "ghcr.io/dexidp/dex:v2.43.1"

// DexTenantSpec defines the desired state of a tenant-scoped, operator-managed Dex instance.
//
// A DexTenant owns one Dex Deployment, Service, and Kubernetes-storage-backed issuer. The
// tenant's issuer URL is what a backend application targets to authenticate against the
// specific upstream IDP configured for that tenant, via the referenced DexConnector.
type DexTenantSpec struct {
	// issuer is the externally reachable issuer URL for this tenant's managed Dex instance.
	// Backend applications select the tenant, and therefore the upstream IDP, by directing
	// users to this issuer. The operator does not create Ingress or Gateway routes; the
	// issuer must be made reachable at this URL by other means.
	// +kubebuilder:validation:MinLength=1
	// +required
	Issuer string `json:"issuer"`

	// connectorRef references the DexConnector used to authenticate users for this tenant.
	// Exactly one connector is supported per tenant.
	// +required
	ConnectorRef corev1.LocalObjectReference `json:"connectorRef"`

	// image is the Dex container image used for the managed Dex deployment. Defaults to
	// DefaultDexImage when unset.
	// +optional
	Image string `json:"image,omitempty"`

	// replicas is the desired number of managed Dex pod replicas.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
}

// DexTenantStatus defines the observed state of DexTenant.
type DexTenantStatus struct {
	// observedGeneration is the last generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// connectionRef is the name of the DexConnection the operator manages for reaching this
	// tenant's Dex gRPC endpoint.
	// +optional
	ConnectionRef string `json:"connectionRef,omitempty"`

	// serviceName is the name of the Kubernetes Service exposing this tenant's Dex instance.
	// +optional
	ServiceName string `json:"serviceName,omitempty"`

	// conditions represent the current state of the DexTenant resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Issuer",type=string,JSONPath=".spec.issuer"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"

// DexTenant is the Schema for the dextenants API
type DexTenant struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of DexTenant
	// +required
	Spec DexTenantSpec `json:"spec"`

	// status defines the observed state of DexTenant
	// +optional
	Status DexTenantStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DexTenantList contains a list of DexTenant
type DexTenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DexTenant `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DexTenant{}, &DexTenantList{})
		return nil
	})
}
