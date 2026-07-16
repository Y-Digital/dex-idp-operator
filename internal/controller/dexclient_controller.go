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
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"

	dexapi "github.com/dexidp/dex/api/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	idpv1alpha1 "groeneslot.nl/dex-idp-operator/api/v1alpha1"
	"groeneslot.nl/dex-idp-operator/internal/dex"
)

const (
	dexClientFinalizer = "idp.groeneslot.nl/dex-client"
	// generatedSecretKey is the Secret data key used for operator-generated client secrets.
	generatedSecretKey = "client-secret"

	reasonSynced         = "Synced"
	messageClientSynced  = "Dex client is synced"
	reasonDexUnavailable = "DexUnavailable"
)

// DexClientFactory creates a Dex client from resolved connection settings.
type DexClientFactory func(context.Context, dex.DialConfig) (dex.Client, error)

// DexClientReconciler reconciles a DexClient object
type DexClientReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	ClientFactory DexClientFactory
}

// dexTarget is one resolved Dex instance a DexClient must be synced against, either the
// single connectionRef target or one entry from tenantRefs.
type dexTarget struct {
	// Key identifies the target, formatted as "connection:<name>" or "tenant:<name>".
	Key string
	// DialConfig is nil when ResolveErr is set.
	DialConfig *dex.DialConfig
	Secret     string
	// SecretRefName is the name of the generated Secret backing Secret, when applicable.
	SecretRefName string
	// ResolveErr is set when the target's DexConnection/DexTenant could not be resolved.
	ResolveErr error
}

// +kubebuilder:rbac:groups=idp.groeneslot.nl,resources=dexclients,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=idp.groeneslot.nl,resources=dexclients/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=idp.groeneslot.nl,resources=dexclients/finalizers,verbs=update
// +kubebuilder:rbac:groups=idp.groeneslot.nl,resources=dexconnections,verbs=get;list;watch
// +kubebuilder:rbac:groups=idp.groeneslot.nl,resources=dextenants,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *DexClientReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	dexClient := &idpv1alpha1.DexClient{}
	if err := r.Get(ctx, req.NamespacedName, dexClient); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !dexClient.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, dexClient)
	}

	if controllerutil.AddFinalizer(dexClient, dexClientFinalizer) {
		if err := r.Update(ctx, dexClient); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	targets := r.resolveTargets(ctx, dexClient, true)

	mounts := make([]idpv1alpha1.DexClientMountStatus, 0, len(targets))
	var errs []error
	allReady := true
	anyBlocked := false

	for _, target := range targets {
		mountStatus := idpv1alpha1.DexClientMountStatus{Target: target.Key, SecretRef: target.SecretRefName}

		if target.ResolveErr != nil {
			mountStatus.Reason = "ResolveFailed"
			mountStatus.Message = target.ResolveErr.Error()
			mounts = append(mounts, mountStatus)
			allReady = false
			errs = append(errs, target.ResolveErr)
			continue
		}

		ready, blocked, reason, message, applyErr := r.applyToTarget(ctx, dexClient, target)
		mountStatus.Ready = ready
		mountStatus.Reason = reason
		mountStatus.Message = message
		mounts = append(mounts, mountStatus)
		if blocked {
			anyBlocked = true
		}
		if !ready {
			allReady = false
		}
		if applyErr != nil {
			errs = append(errs, applyErr)
		}
	}

	if err := r.setMountStatus(ctx, dexClient, mounts, allReady, anyBlocked); err != nil {
		return ctrl.Result{}, err
	}

	if len(errs) > 0 {
		return ctrl.Result{}, errors.Join(errs...)
	}

	log.Info("Synced Dex client", "clientID", dexClient.Spec.ClientID, "mounts", len(mounts))
	return ctrl.Result{}, nil
}

