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
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	idpv1alpha1 "groeneslot.nl/dex-idp-operator/api/v1alpha1"
	"groeneslot.nl/dex-idp-operator/internal/dex"
)

const (
	// dexUnavailableRequeueInterval controls how quickly the controller retries syncing the
	// connector once the managed Dex Deployment exists but is not reachable yet, for example
	// while its pod is still starting.
	dexUnavailableRequeueInterval = 10 * time.Second

	messageConnectorSynced = "Dex tenant connector is synced"
)

// DexTenantReconciler reconciles a DexTenant object
type DexTenantReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	ClientFactory DexClientFactory
}

// +kubebuilder:rbac:groups=idp.groeneslot.nl,resources=dextenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=idp.groeneslot.nl,resources=dextenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=idp.groeneslot.nl,resources=dextenants/finalizers,verbs=update
// +kubebuilder:rbac:groups=idp.groeneslot.nl,resources=dexconnectors,verbs=get;list;watch
// +kubebuilder:rbac:groups=idp.groeneslot.nl,resources=dexconnections,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=dex.coreos.com,resources=authcodes;authrequests;oauth2clients;signingkeies;refreshtokens;passwords;offlinesessionses;connectors,verbs=get;list;watch;create;update;patch;delete

// Reconcile provisions a tenant-scoped, operator-managed Dex instance: it ensures Dex's
// Kubernetes storage CRDs and RBAC exist, renders the Dex Deployment/Service/ConfigMap,
// manages an owned DexConnection for reaching the instance, pre-seeds the tenant's connector
// into storage before Dex ever starts, and keeps that connector in sync afterward using Dex's
// gRPC connector API.
func (r *DexTenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	tenant := &idpv1alpha1.DexTenant{}
	if err := r.Get(ctx, req.NamespacedName, tenant); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	connectorSpec := &idpv1alpha1.DexConnector{}
	connectorKey := types.NamespacedName{Namespace: tenant.Namespace, Name: tenant.Spec.ConnectorRef.Name}
	if err := r.Get(ctx, connectorKey, connectorSpec); err != nil {
		return ctrl.Result{}, r.statusError(ctx, tenant, "ConnectorNotFound", err)
	}

	connector, err := resolveDexConnector(ctx, r.Client, tenant.Namespace, connectorSpec)
	if err != nil {
		return ctrl.Result{}, r.statusError(ctx, tenant, "ConnectorInvalid", err)
	}

	if err := ensureDexStorageCRDs(ctx, r.Client); err != nil {
		return ctrl.Result{}, r.statusError(ctx, tenant, "StorageCRDsFailed", err)
	}
	if err := r.ensureServiceAccount(ctx, tenant); err != nil {
		return ctrl.Result{}, r.statusError(ctx, tenant, "ServiceAccountFailed", err)
	}
	if err := r.ensureStorageRBAC(ctx, tenant); err != nil {
		return ctrl.Result{}, r.statusError(ctx, tenant, "RBACFailed", err)
	}
	if err := r.ensureConfigMap(ctx, tenant); err != nil {
		return ctrl.Result{}, r.statusError(ctx, tenant, "ConfigMapFailed", err)
	}
	if err := r.ensureDeployment(ctx, tenant); err != nil {
		return ctrl.Result{}, r.statusError(ctx, tenant, "DeploymentFailed", err)
	}
	if err := r.ensureService(ctx, tenant); err != nil {
		return ctrl.Result{}, r.statusError(ctx, tenant, "ServiceFailed", err)
	}

	connection, err := r.ensureDexConnection(ctx, tenant)
	if err != nil {
		return ctrl.Result{}, r.statusError(ctx, tenant, "DexConnectionFailed", err)
	}

	if err := r.preseedConnector(ctx, tenant, connector); err != nil {
		return ctrl.Result{}, r.statusError(ctx, tenant, "ConnectorPreseedFailed", err)
	}

	ready, reason, message, syncErr := r.syncConnector(ctx, connection, connector)

	if statusErr := r.setStatus(ctx, tenant, connection.Name, ready, reason, message); statusErr != nil {
		return ctrl.Result{}, statusErr
	}

	if !ready && syncErr == nil {
		log.Info("Waiting for managed Dex instance to become reachable", "tenant", tenant.Name)
		return ctrl.Result{RequeueAfter: dexUnavailableRequeueInterval}, nil
	}
	if syncErr != nil {
		return ctrl.Result{}, syncErr
	}

	log.Info("Synced Dex tenant", "tenant", tenant.Name, "connector", connector.GetId())
	return ctrl.Result{}, nil
}

