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

const dexClientFinalizer = "idp.groeneslot.nl/dex-client"

var errReconciliationBlocked = errors.New("dex client reconciliation blocked")

// DexClientFactory creates a Dex client from resolved connection settings.
type DexClientFactory func(context.Context, dex.DialConfig) (dex.Client, error)

// DexClientReconciler reconciles a DexClient object
type DexClientReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	ClientFactory DexClientFactory
}

// +kubebuilder:rbac:groups=idp.groeneslot.nl,resources=dexclients,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=idp.groeneslot.nl,resources=dexclients/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=idp.groeneslot.nl,resources=dexclients/finalizers,verbs=update
// +kubebuilder:rbac:groups=idp.groeneslot.nl,resources=dexconnections,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

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

	dexConnection := &idpv1alpha1.DexConnection{}
	connectionKey := types.NamespacedName{
		Namespace: dexClient.Namespace,
		Name:      dexClient.Spec.ConnectionRef.Name,
	}
	if err := r.Get(ctx, connectionKey, dexConnection); err != nil {
		statusErr := r.setFailureStatus(ctx, dexClient, "ConnectionNotFound", err.Error())
		if statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	cfg, err := r.dialConfig(ctx, dexClient.Namespace, dexConnection)
	if err != nil {
		return ctrl.Result{}, r.statusError(ctx, dexClient, "ConnectionInvalid", err)
	}

	desired, err := r.desiredDexClient(ctx, dexClient)
	if err != nil {
		return ctrl.Result{}, r.statusError(ctx, dexClient, "SecretInvalid", err)
	}

	dexAPI, err := r.newDexClient(ctx, cfg)
	if err != nil {
		return ctrl.Result{}, r.statusError(ctx, dexClient, "DexUnavailable", err)
	}
	defer func() {
		if closeErr := dexAPI.Close(); closeErr != nil {
			log.Error(closeErr, "Failed to close Dex client")
		}
	}()

	if err := r.applyDexClient(ctx, dexAPI, dexClient, desired); err != nil {
		if errors.Is(err, errReconciliationBlocked) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	log.Info("Synced Dex client", "clientID", dexClient.Spec.ClientID)
	return ctrl.Result{}, r.setSuccessStatus(ctx, dexClient)
}

func (r *DexClientReconciler) reconcileDelete(ctx context.Context, dexClient *idpv1alpha1.DexClient) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(dexClient, dexClientFinalizer) {
		return ctrl.Result{}, nil
	}

	dexConnection := &idpv1alpha1.DexConnection{}
	connectionKey := types.NamespacedName{
		Namespace: dexClient.Namespace,
		Name:      dexClient.Spec.ConnectionRef.Name,
	}
	if err := r.Get(ctx, connectionKey, dexConnection); err != nil {
		if apierrors.IsNotFound(err) {
			controllerutil.RemoveFinalizer(dexClient, dexClientFinalizer)
			return ctrl.Result{}, r.Update(ctx, dexClient)
		}
		return ctrl.Result{}, err
	}

	cfg, err := r.dialConfig(ctx, dexClient.Namespace, dexConnection)
	if err != nil {
		return ctrl.Result{}, err
	}

	dexAPI, err := r.newDexClient(ctx, cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer dexAPI.Close() //nolint:errcheck

	if err := dexAPI.DeleteClient(ctx, dexClient.Spec.ClientID); err != nil && !errors.Is(err, dex.ErrNotFound) {
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(dexClient, dexClientFinalizer)
	return ctrl.Result{}, r.Update(ctx, dexClient)
}

func (r *DexClientReconciler) applyDexClient(
	ctx context.Context,
	dexAPI dex.Client,
	dexClient *idpv1alpha1.DexClient,
	desired *dexapi.Client,
) error {
	existing, err := dexAPI.GetClient(ctx, desired.GetId())
	if errors.Is(err, dex.ErrNotFound) {
		_, createErr := dexAPI.CreateClient(ctx, desired)
		if errors.Is(createErr, dex.ErrAlreadyExists) {
			return r.updateExistingDexClient(ctx, dexAPI, dexClient, desired)
		}
		return createErr
	}
	if err != nil {
		return r.statusError(ctx, dexClient, "DexUnavailable", err)
	}

	if immutableChanged(existing, desired) {
		if !dexClient.Spec.RecreateOnImmutableChange {
			if err := r.setBlockedStatus(ctx, dexClient, "ImmutableChangeBlocked",
				"Requested Dex client secret or public setting changed; set recreateOnImmutableChange to recreate it"); err != nil {
				return err
			}
			return errReconciliationBlocked
		}
		if err := dexAPI.DeleteClient(ctx, desired.GetId()); err != nil && !errors.Is(err, dex.ErrNotFound) {
			return r.statusError(ctx, dexClient, "DexDeleteFailed", err)
		}
		_, err = dexAPI.CreateClient(ctx, desired)
		if err != nil {
			return r.statusError(ctx, dexClient, "DexCreateFailed", err)
		}
		return nil
	}

	if !mutableChanged(existing, desired) {
		return nil
	}

	return r.updateExistingDexClient(ctx, dexAPI, dexClient, desired)
}

func (r *DexClientReconciler) updateExistingDexClient(
	ctx context.Context,
	dexAPI dex.Client,
	dexClient *idpv1alpha1.DexClient,
	desired *dexapi.Client,
) error {
	err := dexAPI.UpdateClient(ctx, &dexapi.UpdateClientReq{
		Id:           desired.GetId(),
		RedirectUris: desired.GetRedirectUris(),
		TrustedPeers: desired.GetTrustedPeers(),
		Name:         desired.GetName(),
		LogoUrl:      desired.GetLogoUrl(),
	})
	if errors.Is(err, dex.ErrNotFound) {
		_, err = dexAPI.CreateClient(ctx, desired)
	}
	if err != nil {
		return r.statusError(ctx, dexClient, "DexUpdateFailed", err)
	}
	return nil
}

func (r *DexClientReconciler) newDexClient(ctx context.Context, cfg dex.DialConfig) (dex.Client, error) {
	factory := r.ClientFactory
	if factory == nil {
		factory = dex.Dial
	}
	return factory(ctx, cfg)
}

func (r *DexClientReconciler) desiredDexClient(ctx context.Context, dexClient *idpv1alpha1.DexClient) (*dexapi.Client, error) {
	var secret string
	if !dexClient.Spec.Public {
		value, err := r.resolveValue(ctx, dexClient.Namespace, dexClient.Spec.Secret)
		if err != nil {
			return nil, err
		}
		secret = string(value)
	}

	return &dexapi.Client{
		Id:           dexClient.Spec.ClientID,
		Secret:       secret,
		RedirectUris: dexClient.Spec.RedirectURIs,
		TrustedPeers: dexClient.Spec.TrustedPeers,
		Public:       dexClient.Spec.Public,
		Name:         dexClient.Spec.Name,
		LogoUrl:      dexClient.Spec.LogoURL,
	}, nil
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
		cfg.TLS.CACertPEM, err = r.resolveValue(ctx, namespace, tlsSpec.CA)
		if err != nil {
			return cfg, err
		}
	}
	if tlsSpec.ClientCertificate != nil {
		cfg.TLS.ClientCertPEM, err = r.resolveValue(ctx, namespace, &tlsSpec.ClientCertificate.Certificate)
		if err != nil {
			return cfg, err
		}
		cfg.TLS.ClientKeyPEM, err = r.resolveValue(ctx, namespace, &tlsSpec.ClientCertificate.PrivateKey)
		if err != nil {
			return cfg, err
		}
	}

	return cfg, nil
}

func (r *DexClientReconciler) resolveValue(
	ctx context.Context,
	namespace string,
	source *idpv1alpha1.ValueSource,
) ([]byte, error) {
	if source == nil {
		return nil, fmt.Errorf("value source is required")
	}
	if source.Value != nil {
		return []byte(*source.Value), nil
	}
	if source.ValueFrom == nil {
		return nil, fmt.Errorf("value or valueFrom is required")
	}

	ref := source.ValueFrom.SecretKeyRef
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: namespace, Name: ref.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) && ref.Optional != nil && *ref.Optional {
			return nil, nil
		}
		return nil, err
	}

	value, ok := secret.Data[ref.Key]
	if !ok {
		if ref.Optional != nil && *ref.Optional {
			return nil, nil
		}
		return nil, fmt.Errorf("secret %q does not contain key %q", ref.Name, ref.Key)
	}

	return value, nil
}