func (r *DexClientReconciler) reconcileDelete(ctx context.Context, dexClient *idpv1alpha1.DexClient) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(dexClient, dexClientFinalizer) {
		return ctrl.Result{}, nil
	}

	targets := r.resolveTargets(ctx, dexClient, false)

	for _, target := range targets {
		if target.ResolveErr != nil || target.DialConfig == nil {
			// The target's Dex is unreachable or already gone; nothing to clean up there.
			continue
		}
		if err := r.deleteFromTarget(ctx, *target.DialConfig, dexClient.Spec.ClientID); err != nil {
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(dexClient, dexClientFinalizer)
	return ctrl.Result{}, r.Update(ctx, dexClient)
}

func (r *DexClientReconciler) deleteFromTarget(ctx context.Context, cfg dex.DialConfig, clientID string) error {
	dexAPI, err := r.newDexClient(ctx, cfg)
	if err != nil {
		return err
	}
	defer dexAPI.Close() //nolint:errcheck

	if err := dexAPI.DeleteClient(ctx, clientID); err != nil && !errors.Is(err, dex.ErrNotFound) {
		return err
	}
	return nil
}

// resolveTargets resolves every mount target declared by dexClient. Individual target
// resolution failures are recorded on the returned dexTarget rather than aborting the whole
// call, so one broken tenant does not prevent syncing the client onto the others.
func (r *DexClientReconciler) resolveTargets(
	ctx context.Context,
	dexClient *idpv1alpha1.DexClient,
	includeSecrets bool,
) []dexTarget {
	if dexClient.Spec.ConnectionRef != nil {
		return []dexTarget{r.resolveConnectionTarget(ctx, dexClient, dexClient.Spec.ConnectionRef.Name, includeSecrets)}
	}

	targets := make([]dexTarget, 0, len(dexClient.Spec.TenantRefs))
	for _, ref := range dexClient.Spec.TenantRefs {
		targets = append(targets, r.resolveTenantTarget(ctx, dexClient, ref, includeSecrets))
	}
	return targets
}

func (r *DexClientReconciler) resolveConnectionTarget(
	ctx context.Context,
	dexClient *idpv1alpha1.DexClient,
	connectionName string,
	includeSecrets bool,
) dexTarget {
	key := "connection:" + connectionName

	dexConnection := &idpv1alpha1.DexConnection{}
	connectionKey := types.NamespacedName{Namespace: dexClient.Namespace, Name: connectionName}
	if err := r.Get(ctx, connectionKey, dexConnection); err != nil {
		return dexTarget{Key: key, ResolveErr: fmt.Errorf("get DexConnection %q: %w", connectionName, err)}
	}

	cfg, err := r.dialConfig(ctx, dexClient.Namespace, dexConnection)
	if err != nil {
		return dexTarget{Key: key, ResolveErr: fmt.Errorf("build dial config for DexConnection %q: %w", connectionName, err)}
	}

	if !includeSecrets {
		return dexTarget{Key: key, DialConfig: &cfg}
	}

	secret, err := r.resolveClientSecret(ctx, dexClient)
	if err != nil {
		return dexTarget{Key: key, ResolveErr: err}
	}
	return dexTarget{Key: key, DialConfig: &cfg, Secret: secret}
}

// resolveTenantTarget resolves one tenantRefs entry. tenantNamespace defaults to the
// DexClient's own namespace when the reference does not set an explicit namespace, since
// each DexTenant is expected to run in its own dedicated namespace (see DexTenantReference).
func (r *DexClientReconciler) resolveTenantTarget(
	ctx context.Context,
	dexClient *idpv1alpha1.DexClient,
	ref idpv1alpha1.DexTenantReference,
	includeSecrets bool,
) dexTarget {
	tenantNamespace := ref.Namespace
	if tenantNamespace == "" {
		tenantNamespace = dexClient.Namespace
	}
	key := "tenant:" + tenantNamespace + "/" + ref.Name

	tenant := &idpv1alpha1.DexTenant{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: tenantNamespace, Name: ref.Name}, tenant); err != nil {
		return dexTarget{Key: key, ResolveErr: fmt.Errorf("get DexTenant %q: %w", key, err)}
	}
	if tenant.Status.ConnectionRef == "" {
		return dexTarget{Key: key, ResolveErr: fmt.Errorf("DexTenant %q does not have a ready Dex connection yet", key)}
	}

	dexConnection := &idpv1alpha1.DexConnection{}
	connectionKey := types.NamespacedName{Namespace: tenantNamespace, Name: tenant.Status.ConnectionRef}
	if err := r.Get(ctx, connectionKey, dexConnection); err != nil {
		return dexTarget{Key: key, ResolveErr: fmt.Errorf(
			"get DexConnection %q for DexTenant %q: %w", tenant.Status.ConnectionRef, key, err)}
	}

	cfg, err := r.dialConfig(ctx, tenantNamespace, dexConnection)
	if err != nil {
		return dexTarget{Key: key, ResolveErr: fmt.Errorf("build dial config for DexTenant %q: %w", key, err)}
	}

	if !includeSecrets {
		return dexTarget{Key: key, DialConfig: &cfg}
	}

	if dexClient.Spec.Public {
		return dexTarget{Key: key, DialConfig: &cfg}
	}

	secretRefName := generatedSecretName(dexClient.Name, ref.Name, tenantNamespace, dexClient.Namespace)
	secretValue, err := r.ensureGeneratedSecret(ctx, dexClient, secretRefName)
	if err != nil {
		return dexTarget{Key: key, ResolveErr: fmt.Errorf("ensure client secret for DexTenant %q: %w", key, err)}
	}
	return dexTarget{Key: key, DialConfig: &cfg, Secret: secretValue, SecretRefName: secretRefName}
}

