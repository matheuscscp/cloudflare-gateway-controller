// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"maps"

	"github.com/fluxcd/pkg/ssa"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// sidecarConfigMapKey is the key used to store the sidecar config in the ConfigMap.
const sidecarConfigMapKey = "config.yaml"

// reconcileSidecarConfigMap builds the sidecar Config from valid routes and
// creates/updates the ConfigMap. Returns denied refs, change messages, and error.
func (r *GatewayReconciler) reconcileSidecarConfigMap(ctx context.Context, gw *gatewayv1.Gateway, routes []*gatewayv1.HTTPRoute) (map[types.NamespacedName][]string, []string, error) {
	l := log.FromContext(ctx)

	cfg, routesWithDeniedRefs, err := buildSidecarConfig(ctx, r.Client, routes)
	if err != nil {
		return nil, nil, err
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling sidecar config: %w", err)
	}

	cmName := apiv1.GatewayResourceName(gw)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: gw.Namespace,
		},
	}
	if err := controllerutil.SetControllerReference(gw, cm, r.Scheme()); err != nil {
		return nil, nil, fmt.Errorf("setting owner reference on sidecar configmap: %w", err)
	}

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = sidecarLabels(gw)
		cm.Data = map[string]string{
			sidecarConfigMapKey: string(data),
		}
		return controllerutil.SetControllerReference(gw, cm, r.Scheme())
	})
	if err != nil {
		return nil, nil, fmt.Errorf("creating/updating sidecar ConfigMap %s: %w", cmName, err)
	}

	var changes []string
	if result != controllerutil.OperationResultNone {
		changes = append(changes, fmt.Sprintf("sidecar ConfigMap %s %s", cmName, result))
	}
	l.V(1).Info("Reconciled sidecar ConfigMap", "configmap", cmName, "result", result)
	return routesWithDeniedRefs, changes, nil
}

// buildSidecarConfig converts valid HTTPRoutes into sidecar.Config routes.
// This parallels buildIngressRules but produces sidecar.Route structs.
// Each rule's backendRefs are emitted as weighted backends on the sidecar route.
// Returns the sidecar config, a map of routes with denied backend refs, and
// any transient error from ReferenceGrant checks.
func buildSidecarConfig(ctx context.Context, r client.Reader, routes []*gatewayv1.HTTPRoute) (sidecar.Config, map[types.NamespacedName][]string, error) {
	var sidecarRoutes []sidecar.Route
	routesWithDeniedRefs := make(map[types.NamespacedName][]string)
	for _, route := range routes {
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
			pathPrefix := pathFromMatches(rule.Matches)
			for _, hostname := range route.Spec.Hostnames {
				sidecarRoutes = append(sidecarRoutes, sidecar.Route{
					Hostname:   string(hostname),
					PathPrefix: pathPrefix,
					Backends:   backends,
				})
			}
		}
	}
	return sidecar.Config{Routes: sidecarRoutes}, routesWithDeniedRefs, nil
}

