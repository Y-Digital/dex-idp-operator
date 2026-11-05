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
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/big"
	"strconv"

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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	idpv1alpha1 "idp.y.digital/dex-idp-operator/api/v1alpha1"
)

// dexStorageAPIGroup and dexStorageAPIVersion identify the CRD group Dex's Kubernetes storage
// backend uses to persist its state (clients, connectors, signing keys, and so on).
const (
	dexStorageAPIGroup   = "dex.coreos.com"
	dexStorageAPIVersion = "v1"
	dexConfigChecksumKey = "idp.y.digital/config-checksum"
	dexAppName           = "dex"
	dexConfigVolumeName  = "config"
	// dexContainerUserID is the UID/GID of the dex user in official Dex images
	// (dexidp/dex Dockerfile user-setup stage). Dex 2.45+ uses USER dex:dex
	// instead of USER 1001:1001; kubelet cannot verify runAsNonRoot against a
	// named image user, so the pod must set a numeric runAsUser.
	dexContainerUserID int64 = 1001

	// dexSessionCookieKey is the Secret data key and Dex config field for the AES key
	// that encrypts the browser session cookie.
	dexSessionCookieKey = "cookieEncryptionKey"
	// dexSessionsEnabledEnv is the Dex feature flag that turns on browser sessions.
	dexSessionsEnabledEnv = "DEX_SESSIONS_ENABLED"
	// dexSessionKeyAlphabet excludes '$' so a generated key is not expanded if Dex
	// later treats the rendered config value as an environment reference.
	dexSessionKeyAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
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
	{"devicerequests", "devicerequest", "DeviceRequest"},
	{"devicetokens", "devicetoken", "DeviceToken"},
	{"useridentities", "useridentity", "UserIdentity"},
	{"authsessions", "authsession", "AuthSession"},
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
							XPreserveUnknownFields: new(true),
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
		"app.kubernetes.io/name":     dexAppName,
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

type dexOAuth2YAML struct {
	SkipApprovalScreen bool     `json:"skipApprovalScreen"`
	GrantTypes         []string `json:"grantTypes,omitempty"`
}

type dexSessionsYAML struct {
	CookieName                 string `json:"cookieName,omitempty"`
	AbsoluteLifetime           string `json:"absoluteLifetime,omitempty"`
	ValidIfNotUsedFor          string `json:"validIfNotUsedFor,omitempty"`
	RememberMeCheckedByDefault *bool  `json:"rememberMeCheckedByDefault,omitempty"`
	CookieEncryptionKey        string `json:"cookieEncryptionKey"`
	SSOSharedWithDefault       string `json:"ssoSharedWithDefault,omitempty"`
}

func dexTenantConfig(tenant *idpv1alpha1.DexTenant, cookieEncryptionKey string) (string, error) {
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
		block, err := yaml.Marshal(map[string]dexOAuth2YAML{
			"oauth2": {
				SkipApprovalScreen: tenant.Spec.OAuth2.SkipApprovalScreen,
				GrantTypes:         tenant.Spec.OAuth2.GrantTypes,
			},
		})
		if err != nil {
			config += fmt.Sprintf("oauth2:\n  skipApprovalScreen: %t\n", tenant.Spec.OAuth2.SkipApprovalScreen)
		} else {
			config += string(block)
		}
	}
	if tenant.Spec.Sessions == nil {
		return config, nil
	}
	if err := validateCookieEncryptionKey(cookieEncryptionKey); err != nil {
		return "", err
	}
	block, err := yaml.Marshal(map[string]dexSessionsYAML{
		"sessions": sessionConfigYAML(tenant.Spec.Sessions, cookieEncryptionKey),
	})
	if err != nil {
		return "", fmt.Errorf("marshal sessions config: %w", err)
	}
	return config + string(block), nil
}

func sessionConfigYAML(sessions *idpv1alpha1.DexTenantSessionConfig, cookieEncryptionKey string) dexSessionsYAML {
	rendered := dexSessionsYAML{
		CookieName:                 sessions.CookieName,
		RememberMeCheckedByDefault: sessions.RememberMeCheckedByDefault,
		CookieEncryptionKey:        cookieEncryptionKey,
		SSOSharedWithDefault:       sessions.SSOSharedWithDefault,
	}
	if sessions.AbsoluteLifetime != nil {
		rendered.AbsoluteLifetime = sessions.AbsoluteLifetime.Duration.String()
	}
	if sessions.ValidIfNotUsedFor != nil {
		rendered.ValidIfNotUsedFor = sessions.ValidIfNotUsedFor.Duration.String()
	}
	return rendered
}

func validateCookieEncryptionKey(key string) error {
	switch len(key) {
	case 16, 24, 32:
		return nil
	default:
		return fmt.Errorf("cookie encryption key must be 16, 24, or 32 bytes, got %d", len(key))
	}
}

func dexSessionSecretName(tenant *idpv1alpha1.DexTenant) string {
	return dexTenantResourceName(tenant) + "-session"
}

