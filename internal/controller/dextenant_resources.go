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
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	dexapi "github.com/dexidp/dex/api/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	idpv1alpha1 "groeneslot.nl/dex-idp-operator/api/v1alpha1"
)

// dexStorageAPIGroup and dexStorageAPIVersion identify the CRD group Dex's Kubernetes storage
// backend uses to persist its state (clients, connectors, signing keys, and so on).
const (
	dexStorageAPIGroup   = "dex.coreos.com"
	dexStorageAPIVersion = "v1"
	dexConfigChecksumKey = "idp.groeneslot.nl/config-checksum"
)

// dexStorageResources lists every custom resource Dex's Kubernetes storage backend requires,
// mirroring github.com/dexidp/dex/storage/kubernetes.
var dexStorageResources = []struct {
	Plural   string
	Singular string
	Kind     string
}{
	{"authcodes", "authcode", "AuthCode"},
	{"authrequests", "authrequest", "AuthRequest"},
	{"oauth2clients", "oauth2client", "OAuth2Client"},
	{"signingkeies", "signingkey", "SigningKey"},
	{"refreshtokens", "refreshtoken", "RefreshToken"},
	{"passwords", "password", "Password"},
	{"offlinesessionses", "offlinesessions", "OfflineSessions"},
	{"connectors", "connector", "Connector"},
}

// ensureDexStorageCRDs makes sure the CRDs required by Dex's Kubernetes storage backend
// exist. These CRDs are shared cluster-wide across every DexTenant, so they are created
// without an owner reference and are never deleted by the controller.
func ensureDexStorageCRDs(ctx context.Context, c client.Client) error {
	for _, resource := range dexStorageResources {
		crd := buildDexStorageCRD(resource.Plural, resource.Singular, resource.Kind)
		existing := &apiextensionsv1.CustomResourceDefinition{}
		err := c.Get(ctx, types.NamespacedName{Name: crd.Name}, existing)
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get CRD %q: %w", crd.Name, err)
		}
		if err := c.Create(ctx, crd); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create CRD %q: %w", crd.Name, err)
		}
	}
	return nil
}

func buildDexStorageCRD(plural, singular, kind string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s.%s", plural, dexStorageAPIGroup),
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: dexStorageAPIGroup,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   plural,
				Singular: singular,
				Kind:     kind,
				ListKind: kind + "List",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    dexStorageAPIVersion,
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type:                   "object",
							XPreserveUnknownFields: ptr.To(true),
						},
					},
				},
			},
		},
	}
}

// dexTenantResourceName is the shared name used for every namespaced resource the operator
// creates on behalf of a DexTenant (ServiceAccount, Role, RoleBinding, ConfigMap, Deployment,
// and Service).
func dexTenantResourceName(tenant *idpv1alpha1.DexTenant) string {
	return tenant.Name + "-dex"
}

func dexTenantLabels(tenant *idpv1alpha1.DexTenant) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "dex",
		"app.kubernetes.io/instance": tenant.Name,
	}
}

func (r *DexTenantReconciler) ensureServiceAccount(ctx context.Context, tenant *idpv1alpha1.DexTenant) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: dexTenantResourceName(tenant), Namespace: tenant.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		return controllerutil.SetControllerReference(tenant, sa, r.Scheme)
	})
	return err
}

func (r *DexTenantReconciler) ensureStorageRBAC(ctx context.Context, tenant *idpv1alpha1.DexTenant) error {
	plurals := make([]string, len(dexStorageResources))
	for i, resource := range dexStorageResources {
		plurals[i] = resource.Plural
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: dexTenantResourceName(tenant), Namespace: tenant.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups: []string{dexStorageAPIGroup},
				Resources: plurals,
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
		}
		return controllerutil.SetControllerReference(tenant, role, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensure Role: %w", err)
	}

	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: dexTenantResourceName(tenant), Namespace: tenant.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: dexTenantResourceName(tenant)}
		binding.Subjects = []rbacv1.Subject{
			{Kind: rbacv1.ServiceAccountKind, Name: dexTenantResourceName(tenant), Namespace: tenant.Namespace},
		}
		return controllerutil.SetControllerReference(tenant, binding, r.Scheme)
	}); err != nil {
		return fmt.Errorf("ensure RoleBinding: %w", err)
	}

	return nil
}

func dexTenantConfig(tenant *idpv1alpha1.DexTenant) string {
	config := fmt.Sprintf(`issuer: %s
storage:
  type: kubernetes
  config:
    inCluster: true
web:
  http: 0.0.0.0:5556
grpc:
  addr: 0.0.0.0:5557
`, tenant.Spec.Issuer)
	if tenant.Spec.OAuth2 != nil {
		config += fmt.Sprintf(`oauth2:
  skipApprovalScreen: %t
`, tenant.Spec.OAuth2.SkipApprovalScreen)
	}
	return config
}