// reconcileSidecarRBAC creates/updates the ServiceAccount, Role, and RoleBinding
// for the sidecar's least-privilege ConfigMap access. Returns change messages.
func (r *GatewayReconciler) reconcileSidecarRBAC(ctx context.Context, gw *gatewayv1.Gateway) ([]string, error) {
	l := log.FromContext(ctx)
	var changes []string

	saName := apiv1.GatewayResourceName(gw)
	cmName := apiv1.GatewayResourceName(gw)
	labels := sidecarLabels(gw)

	ownerRef := metav1.OwnerReference{
		APIVersion:         gatewayv1.GroupVersion.String(),
		Kind:               apiv1.KindGateway,
		Name:               gw.Name,
		UID:                gw.UID,
		BlockOwnerDeletion: new(true),
		Controller:         new(true),
	}

	// ServiceAccount
	sa := &unstructured.Unstructured{}
	sa.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(apiv1.KindServiceAccount))
	sa.SetName(saName)
	sa.SetNamespace(gw.Namespace)
	sa.SetLabels(labels)
	sa.SetOwnerReferences([]metav1.OwnerReference{ownerRef})

	ssaEntry, err := r.ResourceManager.Apply(ctx, sa, ssaApplyOptions)
	if err != nil {
		return nil, fmt.Errorf("applying sidecar ServiceAccount %s: %w", saName, err)
	}
	if ssaEntry.Action != ssa.UnchangedAction {
		changes = append(changes, fmt.Sprintf("sidecar ServiceAccount %s %s", saName, ssaEntry.Action))
	}
	l.V(1).Info("Reconciled sidecar ServiceAccount", "serviceaccount", saName, "action", ssaEntry.Action)

	// Role
	role := &unstructured.Unstructured{}
	role.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind(apiv1.KindRole))
	role.SetName(saName)
	role.SetNamespace(gw.Namespace)
	role.SetLabels(labels)
	role.SetOwnerReferences([]metav1.OwnerReference{ownerRef})
	if err := unstructured.SetNestedSlice(role.Object, []any{
		map[string]any{
			"apiGroups":     []any{""},
			"resources":     []any{"configmaps"},
			"resourceNames": []any{cmName},
			"verbs":         []any{"get", "list", "watch"},
		},
	}, "rules"); err != nil {
		return nil, fmt.Errorf("setting role rules: %w", err)
	}

	ssaEntry, err = r.ResourceManager.Apply(ctx, role, ssaApplyOptions)
	if err != nil {
		return nil, fmt.Errorf("applying sidecar Role %s: %w", saName, err)
	}
	if ssaEntry.Action != ssa.UnchangedAction {
		changes = append(changes, fmt.Sprintf("sidecar Role %s %s", saName, ssaEntry.Action))
	}
	l.V(1).Info("Reconciled sidecar Role", "role", saName, "action", ssaEntry.Action)

	// RoleBinding
	rb := &unstructured.Unstructured{}
	rb.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind(apiv1.KindRoleBinding))
	rb.SetName(saName)
	rb.SetNamespace(gw.Namespace)
	rb.SetLabels(labels)
	rb.SetOwnerReferences([]metav1.OwnerReference{ownerRef})
	if err := unstructured.SetNestedField(rb.Object, map[string]any{
		"apiGroup": "rbac.authorization.k8s.io",
		"kind":     "Role",
		"name":     saName,
	}, "roleRef"); err != nil {
		return nil, fmt.Errorf("setting rolebinding roleRef: %w", err)
	}
	if err := unstructured.SetNestedSlice(rb.Object, []any{
		map[string]any{
			"kind":      "ServiceAccount",
			"name":      saName,
			"namespace": gw.Namespace,
		},
	}, "subjects"); err != nil {
		return nil, fmt.Errorf("setting rolebinding subjects: %w", err)
	}

	ssaEntry, err = r.ResourceManager.Apply(ctx, rb, ssaApplyOptions)
	if err != nil {
		return nil, fmt.Errorf("applying sidecar RoleBinding %s: %w", saName, err)
	}
	if ssaEntry.Action != ssa.UnchangedAction {
		changes = append(changes, fmt.Sprintf("sidecar RoleBinding %s %s", saName, ssaEntry.Action))
	}
	l.V(1).Info("Reconciled sidecar RoleBinding", "rolebinding", saName, "action", ssaEntry.Action)

	return changes, nil
}

