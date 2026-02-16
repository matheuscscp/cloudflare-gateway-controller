// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

func (r *GatewayClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	gatewayClassCRDChangedPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return e.Object.GetName() == apiv1.CRDGatewayClass
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectNew.GetName() == apiv1.CRDGatewayClass &&
				e.ObjectOld.GetAnnotations()[apiv1.AnnotationBundleVersion] !=
					e.ObjectNew.GetAnnotations()[apiv1.AnnotationBundleVersion]
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return e.Object.GetName() == apiv1.CRDGatewayClass
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return e.Object.GetName() == apiv1.CRDGatewayClass
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.GatewayClass{},
			builder.WithPredicates(debugPredicate(apiv1.KindGatewayClass, predicate.Or(
				predicate.GenerationChangedPredicate{},
				predicate.AnnotationChangedPredicate{})))).
		WatchesMetadata(&apiextensionsv1.CustomResourceDefinition{},
			handler.EnqueueRequestsFromMapFunc(r.managedGatewayClasses),
			builder.WithPredicates(debugPredicate(apiv1.KindCustomResourceDefinition,
				gatewayClassCRDChangedPredicate))).
		WatchesMetadata(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.managedGatewayClasses),
			builder.WithPredicates(debugPredicate(apiv1.KindSecret,
				predicate.ResourceVersionChangedPredicate{}))).
		Complete(r)
}

func (r *GatewayClassReconciler) managedGatewayClasses(ctx context.Context, obj client.Object) []reconcile.Request {
	var classes gatewayv1.GatewayClassList
	if err := r.List(ctx, &classes); err != nil {
		return nil
	}

	var secret *corev1.Secret
	if s, ok := obj.(*corev1.Secret); ok {
		secret = s
	}

	var requests []reconcile.Request
	for i := range classes.Items {
		gc := &classes.Items[i]

		if gc.Spec.ControllerName != apiv1.ControllerName {
			continue
		}

		if secret == nil || (gc.Spec.ParametersRef != nil &&
			gc.Spec.ParametersRef.Group == "" &&
			string(gc.Spec.ParametersRef.Kind) == apiv1.KindSecret &&
			gc.Spec.ParametersRef.Namespace != nil &&
			string(*gc.Spec.ParametersRef.Namespace) == secret.Namespace &&
			string(gc.Spec.ParametersRef.Name) == secret.Name) {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKey{Name: gc.Name},
			})
		}
	}
	return requests
}