func (r *DexTenantReconciler) ensureConfigMap(ctx context.Context, tenant *idpv1alpha1.DexTenant) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: dexTenantResourceName(tenant), Namespace: tenant.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{"config.yaml": dexTenantConfig(tenant)}
		return controllerutil.SetControllerReference(tenant, cm, r.Scheme)
	})
	return err
}

func (r *DexTenantReconciler) ensureDeployment(ctx context.Context, tenant *idpv1alpha1.DexTenant) error {
	image := tenant.Spec.Image
	if image == "" {
		image = idpv1alpha1.DefaultDexImage
	}
	replicas := tenant.Spec.Replicas
	if replicas == nil {
		replicas = ptr.To(int32(1))
	}
	labels := dexTenantLabels(tenant)
	name := dexTenantResourceName(tenant)
	configChecksum := fmt.Sprintf("%x", sha256.Sum256([]byte(dexTenantConfig(tenant))))

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tenant.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		deployment.Spec = appsv1.DeploymentSpec{
			Replicas: replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{dexConfigChecksumKey: configChecksum},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: name,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{
						{
							Name:    "dex",
							Image:   image,
							Command: []string{"dex", "serve", "/etc/dex/config.yaml"},
							Env: []corev1.EnvVar{
								{Name: "DEX_API_CONNECTORS_CRUD", Value: "true"},
							},
							Ports: []corev1.ContainerPort{
								{Name: "http", ContainerPort: 5556},
								{Name: "grpc", ContainerPort: 5557},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "config", MountPath: "/etc/dex", ReadOnly: true},
							},
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: ptr.To(false),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: name},
								},
							},
						},
					},
				},
			},
		}
		return controllerutil.SetControllerReference(tenant, deployment, r.Scheme)
	})
	return err
}

func (r *DexTenantReconciler) ensureService(ctx context.Context, tenant *idpv1alpha1.DexTenant) error {
	name := dexTenantResourceName(tenant)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tenant.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Spec.Selector = dexTenantLabels(tenant)
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: "http", Port: 5556, TargetPort: intstr.FromString("http")},
			{Name: "grpc", Port: 5557, TargetPort: intstr.FromString("grpc")},
		}
		return controllerutil.SetControllerReference(tenant, svc, r.Scheme)
	})
	return err
}

func (r *DexTenantReconciler) ensureDexConnection(ctx context.Context, tenant *idpv1alpha1.DexTenant) (*idpv1alpha1.DexConnection, error) {
	address := fmt.Sprintf("%s.%s.svc.cluster.local:5557", dexTenantResourceName(tenant), tenant.Namespace)
	connection := &idpv1alpha1.DexConnection{
		ObjectMeta: metav1.ObjectMeta{Name: tenant.Name, Namespace: tenant.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, connection, func() error {
		connection.Spec.Address = address
		connection.Spec.Plaintext = true
		connection.Spec.TLS = nil
		return controllerutil.SetControllerReference(tenant, connection, r.Scheme)
	})
	return connection, err
}

// dexStorageConnectorGVK identifies the Connector custom resource Dex's Kubernetes storage
// backend reads from and writes to.
var dexStorageConnectorGVK = schema.GroupVersionKind{
	Group:   dexStorageAPIGroup,
	Version: dexStorageAPIVersion,
	Kind:    "Connector",
}

// preseedConnector creates the Dex storage Connector object for connector if it does not
// already exist. Once created, the operator never overwrites it directly again; subsequent
// changes are applied through Dex's gRPC connector API while Dex is running.
func (r *DexTenantReconciler) preseedConnector(
	ctx context.Context,
	tenant *idpv1alpha1.DexTenant,
	connector *dexapi.Connector,
) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(dexStorageConnectorGVK)
	key := types.NamespacedName{Namespace: tenant.Namespace, Name: connector.GetId()}
	if err := r.Get(ctx, key, existing); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get pre-seeded connector %q: %w", connector.GetId(), err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(dexStorageConnectorGVK)
	obj.SetName(connector.GetId())
	obj.SetNamespace(tenant.Namespace)
	if err := controllerutil.SetControllerReference(tenant, obj, r.Scheme); err != nil {
		return err
	}

	fields := map[string]any{
		"id":     connector.GetId(),
		"type":   connector.GetType(),
		"name":   connector.GetName(),
		"config": base64.StdEncoding.EncodeToString(connector.GetConfig()),
	}
	for k, v := range fields {
		if err := unstructured.SetNestedField(obj.Object, v, k); err != nil {
			return fmt.Errorf("set connector field %q: %w", k, err)
		}
	}

	if err := r.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create pre-seeded connector %q: %w", connector.GetId(), err)
	}
	return nil
}
