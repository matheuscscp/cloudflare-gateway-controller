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
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
)

// reconcileSidecarRBAC creates/updates the ServiceAccount, Role, and RoleBinding
// granting the sidecar read access to the route ConfigMap. Returns change messages.
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
	saTyped := &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       apiv1.KindServiceAccount,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            saName,
			Namespace:       gw.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
	}
	sa, err := toUnstructured(saTyped)
	if err != nil {
		return nil, fmt.Errorf("converting sidecar ServiceAccount to unstructured: %w", err)
	}

	ssaEntry, err := r.ResourceManager.Apply(ctx, sa, ssaApplyOptions)
	if err != nil {
		return nil, fmt.Errorf("applying sidecar ServiceAccount %s: %w", saName, err)
	}
	if ssaEntry.Action != ssa.UnchangedAction {
		changes = append(changes, fmt.Sprintf("sidecar ServiceAccount %s %s", saName, ssaEntry.Action))
	}
	l.V(1).Info("Reconciled sidecar ServiceAccount", "serviceaccount", saName, "action", ssaEntry.Action)

	// Role
	roleTyped := &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{
			APIVersion: rbacv1.SchemeGroupVersion.String(),
			Kind:       apiv1.KindRole,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            saName,
			Namespace:       gw.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Rules: []rbacv1.PolicyRule{{
			APIGroups:     []string{""},
			Resources:     []string{"configmaps"},
			ResourceNames: []string{cmName},
			Verbs:         []string{"get", "list", "watch"},
		}},
	}
	role, err := toUnstructured(roleTyped)
	if err != nil {
		return nil, fmt.Errorf("converting sidecar Role to unstructured: %w", err)
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
	rbTyped := &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: rbacv1.SchemeGroupVersion.String(),
			Kind:       apiv1.KindRoleBinding,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            saName,
			Namespace:       gw.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.SchemeGroupVersion.Group,
			Kind:     apiv1.KindRole,
			Name:     saName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      apiv1.KindServiceAccount,
			Name:      saName,
			Namespace: gw.Namespace,
		}},
	}
	rb, err := toUnstructured(rbTyped)
	if err != nil {
		return nil, fmt.Errorf("converting sidecar RoleBinding to unstructured: %w", err)
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

// cleanupStaleSidecarRBACResources deletes sidecar ServiceAccounts,
// Roles, and RoleBindings that are no longer desired.
func (r *GatewayReconciler) cleanupStaleSidecarRBACResources(ctx context.Context, gw *gatewayv1.Gateway) ([]string, []string) {
	l := log.FromContext(ctx)
	matchLabels := client.MatchingLabels(apiv1.GatewayResourceLabels(gw.Name, apiv1.LabelAppComponentSidecar))

	var changes []string
	var errs []string

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
	lbls := apiv1.GatewayResourceLabels(gw.Name, apiv1.LabelAppComponentSidecar)
	maps.Copy(lbls, infrastructureLabels(gw.Spec.Infrastructure))
	return lbls
}

// sidecarEnabled returns true when the sidecar should run for the given Gateway.
// The default depends on whether the controller has a sidecar image configured:
// when SidecarImage is set the sidecar is enabled by default; when it is empty
// the sidecar is disabled by default. The case where the sidecar is explicitly
// enabled but no sidecar image is configured is rejected by validateParameters.
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

// removeOwnerReferencesFromSidecarRBACResources removes the Gateway's owner references
// from sidecar ServiceAccounts, Roles, and RoleBindings so they survive garbage
// collection when the Gateway is deleted with reconciliation disabled.
func (r *GatewayReconciler) removeOwnerReferencesFromSidecarRBACResources(ctx context.Context, gw *gatewayv1.Gateway) ([]client.Object, error) {
	l := log.FromContext(ctx)
	var removed []client.Object
	matchLabels := client.MatchingLabels(apiv1.GatewayResourceLabels(gw.Name, apiv1.LabelAppComponentSidecar))

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
