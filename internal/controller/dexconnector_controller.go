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
	"bytes"
	"context"
	"encoding/json"
	"fmt"

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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	idpv1alpha1 "groeneslot.nl/dex-idp-operator/api/v1alpha1"
)

// DexConnectorReconciler reconciles a DexConnector object
type DexConnectorReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=idp.groeneslot.nl,resources=dexconnectors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=idp.groeneslot.nl,resources=dexconnectors/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=idp.groeneslot.nl,resources=dexconnectors/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile validates that a DexConnector's config resolves to valid Dex connector
// configuration and reports the result on status. DexTenant is responsible for applying the
// resolved connector to its managed Dex instance.
func (r *DexConnectorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	connector := &idpv1alpha1.DexConnector{}
	if err := r.Get(ctx, req.NamespacedName, connector); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	patch := client.MergeFrom(connector.DeepCopy())
	connector.Status.ObservedGeneration = connector.Generation

	if _, err := resolveDexConnector(ctx, r.Client, connector.Namespace, connector); err != nil {
		meta.SetStatusCondition(&connector.Status.Conditions, metav1.Condition{
			Type:               idpv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: connector.Generation,
			Reason:             "ConfigInvalid",
			Message:            err.Error(),
		})
		return ctrl.Result{}, r.Status().Patch(ctx, connector, patch)
	}

	meta.SetStatusCondition(&connector.Status.Conditions, metav1.Condition{
		Type:               idpv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: connector.Generation,
		Reason:             "Resolved",
		Message:            "Dex connector configuration is valid",
	})
	return ctrl.Result{}, r.Status().Patch(ctx, connector, patch)
}

// resolveDexConnector resolves a DexConnector's secretRefs, interpolates them into its raw
// config, and returns the result as a ready-to-use Dex API connector.
func resolveDexConnector(
	ctx context.Context,
	c client.Client,
	namespace string,
	connector *idpv1alpha1.DexConnector,
) (*dexapi.Connector, error) {
	config, err := interpolateConnectorConfig(ctx, c, namespace, connector)
	if err != nil {
		return nil, err
	}

	var probe map[string]any
	if err := json.Unmarshal(config, &probe); err != nil {
		return nil, fmt.Errorf("resolved connector config is not a valid JSON object: %w", err)
	}

	name := connector.Spec.DisplayName
	if name == "" {
		name = connector.Spec.ConnectorID
	}

	return &dexapi.Connector{
		Id:     connector.Spec.ConnectorID,
		Type:   connector.Spec.Type,
		Name:   name,
		Config: config,
	}, nil
}

func interpolateConnectorConfig(
	ctx context.Context,
	c client.Client,
	namespace string,
	connector *idpv1alpha1.DexConnector,
) ([]byte, error) {
	result := connector.Spec.Config.Raw
	for _, ref := range connector.Spec.SecretRefs {
		value, err := resolveValueSource(ctx, c, namespace, &idpv1alpha1.ValueSource{ValueFrom: &ref.ValueFrom})
		if err != nil {
			return nil, fmt.Errorf("resolve secretRef %q: %w", ref.Name, err)
		}
		placeholder := []byte("${" + ref.Name + "}")
		result = bytes.ReplaceAll(result, placeholder, jsonEscapeStringContent(string(value)))
	}
	return result, nil
}

// jsonEscapeStringContent returns s escaped for embedding inside an existing JSON string
// literal, without adding the surrounding quotes.
func jsonEscapeStringContent(s string) []byte {
	encoded, _ := json.Marshal(s) // json.Marshal on a string never fails.
	return encoded[1 : len(encoded)-1]
}

// SetupWithManager sets up the controller with the Manager.
func (r *DexConnectorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&idpv1alpha1.DexConnector{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.dexConnectorsForSecret)).
		Named("dexconnector").
		Complete(r)
}

func (r *DexConnectorReconciler) dexConnectorsForSecret(ctx context.Context, object client.Object) []reconcile.Request {
	connectors := &idpv1alpha1.DexConnectorList{}
	if err := r.List(ctx, connectors, client.InNamespace(object.GetNamespace())); err != nil {
		return nil
	}

	requested := sets.New[types.NamespacedName]()
	for _, item := range connectors.Items {
		for _, ref := range item.Spec.SecretRefs {
			if ref.ValueFrom.SecretKeyRef.Name == object.GetName() {
				requested.Insert(types.NamespacedName{Namespace: item.Namespace, Name: item.Name})
				break
			}
		}
	}

	requests := make([]reconcile.Request, 0, requested.Len())
	for key := range requested {
		requests = append(requests, reconcile.Request{NamespacedName: key})
	}
	return requests
}