// syncConnector keeps the tenant's live Dex connector in sync via gRPC. It is not an error for
// Dex to be temporarily unreachable, for example while its pod is still starting; the caller
// requeues in that case instead of failing.
func (r *DexTenantReconciler) syncConnector(
	ctx context.Context,
	connection *idpv1alpha1.DexConnection,
	connector *dexapi.Connector,
) (ready bool, reason, message string, err error) {
	cfg := dex.DialConfig{Address: connection.Spec.Address, Plaintext: true}

	factory := r.ClientFactory
	if factory == nil {
		factory = dex.Dial
	}
	dexAPI, dialErr := factory(ctx, cfg)
	if dialErr != nil {
		return false, reasonDexUnavailable, dialErr.Error(), nil
	}
	defer dexAPI.Close() //nolint:errcheck

	if _, versionErr := dexAPI.GetVersion(ctx); versionErr != nil {
		return false, reasonDexUnavailable, versionErr.Error(), nil
	}

	updateErr := dexAPI.UpdateConnector(ctx, &dexapi.UpdateConnectorReq{
		Id:        connector.GetId(),
		NewType:   connector.GetType(),
		NewName:   connector.GetName(),
		NewConfig: connector.GetConfig(),
	})
	if errors.Is(updateErr, dex.ErrNotFound) {
		updateErr = dexAPI.CreateConnector(ctx, connector)
		if errors.Is(updateErr, dex.ErrAlreadyExists) {
			updateErr = nil
		}
	}
	if updateErr != nil {
		return false, "ConnectorSyncFailed", updateErr.Error(), updateErr
	}
	return true, reasonSynced, messageConnectorSynced, nil
}

func (r *DexTenantReconciler) statusError(ctx context.Context, tenant *idpv1alpha1.DexTenant, reason string, err error) error {
	if statusErr := r.setStatus(ctx, tenant, "", false, reason, err.Error()); statusErr != nil {
		return statusErr
	}
	return err
}

func (r *DexTenantReconciler) setStatus(
	ctx context.Context,
	tenant *idpv1alpha1.DexTenant,
	connectionName string,
	ready bool,
	reason, message string,
) error {
	patch := client.MergeFrom(tenant.DeepCopy())
	tenant.Status.ObservedGeneration = tenant.Generation
	if connectionName != "" {
		tenant.Status.ConnectionRef = connectionName
		tenant.Status.ServiceName = dexTenantResourceName(tenant)
	}

	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&tenant.Status.Conditions, metav1.Condition{
		Type:               idpv1alpha1.ConditionReady,
		Status:             status,
		ObservedGeneration: tenant.Generation,
		Reason:             reason,
		Message:            message,
	})
	return r.Status().Patch(ctx, tenant, patch)
}

// SetupWithManager sets up the controller with the Manager.
func (r *DexTenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&idpv1alpha1.DexTenant{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.Service{}).
		Watches(&idpv1alpha1.DexConnector{}, handler.EnqueueRequestsFromMapFunc(r.dexTenantsForConnector)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.dexTenantsForSecret)).
		Named("dextenant").
		Complete(r)
}

func (r *DexTenantReconciler) dexTenantsForConnector(ctx context.Context, object client.Object) []reconcile.Request {
	tenants := &idpv1alpha1.DexTenantList{}
	if err := r.List(ctx, tenants, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(tenants.Items))
	for _, item := range tenants.Items {
		if item.Spec.ConnectorRef.Name == object.GetName() {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: item.Namespace, Name: item.Name},
			})
		}
	}
	return requests
}

func (r *DexTenantReconciler) dexTenantsForSecret(ctx context.Context, object client.Object) []reconcile.Request {
	connectors := &idpv1alpha1.DexConnectorList{}
	if err := r.List(ctx, connectors, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}
	referencing := sets.New[string]()
	for _, connector := range connectors.Items {
		for _, ref := range connector.Spec.SecretRefs {
			if ref.ValueFrom.SecretKeyRef.Name == object.GetName() {
				referencing.Insert(connector.Name)
				break
			}
		}
	}
	if referencing.Len() == 0 {
		return nil
	}

	tenants := &idpv1alpha1.DexTenantList{}
	if err := r.List(ctx, tenants, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(tenants.Items))
	for _, item := range tenants.Items {
		if referencing.Has(item.Spec.ConnectorRef.Name) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: item.Namespace, Name: item.Name},
			})
		}
	}
	return requests
}
