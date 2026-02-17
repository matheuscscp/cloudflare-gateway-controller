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
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

const (
	indexSpecControllerName = ".spec.controllerName"
	indexSpecParametersRef  = ".spec.parametersRef"
)

func (r *GatewayClassReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	// Index GatewayClasses by .spec.controllerName.
	mgr.GetCache().IndexField(ctx, &gatewayv1.GatewayClass{}, indexSpecControllerName,
		func(obj client.Object) []string {
			gc := obj.(*gatewayv1.GatewayClass)
			return []string{string(gc.Spec.ControllerName)}
		})

	// Index GatewayClasses by Secret ref.
	mgr.GetCache().IndexField(ctx, &gatewayv1.GatewayClass{}, indexSpecParametersRef,
		func(obj client.Object) []string {
			gc := obj.(*gatewayv1.GatewayClass)
			if gc.Spec.ParametersRef == nil ||
				gc.Spec.ParametersRef.Group != "" ||
				string(gc.Spec.ParametersRef.Kind) != apiv1.KindSecret ||
				gc.Spec.ParametersRef.Namespace == nil {
				return nil
			}
			return []string{string(*gc.Spec.ParametersRef.Namespace) + "/" + string(gc.Spec.ParametersRef.Name)}
		})

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
			handler.EnqueueRequestsFromMapFunc(r.managedGatewayClasses(false)),
			builder.WithPredicates(debugPredicate(apiv1.KindCustomResourceDefinition,
				gatewayClassCRDChangedPredicate))).
		WatchesMetadata(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.managedGatewayClasses(true)),
			builder.WithPredicates(debugPredicate(apiv1.KindSecret,
				predicate.ResourceVersionChangedPredicate{}))).
		Complete(r)
}

func (r *GatewayClassReconciler) managedGatewayClasses(withSecret bool) func(ctx context.Context, obj client.Object) []reconcile.Request {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		matchingFields := client.MatchingFields{
			indexSpecControllerName: string(apiv1.ControllerName),
		}
		if withSecret {
			matchingFields[indexSpecParametersRef] = obj.GetNamespace() + "/" + obj.GetName()
		}

		var classes gatewayv1.GatewayClassList
		if err := r.List(ctx, &classes, matchingFields); err != nil {
			log.FromContext(ctx).Error(err, "failed to list managed GatewayClasses")
			return nil
		}

		var requests []reconcile.Request
		for _, gc := range classes.Items {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKey{Name: gc.Name},
			})
		}
		return requests
	}
}