// generatedSecretName builds the name of the operator-generated per-tenant client Secret,
// which always lives in the DexClient's own namespace. The tenant namespace is only included
// when it differs from the DexClient's namespace, keeping the common same-namespace case
// readable while still avoiding collisions across cross-namespace mounts of the same tenant
// name.
func generatedSecretName(dexClientName, tenantName, tenantNamespace, dexClientNamespace string) string {
	if tenantNamespace == dexClientNamespace {
		return fmt.Sprintf("%s-%s-secret", dexClientName, tenantName)
	}
	return fmt.Sprintf("%s-%s-%s-secret", dexClientName, tenantNamespace, tenantName)
}

func (r *DexClientReconciler) resolveClientSecret(ctx context.Context, dexClient *idpv1alpha1.DexClient) (string, error) {
	if dexClient.Spec.Public {
		return "", nil
	}
	value, err := resolveValueSource(ctx, r.Client, dexClient.Namespace, dexClient.Spec.Secret)
	if err != nil {
		return "", err
	}
	return string(value), nil
}

// ensureGeneratedSecret returns the client secret value stored in the named Secret, creating
// it with a freshly generated random value owned by dexClient if it does not already exist.
// The generated value is never modified once created, so it stays stable across reconciles.
func (r *DexClientReconciler) ensureGeneratedSecret(
	ctx context.Context,
	dexClient *idpv1alpha1.DexClient,
	name string,
) (string, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: dexClient.Namespace, Name: name}
	err := r.Get(ctx, key, secret)
	switch {
	case err == nil:
		value, ok := secret.Data[generatedSecretKey]
		if !ok {
			return "", fmt.Errorf("secret %q does not contain key %q", name, generatedSecretKey)
		}
		return string(value), nil
	case apierrors.IsNotFound(err):
		return r.createGeneratedSecret(ctx, dexClient, name)
	default:
		return "", err
	}
}

func (r *DexClientReconciler) createGeneratedSecret(
	ctx context.Context,
	dexClient *idpv1alpha1.DexClient,
	name string,
) (string, error) {
	value, err := generateClientSecret()
	if err != nil {
		return "", err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: dexClient.Namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{generatedSecretKey: []byte(value)},
	}
	if err := controllerutil.SetControllerReference(dexClient, secret, r.Scheme); err != nil {
		return "", err
	}
	if err := r.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return r.ensureGeneratedSecret(ctx, dexClient, name)
		}
		return "", err
	}
	return value, nil
}

func generateClientSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate client secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// applyToTarget syncs dexClient onto one resolved target and reports its resulting mount
// status without writing to the DexClient's status subresource itself.
func (r *DexClientReconciler) applyToTarget(
	ctx context.Context,
	dexClient *idpv1alpha1.DexClient,
	target dexTarget,
) (ready, blocked bool, reason, message string, err error) {
	desired := &dexapi.Client{
		Id:           dexClient.Spec.ClientID,
		Secret:       target.Secret,
		RedirectUris: dexClient.Spec.RedirectURIs,
		TrustedPeers: dexClient.Spec.TrustedPeers,
		Public:       dexClient.Spec.Public,
		Name:         dexClient.Spec.Name,
		LogoUrl:      dexClient.Spec.LogoURL,
	}

	dexAPI, dialErr := r.newDexClient(ctx, *target.DialConfig)
	if dialErr != nil {
		return false, false, reasonDexUnavailable, dialErr.Error(), dialErr
	}
	defer func() {
		if closeErr := dexAPI.Close(); closeErr != nil {
			logf.FromContext(ctx).Error(closeErr, "Failed to close Dex client")
		}
	}()

	existing, getErr := dexAPI.GetClient(ctx, desired.GetId())
	if errors.Is(getErr, dex.ErrNotFound) {
		if _, createErr := dexAPI.CreateClient(ctx, desired); createErr != nil {
			if errors.Is(createErr, dex.ErrAlreadyExists) {
				ready, reason, message, err := r.updateDexClient(ctx, dexAPI, desired)
				return ready, false, reason, message, err
			}
			return false, false, "DexCreateFailed", createErr.Error(), createErr
		}
		return true, false, reasonSynced, messageClientSynced, nil
	}
	if getErr != nil {
		return false, false, reasonDexUnavailable, getErr.Error(), getErr
	}

	if immutableChanged(existing, desired) {
		if !dexClient.Spec.RecreateOnImmutableChange {
			return false, true, "ImmutableChangeBlocked",
				"Requested Dex client secret or public setting changed; set recreateOnImmutableChange to recreate it", nil
		}
		if delErr := dexAPI.DeleteClient(ctx, desired.GetId()); delErr != nil && !errors.Is(delErr, dex.ErrNotFound) {
			return false, false, "DexDeleteFailed", delErr.Error(), delErr
		}
		if _, createErr := dexAPI.CreateClient(ctx, desired); createErr != nil {
			return false, false, "DexCreateFailed", createErr.Error(), createErr
		}
		return true, false, reasonSynced, messageClientSynced, nil
	}

	if !mutableChanged(existing, desired) {
		return true, false, reasonSynced, messageClientSynced, nil
	}

	ready, reason, message, err = r.updateDexClient(ctx, dexAPI, desired)
	return ready, false, reason, message, err
}

func (r *DexClientReconciler) updateDexClient(
	ctx context.Context,
	dexAPI dex.Client,
	desired *dexapi.Client,
) (ready bool, reason, message string, err error) {
	updateErr := dexAPI.UpdateClient(ctx, &dexapi.UpdateClientReq{
		Id:           desired.GetId(),
		RedirectUris: desired.GetRedirectUris(),
		TrustedPeers: desired.GetTrustedPeers(),
		Name:         desired.GetName(),
		LogoUrl:      desired.GetLogoUrl(),
	})
	if errors.Is(updateErr, dex.ErrNotFound) {
		_, updateErr = dexAPI.CreateClient(ctx, desired)
	}
	if updateErr != nil {
		return false, "DexUpdateFailed", updateErr.Error(), updateErr
	}
	return true, reasonSynced, messageClientSynced, nil
}

func (r *DexClientReconciler) newDexClient(ctx context.Context, cfg dex.DialConfig) (dex.Client, error) {
	factory := r.ClientFactory
	if factory == nil {
		factory = dex.Dial
	}
	return factory(ctx, cfg)
}