func (r *DexClientReconciler) statusError(
	ctx context.Context,
	dexClient *idpv1alpha1.DexClient,
	reason string,
	err error,
) error {
	if statusErr := r.setFailureStatus(ctx, dexClient, reason, err.Error()); statusErr != nil {
		return statusErr
	}
	return err
}

func (r *DexClientReconciler) setSuccessStatus(ctx context.Context, dexClient *idpv1alpha1.DexClient) error {
	patch := client.MergeFrom(dexClient.DeepCopy())
	dexClient.Status.ObservedGeneration = dexClient.Generation
	dexClient.Status.ClientID = dexClient.Spec.ClientID
	meta.SetStatusCondition(&dexClient.Status.Conditions, metav1.Condition{
		Type:               idpv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: dexClient.Generation,
		Reason:             "Synced",
		Message:            "Dex client is synced",
	})
	meta.SetStatusCondition(&dexClient.Status.Conditions, metav1.Condition{
		Type:               idpv1alpha1.ConditionSynced,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: dexClient.Generation,
		Reason:             "Synced",
		Message:            "Dex client is synced",
	})
	meta.SetStatusCondition(&dexClient.Status.Conditions, metav1.Condition{
		Type:               idpv1alpha1.ConditionBlocked,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: dexClient.Generation,
		Reason:             "NotBlocked",
		Message:            "Dex client reconciliation is not blocked",
	})
	return r.Status().Patch(ctx, dexClient, patch)
}

