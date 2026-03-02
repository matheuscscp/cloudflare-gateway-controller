// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/sidecar"
)

// routeConfigMapKey is the key used to store the route config in the ConfigMap.
const routeConfigMapKey = "config.yaml"

// routeConfigMapLabels returns the standard labels for the route ConfigMap.
func routeConfigMapLabels(gw *gatewayv1.Gateway) map[string]string {
	lbls := apiv1.GatewayResourceLabels(gw.Name, apiv1.LabelAppComponentRoutes)
	maps.Copy(lbls, infrastructureLabels(gw.Spec.Infrastructure))
	return lbls
}

// reconcileRouteConfigMap builds the route Config from valid routes and
// creates/updates the ConfigMap. Returns denied refs, change messages, and error.
func (r *GatewayReconciler) reconcileRouteConfigMap(ctx context.Context, gw *gatewayv1.Gateway, routes []*gatewayv1.HTTPRoute) (map[types.NamespacedName][]string, []string, error) {
	l := log.FromContext(ctx)

	cfg, routesWithDeniedRefs, err := buildRouteConfig(ctx, r.Client, routes)
	if err != nil {
		return nil, nil, err
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling route config: %w", err)
	}

	cmName := apiv1.GatewayResourceName(gw)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: gw.Namespace,
		},
	}
	if err := controllerutil.SetControllerReference(gw, cm, r.Scheme()); err != nil {
		return nil, nil, fmt.Errorf("setting owner reference on route configmap: %w", err)
	}

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = routeConfigMapLabels(gw)
		cm.Data = map[string]string{
			routeConfigMapKey: string(data),
		}
		return controllerutil.SetControllerReference(gw, cm, r.Scheme())
	})
	if err != nil {
		return nil, nil, fmt.Errorf("creating/updating route ConfigMap %s: %w", cmName, err)
	}

	var changes []string
	if result != controllerutil.OperationResultNone {
		changes = append(changes, fmt.Sprintf("route ConfigMap %s %s", cmName, result))
	}
	l.V(1).Info("Reconciled route ConfigMap", "configmap", cmName, "result", result)
	return routesWithDeniedRefs, changes, nil
}

// buildRouteConfig converts valid HTTPRoutes into route config entries.
// Each rule's backendRefs are emitted as weighted backends on the route.
// Returns the route config, a map of routes with denied backend refs, and
// any transient error from ReferenceGrant checks.
func buildRouteConfig(ctx context.Context, r client.Reader, routes []*gatewayv1.HTTPRoute) (sidecar.Config, map[types.NamespacedName][]string, error) {
	var cfgRoutes []sidecar.Route
	routesWithDeniedRefs := make(map[types.NamespacedName][]string)
	for _, route := range routes {
		owner := route.Namespace + "/" + route.Name
		for _, rule := range route.Spec.Rules {
			if len(rule.BackendRefs) == 0 {
				continue
			}
			var backends []sidecar.Backend
			denied := false
			for _, ref := range rule.BackendRefs {
				ns := route.Namespace
				if ref.Namespace != nil {
					ns = string(*ref.Namespace)
				}
				granted, err := backendReferenceGranted(ctx, r, route.Namespace, ns, string(ref.Name))
				if err != nil {
					return sidecar.Config{}, nil, fmt.Errorf("checking ReferenceGrant for backendRef %s/%s in HTTPRoute %s/%s: %w", ns, ref.Name, route.Namespace, route.Name, err)
				}
				if !granted {
					key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
					routesWithDeniedRefs[key] = append(routesWithDeniedRefs[key], ns+"/"+string(ref.Name))
					denied = true
					continue
				}
				port := int32(80)
				if ref.Port != nil {
					port = *ref.Port
				}
				weight := int32(1)
				if ref.Weight != nil {
					weight = *ref.Weight
				}
				service := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", string(ref.Name), ns, port)
				backends = append(backends, sidecar.Backend{Service: service, Weight: weight})
			}
			if denied || len(backends) == 0 {
				continue
			}
			var sp *sidecar.SessionPersistence
			if rule.SessionPersistence != nil {
				spType := "Cookie"
				if rule.SessionPersistence.Type != nil {
					spType = string(*rule.SessionPersistence.Type)
				}
				sessionName := "cgw-session"
				if spType == "Header" {
					sessionName = "X-Cgw-Session"
				}
				if rule.SessionPersistence.SessionName != nil {
					sessionName = *rule.SessionPersistence.SessionName
				}
				sp = &sidecar.SessionPersistence{
					Type:        spType,
					SessionName: sessionName,
				}
				if rule.SessionPersistence.AbsoluteTimeout != nil {
					sp.AbsoluteTimeout = string(*rule.SessionPersistence.AbsoluteTimeout)
				}
				if rule.SessionPersistence.IdleTimeout != nil {
					sp.IdleTimeout = string(*rule.SessionPersistence.IdleTimeout)
				}
				if rule.SessionPersistence.CookieConfig != nil && rule.SessionPersistence.CookieConfig.LifetimeType != nil {
					sp.CookieLifetimeType = string(*rule.SessionPersistence.CookieConfig.LifetimeType)
				} else {
					sp.CookieLifetimeType = "Session"
				}
			}
			pathPrefix := pathFromMatches(rule.Matches)
			for _, hostname := range route.Spec.Hostnames {
				cfgRoutes = append(cfgRoutes, sidecar.Route{
					Hostname:           string(hostname),
					PathPrefix:         pathPrefix,
					Owner:              owner,
					Backends:           backends,
					SessionPersistence: sp,
				})
			}
		}
	}
	return sidecar.Config{Routes: cfgRoutes}, routesWithDeniedRefs, nil
}

