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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	idpv1alpha1 "groeneslot.nl/dex-idp-operator/api/v1alpha1"
)

// DexConnectionReconciler reconciles a DexConnection object
type DexConnectionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=idp.y.digital,resources=dexconnections,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=idp.y.digital,resources=dexconnections/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=idp.y.digital,resources=dexconnections/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the DexConnection object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/reconcile
func (r *DexConnectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	connection := &idpv1alpha1.DexConnection{}
	if err := r.Get(ctx, req.NamespacedName, connection); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	patch := client.MergeFrom(connection.DeepCopy())
	connection.Status.ObservedGeneration = connection.Generation
	meta.SetStatusCondition(&connection.Status.Conditions, metav1.Condition{
		Type:               idpv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: connection.Generation,
		Reason:             "Configured",
		Message:            "Dex connection configuration is valid",
	})

	return ctrl.Result{}, r.Status().Patch(ctx, connection, patch)
}

// SetupWithManager sets up the controller with the Manager.
func (r *DexConnectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&idpv1alpha1.DexConnection{}).
		Named("dexconnection").
		Complete(r)
}