func (r *DexClientReconciler) setFailureStatus(
	ctx context.Context,
	dexClient *idpv1alpha1.DexClient,
	reason string,
	message string,
) error {
	patch := client.MergeFrom(dexClient.DeepCopy())
	dexClient.Status.ObservedGeneration = dexClient.Generation
	dexClient.Status.ClientID = dexClient.Spec.ClientID
	meta.SetStatusCondition(&dexClient.Status.Conditions, metav1.Condition{
		Type:               idpv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: dexClient.Generation,
		Reason:             reason,
		Message:            message,
	})
	meta.SetStatusCondition(&dexClient.Status.Conditions, metav1.Condition{
		Type:               idpv1alpha1.ConditionSynced,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: dexClient.Generation,
		Reason:             reason,
		Message:            message,
	})
	return r.Status().Patch(ctx, dexClient, patch)
}

func (r *DexClientReconciler) setBlockedStatus(
	ctx context.Context,
	dexClient *idpv1alpha1.DexClient,
	reason string,
	message string,
) error {
	if err := r.setFailureStatus(ctx, dexClient, reason, message); err != nil {
		return err
	}

	latest := &idpv1alpha1.DexClient{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(dexClient), latest); err != nil {
		return err
	}
	patch := client.MergeFrom(latest.DeepCopy())
	meta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
		Type:               idpv1alpha1.ConditionBlocked,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: latest.Generation,
		Reason:             reason,
		Message:            message,
	})
	return r.Status().Patch(ctx, latest, patch)
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
		if item.Spec.ConnectionRef.Name == object.GetName() {
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

func valueSourceReferencesSecret(source *idpv1alpha1.ValueSource, secretName string) bool {
	return source != nil &&
		source.ValueFrom != nil &&
		source.ValueFrom.SecretKeyRef.Name == secretName
}