// getExistingRouteConfig reads the current route ConfigMap for a Gateway and
// returns the parsed config. Returns nil if the ConfigMap does not exist.
func (r *GatewayReconciler) getExistingRouteConfig(ctx context.Context, gw *gatewayv1.Gateway) (*sidecar.Config, error) {
	cmName := apiv1.GatewayResourceName(gw)
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: gw.Namespace}, &cm); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("getting route ConfigMap %s: %w", cmName, err)
	}
	data, ok := cm.Data[routeConfigMapKey]
	if !ok {
		return nil, nil
	}
	var cfg sidecar.Config
	if err := yaml.Unmarshal([]byte(data), &cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling route config from ConfigMap %s: %w", cmName, err)
	}
	return &cfg, nil
}

// removeOwnerReferencesFromRouteConfigMap removes the Gateway's owner references
// from route ConfigMaps so they survive garbage collection when the Gateway is
// deleted with reconciliation disabled.
func (r *GatewayReconciler) removeOwnerReferencesFromRouteConfigMap(ctx context.Context, gw *gatewayv1.Gateway) ([]client.Object, error) {
	l := log.FromContext(ctx)
	var removed []client.Object
	matchLabels := client.MatchingLabels(apiv1.GatewayResourceLabels(gw.Name, apiv1.LabelAppComponentRoutes))

	var cmList corev1.ConfigMapList
	if err := r.List(ctx, &cmList, client.InNamespace(gw.Namespace), matchLabels); err != nil {
		return removed, fmt.Errorf("listing route ConfigMaps: %w", err)
	}
	for i := range cmList.Items {
		cm := &cmList.Items[i]
		cmKey := client.ObjectKeyFromObject(cm)
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, cmKey, cm); err != nil {
				return client.IgnoreNotFound(err)
			}
			cmPatch := client.MergeFromWithOptions(cm.DeepCopy(), client.MergeFromWithOptimisticLock{})
			if !removeOwnerRef(cm, gw.UID) {
				return nil
			}
			if err := r.Patch(ctx, cm, cmPatch); err != nil {
				return fmt.Errorf("patching ConfigMap %s: %w", cm.Name, err)
			}
			cm.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(apiv1.KindConfigMap))
			removed = append(removed, cm)
			return nil
		}); err != nil {
			return removed, err
		}
		l.V(1).Info("Removed owner reference from route ConfigMap", "configmap", cmKey)
	}

	return removed, nil
}