func (r *DexClientReconciler) dialConfig(
	ctx context.Context,
	namespace string,
	connection *idpv1alpha1.DexConnection,
) (dex.DialConfig, error) {
	cfg := dex.DialConfig{
		Address:   connection.Spec.Address,
		Plaintext: connection.Spec.Plaintext,
	}
	if connection.Spec.TLS == nil {
		return cfg, nil
	}

	tlsSpec := connection.Spec.TLS
	cfg.TLS.InsecureSkipVerify = tlsSpec.InsecureSkipVerify
	cfg.TLS.ServerName = tlsSpec.ServerName

	var err error
	if tlsSpec.CA != nil {
		cfg.TLS.CACertPEM, err = resolveValueSource(ctx, r.Client, namespace, tlsSpec.CA)
		if err != nil {
			return cfg, err
		}
	}
	if tlsSpec.ClientCertificate != nil {
		cfg.TLS.ClientCertPEM, err = resolveValueSource(ctx, r.Client, namespace, &tlsSpec.ClientCertificate.Certificate)
		if err != nil {
			return cfg, err
		}
		cfg.TLS.ClientKeyPEM, err = resolveValueSource(ctx, r.Client, namespace, &tlsSpec.ClientCertificate.PrivateKey)
		if err != nil {
			return cfg, err
		}
	}

	return cfg, nil
}

// setMountStatus records the per-target mount results and the aggregated Ready, Synced, and
// Blocked conditions in a single status update.
func (r *DexClientReconciler) setMountStatus(
	ctx context.Context,
	dexClient *idpv1alpha1.DexClient,
	mounts []idpv1alpha1.DexClientMountStatus,
	allReady bool,
	anyBlocked bool,
) error {
	patch := client.MergeFrom(dexClient.DeepCopy())
	dexClient.Status.ObservedGeneration = dexClient.Generation
	dexClient.Status.ClientID = dexClient.Spec.ClientID
	dexClient.Status.Mounts = mounts

	readyStatus, reason, message := metav1.ConditionFalse, "SyncFailed", "One or more Dex client mounts failed to sync"
	if allReady {
		readyStatus, reason, message = metav1.ConditionTrue, reasonSynced, messageClientSynced
	}
	meta.SetStatusCondition(&dexClient.Status.Conditions, metav1.Condition{
		Type:               idpv1alpha1.ConditionReady,
		Status:             readyStatus,
		ObservedGeneration: dexClient.Generation,
		Reason:             reason,
		Message:            message,
	})
	meta.SetStatusCondition(&dexClient.Status.Conditions, metav1.Condition{
		Type:               idpv1alpha1.ConditionSynced,
		Status:             readyStatus,
		ObservedGeneration: dexClient.Generation,
		Reason:             reason,
		Message:            message,
	})

	blockedStatus, blockedReason, blockedMessage := metav1.ConditionFalse, "NotBlocked", "Dex client reconciliation is not blocked"
	if anyBlocked {
		blockedStatus, blockedReason, blockedMessage = metav1.ConditionTrue, "ImmutableChangeBlocked",
			"One or more mounts have an unsafe immutable change; set recreateOnImmutableChange to recreate them"
	}
	meta.SetStatusCondition(&dexClient.Status.Conditions, metav1.Condition{
		Type:               idpv1alpha1.ConditionBlocked,
		Status:             blockedStatus,
		ObservedGeneration: dexClient.Generation,
		Reason:             blockedReason,
		Message:            blockedMessage,
	})

	return r.Status().Patch(ctx, dexClient, patch)
}

func immutableChanged(existing, desired *dexapi.Client) bool {
	return existing.GetSecret() != desired.GetSecret() || existing.GetPublic() != desired.GetPublic()
}

func mutableChanged(existing, desired *dexapi.Client) bool {
	return !reflect.DeepEqual(existing.GetRedirectUris(), desired.GetRedirectUris()) ||
		!reflect.DeepEqual(existing.GetTrustedPeers(), desired.GetTrustedPeers()) ||
		existing.GetName() != desired.GetName() ||
		existing.GetLogoUrl() != desired.GetLogoUrl()
}