func (r *DexTenantReconciler) resolveSessionCookieKey(ctx context.Context, tenant *idpv1alpha1.DexTenant) (string, error) {
	if tenant.Spec.Sessions == nil {
		return "", nil
	}
	if ref := tenant.Spec.Sessions.CookieEncryptionKeySecretRef; ref != nil {
		return readCookieEncryptionKey(ctx, r.Client, tenant.Namespace, ref)
	}
	return r.ensureGeneratedSessionKey(ctx, tenant)
}

func readCookieEncryptionKey(
	ctx context.Context,
	c client.Client,
	namespace string,
	ref *corev1.SecretKeySelector,
) (string, error) {
	if ref.Name == "" || ref.Key == "" {
		return "", fmt.Errorf("cookie encryption key secret ref requires a name and key")
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, secret); err != nil {
		return "", fmt.Errorf("get cookie encryption key secret %q: %w", ref.Name, err)
	}
	value, ok := secret.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("secret %q does not contain key %q", ref.Name, ref.Key)
	}
	key := string(value)
	if err := validateCookieEncryptionKey(key); err != nil {
		return "", fmt.Errorf("secret %q key %q: %w", ref.Name, ref.Key, err)
	}
	return key, nil
}

func (r *DexTenantReconciler) ensureGeneratedSessionKey(ctx context.Context, tenant *idpv1alpha1.DexTenant) (string, error) {
	name := dexSessionSecretName(tenant)
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: tenant.Namespace, Name: name}, secret)
	switch {
	case err == nil:
		value, ok := secret.Data[dexSessionCookieKey]
		if !ok {
			return "", fmt.Errorf("secret %q does not contain key %q", name, dexSessionCookieKey)
		}
		key := string(value)
		if err := validateCookieEncryptionKey(key); err != nil {
			return "", fmt.Errorf("secret %q: %w", name, err)
		}
		return key, nil
	case apierrors.IsNotFound(err):
		return r.createSessionSecret(ctx, tenant, name)
	default:
		return "", err
	}
}

func (r *DexTenantReconciler) createSessionSecret(ctx context.Context, tenant *idpv1alpha1.DexTenant, name string) (string, error) {
	key, err := generateCookieEncryptionKey()
	if err != nil {
		return "", err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tenant.Namespace},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{dexSessionCookieKey: []byte(key)},
	}
	if err := controllerutil.SetControllerReference(tenant, secret, r.Scheme); err != nil {
		return "", err
	}
	if err := r.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return r.ensureGeneratedSessionKey(ctx, tenant)
		}
		return "", err
	}
	return key, nil
}

func generateCookieEncryptionKey() (string, error) {
	out := make([]byte, 32)
	alphabetLen := big.NewInt(int64(len(dexSessionKeyAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, alphabetLen)
		if err != nil {
			return "", fmt.Errorf("generate cookie encryption key: %w", err)
		}
		out[i] = dexSessionKeyAlphabet[n.Int64()]
	}
	return string(out), nil
}

func (r *DexTenantReconciler) ensureConfigMap(ctx context.Context, tenant *idpv1alpha1.DexTenant, cookieEncryptionKey string) error {
	config, err := dexTenantConfig(tenant, cookieEncryptionKey)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: dexTenantResourceName(tenant), Namespace: tenant.Namespace},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{"config.yaml": config}
		return controllerutil.SetControllerReference(tenant, cm, r.Scheme)
	})
	return err
}

func dexContainerEnv(tenant *idpv1alpha1.DexTenant) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "DEX_API_CONNECTORS_CRUD", Value: strconv.FormatBool(true)},
		{Name: "DEX_CLIENT_CREDENTIAL_GRANT_ENABLED_BY_DEFAULT", Value: strconv.FormatBool(true)},
	}
	if tenant.Spec.Sessions != nil {
		env = append(env, corev1.EnvVar{Name: dexSessionsEnabledEnv, Value: strconv.FormatBool(true)})
	}
	return env
}

func (r *DexTenantReconciler) ensureDeployment(ctx context.Context, tenant *idpv1alpha1.DexTenant, cookieEncryptionKey string) error {
	image := tenant.Spec.Image
	if image == "" {
		image = idpv1alpha1.DefaultDexImage
	}
	replicas := tenant.Spec.Replicas
	if replicas == nil {
		replicas = new(int32(1))
	}
	labels := dexTenantLabels(tenant)
	name := dexTenantResourceName(tenant)
	config, err := dexTenantConfig(tenant, cookieEncryptionKey)
	if err != nil {
		return err
	}
	configChecksum := fmt.Sprintf("%x", sha256.Sum256([]byte(config)))

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tenant.Namespace},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
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
						RunAsNonRoot: new(true),
						RunAsUser:    new(dexContainerUserID),
						RunAsGroup:   new(dexContainerUserID),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{
						{
							Name:    dexAppName,
							Image:   image,
							Command: []string{dexAppName, "serve", "/etc/dex/config.yaml"},
							Env:     dexContainerEnv(tenant),
							Ports: []corev1.ContainerPort{
								{Name: "http", ContainerPort: 5556},
								{Name: "grpc", ContainerPort: 5557},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: dexConfigVolumeName, MountPath: "/etc/dex", ReadOnly: true},
							},
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: new(false),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: dexConfigVolumeName,
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