// cleanupStaleSidecarResources deletes sidecar ConfigMaps, ServiceAccounts,
// Roles, and RoleBindings that are no longer desired.
func (r *GatewayReconciler) cleanupStaleSidecarResources(ctx context.Context, gw *gatewayv1.Gateway) ([]string, []string) {
	l := log.FromContext(ctx)
	matchLabels := client.MatchingLabels{
		"app.kubernetes.io/name":       "cloudflared",
		"app.kubernetes.io/managed-by": apiv1.ShortControllerName,
		"app.kubernetes.io/instance":   gw.Name,
		"app.kubernetes.io/component":  "sidecar",
	}

	var changes []string
	var errs []string

	// Delete ConfigMaps
	var cmList corev1.ConfigMapList
	if err := r.List(ctx, &cmList, client.InNamespace(gw.Namespace), matchLabels); err != nil {
		errs = append(errs, fmt.Sprintf("failed to list sidecar ConfigMaps for cleanup: %v", err))
	} else {
		for i := range cmList.Items {
			cm := &cmList.Items[i]
			if err := r.Delete(ctx, cm); client.IgnoreNotFound(err) != nil {
				errs = append(errs, fmt.Sprintf("failed to delete sidecar ConfigMap %s: %v", cm.Name, err))
				continue
			}
			changes = append(changes, fmt.Sprintf("deleted sidecar ConfigMap %s", cm.Name))
			l.V(1).Info("Deleted sidecar ConfigMap", "configmap", cm.Name)
		}
	}

	// Delete ServiceAccounts
	var saList corev1.ServiceAccountList
	if err := r.List(ctx, &saList, client.InNamespace(gw.Namespace), matchLabels); err != nil {
		errs = append(errs, fmt.Sprintf("failed to list sidecar ServiceAccounts for cleanup: %v", err))
	} else {
		for i := range saList.Items {
			sa := &saList.Items[i]
			if err := r.Delete(ctx, sa); client.IgnoreNotFound(err) != nil {
				errs = append(errs, fmt.Sprintf("failed to delete sidecar ServiceAccount %s: %v", sa.Name, err))
				continue
			}
			changes = append(changes, fmt.Sprintf("deleted sidecar ServiceAccount %s", sa.Name))
			l.V(1).Info("Deleted sidecar ServiceAccount", "serviceaccount", sa.Name)
		}
	}

	// Delete Roles
	var roleList rbacv1.RoleList
	if err := r.List(ctx, &roleList, client.InNamespace(gw.Namespace), matchLabels); err != nil {
		errs = append(errs, fmt.Sprintf("failed to list sidecar Roles for cleanup: %v", err))
	} else {
		for i := range roleList.Items {
			role := &roleList.Items[i]
			if err := r.Delete(ctx, role); client.IgnoreNotFound(err) != nil {
				errs = append(errs, fmt.Sprintf("failed to delete sidecar Role %s: %v", role.Name, err))
				continue
			}
			changes = append(changes, fmt.Sprintf("deleted sidecar Role %s", role.Name))
			l.V(1).Info("Deleted sidecar Role", "role", role.Name)
		}
	}

	// Delete RoleBindings
	var rbList rbacv1.RoleBindingList
	if err := r.List(ctx, &rbList, client.InNamespace(gw.Namespace), matchLabels); err != nil {
		errs = append(errs, fmt.Sprintf("failed to list sidecar RoleBindings for cleanup: %v", err))
	} else {
		for i := range rbList.Items {
			rb := &rbList.Items[i]
			if err := r.Delete(ctx, rb); client.IgnoreNotFound(err) != nil {
				errs = append(errs, fmt.Sprintf("failed to delete sidecar RoleBinding %s: %v", rb.Name, err))
				continue
			}
			changes = append(changes, fmt.Sprintf("deleted sidecar RoleBinding %s", rb.Name))
			l.V(1).Info("Deleted sidecar RoleBinding", "rolebinding", rb.Name)
		}
	}

	return changes, errs
}

// sidecarLabels returns the standard labels for sidecar resources.
func sidecarLabels(gw *gatewayv1.Gateway) map[string]string {
	lbls := maps.Clone(infrastructureLabels(gw.Spec.Infrastructure))
	if lbls == nil {
		lbls = make(map[string]string)
	}
	lbls["app.kubernetes.io/name"] = "cloudflared"
	lbls["app.kubernetes.io/managed-by"] = apiv1.ShortControllerName
	lbls["app.kubernetes.io/instance"] = gw.Name
	lbls["app.kubernetes.io/component"] = "sidecar"
	return lbls
}

// sidecarEnabled returns true when the sidecar should run for the given Gateway.
// The sidecar is enabled by default. It is disabled when the sidecar image is
// not configured, or when the CloudflareGatewayParameters explicitly sets
// tunnel.sidecar.enabled to false.
func (r *GatewayReconciler) sidecarEnabled(params *apiv1.CloudflareGatewayParameters) bool {
	if r.SidecarImage == "" {
		return false
	}
	if params == nil || params.Spec.Tunnel == nil ||
		params.Spec.Tunnel.Sidecar == nil || params.Spec.Tunnel.Sidecar.Enabled == nil {
		return true
	}
	return *params.Spec.Tunnel.Sidecar.Enabled
}

// buildSidecarIngressCatchAll returns a single catch-all ingress rule that
// forwards all traffic to the sidecar proxy on localhost:8080.
func buildSidecarIngressCatchAll() string {
	return "http://localhost:8080"
}