// SetupWithManager sets up the controller with the Manager.
func (r *DexClientReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&idpv1alpha1.DexClient{}).
		Watches(&idpv1alpha1.DexConnection{}, handler.EnqueueRequestsFromMapFunc(r.dexClientsForConnection)).
		Watches(&idpv1alpha1.DexTenant{}, handler.EnqueueRequestsFromMapFunc(r.dexClientsForTenant)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.dexClientsForSecret)).
		Named("dexclient").
		Complete(r)
}

func (r *DexClientReconciler) dexClientsForConnection(ctx context.Context, object client.Object) []reconcile.Request {
	dexClients := &idpv1alpha1.DexClientList{}
	if err := r.List(ctx, dexClients, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(dexClients.Items))
	for _, item := range dexClients.Items {
		if item.Spec.ConnectionRef != nil && item.Spec.ConnectionRef.Name == object.GetName() {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: item.Namespace,
					Name:      item.Name,
				},
			})
		}
	}
	return requests
}

// dexClientsForTenant maps a changed DexTenant to the DexClients that mount onto it. Since
// tenantRefs can cross namespaces, this must search DexClients cluster-wide rather than only
// within the changed DexTenant's namespace.
func (r *DexClientReconciler) dexClientsForTenant(ctx context.Context, object client.Object) []reconcile.Request {
	dexClients := &idpv1alpha1.DexClientList{}
	if err := r.List(ctx, dexClients); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(dexClients.Items))
	for _, item := range dexClients.Items {
		for _, ref := range item.Spec.TenantRefs {
			tenantNamespace := ref.Namespace
			if tenantNamespace == "" {
				tenantNamespace = item.Namespace
			}
			if ref.Name == object.GetName() && tenantNamespace == object.GetNamespace() {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: item.Namespace,
						Name:      item.Name,
					},
				})
				break
			}
		}
	}
	return requests
}

func (r *DexClientReconciler) dexClientsForSecret(ctx context.Context, object client.Object) []reconcile.Request {
	dexClients := &idpv1alpha1.DexClientList{}
	if err := r.List(ctx, dexClients, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}

	requested := sets.New[types.NamespacedName]()
	for _, item := range dexClients.Items {
		if dexClientReferencesSecret(&item, object.GetName()) {
			requested.Insert(types.NamespacedName{Namespace: item.Namespace, Name: item.Name})
			continue
		}

		if item.Spec.ConnectionRef == nil {
			continue
		}
		connection := &idpv1alpha1.DexConnection{}
		key := types.NamespacedName{Namespace: item.Namespace, Name: item.Spec.ConnectionRef.Name}
		if err := r.Get(ctx, key, connection); err != nil {
			continue
		}
		if dexConnectionReferencesSecret(connection, object.GetName()) {
			requested.Insert(types.NamespacedName{Namespace: item.Namespace, Name: item.Name})
		}
	}

	requests := make([]reconcile.Request, 0, requested.Len())
	for key := range requested {
		requests = append(requests, reconcile.Request{NamespacedName: key})
	}
	return requests
}

func dexClientReferencesSecret(dexClient *idpv1alpha1.DexClient, secretName string) bool {
	return valueSourceReferencesSecret(dexClient.Spec.Secret, secretName)
}

func dexConnectionReferencesSecret(connection *idpv1alpha1.DexConnection, secretName string) bool {
	if connection.Spec.TLS == nil {
		return false
	}
	if valueSourceReferencesSecret(connection.Spec.TLS.CA, secretName) {
		return true
	}
	clientCert := connection.Spec.TLS.ClientCertificate
	return clientCert != nil &&
		(valueSourceReferencesSecret(&clientCert.Certificate, secretName) ||
			valueSourceReferencesSecret(&clientCert.PrivateKey, secretName))
}