// removeOwnerReferencesFromSidecarResources removes the Gateway's owner references
// from sidecar ConfigMaps, ServiceAccounts, Roles, and RoleBindings so they
// survive garbage collection when the Gateway is deleted with reconciliation disabled.
func (r *GatewayReconciler) removeOwnerReferencesFromSidecarResources(ctx context.Context, gw *gatewayv1.Gateway) ([]client.Object, error) {
	l := log.FromContext(ctx)
	var removed []client.Object
	matchLabels := client.MatchingLabels{
		"app.kubernetes.io/name":       "cloudflared",
		"app.kubernetes.io/managed-by": apiv1.ShortControllerName,
		"app.kubernetes.io/instance":   gw.Name,
		"app.kubernetes.io/component":  "sidecar",
	}

	// ConfigMaps
	var cmList corev1.ConfigMapList
	if err := r.List(ctx, &cmList, client.InNamespace(gw.Namespace), matchLabels); err != nil {
		return removed, fmt.Errorf("listing sidecar ConfigMaps: %w", err)
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
		l.V(1).Info("Removed owner reference from sidecar ConfigMap", "configmap", cmKey)
	}

	// ServiceAccounts
	var saList corev1.ServiceAccountList
	if err := r.List(ctx, &saList, client.InNamespace(gw.Namespace), matchLabels); err != nil {
		return removed, fmt.Errorf("listing sidecar ServiceAccounts: %w", err)
	}
	for i := range saList.Items {
		sa := &saList.Items[i]
		saKey := client.ObjectKeyFromObject(sa)
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, saKey, sa); err != nil {
				return client.IgnoreNotFound(err)
			}
			saPatch := client.MergeFromWithOptions(sa.DeepCopy(), client.MergeFromWithOptimisticLock{})
			if !removeOwnerRef(sa, gw.UID) {
				return nil
			}
			if err := r.Patch(ctx, sa, saPatch); err != nil {
				return fmt.Errorf("patching ServiceAccount %s: %w", sa.Name, err)
			}
			sa.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(apiv1.KindServiceAccount))
			removed = append(removed, sa)
			return nil
		}); err != nil {
			return removed, err
		}
		l.V(1).Info("Removed owner reference from sidecar ServiceAccount", "serviceaccount", saKey)
	}

	// Roles
	var roleList rbacv1.RoleList
	if err := r.List(ctx, &roleList, client.InNamespace(gw.Namespace), matchLabels); err != nil {
		return removed, fmt.Errorf("listing sidecar Roles: %w", err)
	}
	for i := range roleList.Items {
		role := &roleList.Items[i]
		roleKey := client.ObjectKeyFromObject(role)
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, roleKey, role); err != nil {
				return client.IgnoreNotFound(err)
			}
			rolePatch := client.MergeFromWithOptions(role.DeepCopy(), client.MergeFromWithOptimisticLock{})
			if !removeOwnerRef(role, gw.UID) {
				return nil
			}
			if err := r.Patch(ctx, role, rolePatch); err != nil {
				return fmt.Errorf("patching Role %s: %w", role.Name, err)
			}
			role.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind(apiv1.KindRole))
			removed = append(removed, role)
			return nil
		}); err != nil {
			return removed, err
		}
		l.V(1).Info("Removed owner reference from sidecar Role", "role", roleKey)
	}

	// RoleBindings
	var rbList rbacv1.RoleBindingList
	if err := r.List(ctx, &rbList, client.InNamespace(gw.Namespace), matchLabels); err != nil {
		return removed, fmt.Errorf("listing sidecar RoleBindings: %w", err)
	}
	for i := range rbList.Items {
		rb := &rbList.Items[i]
		rbKey := client.ObjectKeyFromObject(rb)
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, rbKey, rb); err != nil {
				return client.IgnoreNotFound(err)
			}
			rbPatch := client.MergeFromWithOptions(rb.DeepCopy(), client.MergeFromWithOptimisticLock{})
			if !removeOwnerRef(rb, gw.UID) {
				return nil
			}
			if err := r.Patch(ctx, rb, rbPatch); err != nil {
				return fmt.Errorf("patching RoleBinding %s: %w", rb.Name, err)
			}
			rb.SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind(apiv1.KindRoleBinding))
			removed = append(removed, rb)
			return nil
		}); err != nil {
			return removed, err
		}
		l.V(1).Info("Removed owner reference from sidecar RoleBinding", "rolebinding", rbKey)
	}

	return removed, nil
}
