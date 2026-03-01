// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/conditions"
)

// gatewayValidationError holds a terminal error and the condition to set on the Gateway.
type gatewayValidationError struct {
	err  error
	cond metav1.Condition
}

// routeConditions holds the pre-computed condition values for an HTTPRoute's
// status.parents entry (ResolvedRefs, DNS, Ready).
type routeConditions struct {
	resolvedRefsStatus                  metav1.ConditionStatus
	resolvedRefsReason, resolvedRefsMsg string
	dnsEnabled                          bool
	dnsStatus                           metav1.ConditionStatus
	dnsReason, dnsMessage               string
	readyStatus                         metav1.ConditionStatus
	readyReason, readyMsg               string
}

// validateGateway checks that the Gateway spec only uses features supported by
// Cloudflare tunnels. Returns nil if valid.
func validateGateway(gw *gatewayv1.Gateway) *gatewayValidationError {
	rejectedCond := func(reason gatewayv1.GatewayConditionReason, msg string) *gatewayValidationError {
		return &gatewayValidationError{
			err: reconcile.TerminalError(fmt.Errorf("%s", strings.ToLower(msg[:1])+msg[1:])),
			cond: metav1.Condition{
				Type:    string(gatewayv1.GatewayConditionAccepted),
				Status:  metav1.ConditionFalse,
				Reason:  string(reason),
				Message: msg,
			},
		}
	}

	if len(gw.Spec.Listeners) != 1 {
		msg := fmt.Sprintf("Gateway must have exactly one listener, got %d", len(gw.Spec.Listeners))
		return rejectedCond(gatewayv1.GatewayReasonListenersNotValid, msg)
	}

	if len(gw.Spec.Addresses) > 0 {
		return rejectedCond(gatewayv1.GatewayReasonUnsupportedAddress, "spec.addresses is not supported")
	}

	l := gw.Spec.Listeners[0]

	if l.Protocol != gatewayv1.HTTPSProtocolType {
		msg := fmt.Sprintf("Listener protocol %q is not supported, must be HTTPS", l.Protocol)
		return rejectedCond(gatewayv1.GatewayReasonListenersNotValid, msg)
	}

	if l.Port != 443 {
		return rejectedCond(gatewayv1.GatewayReasonListenersNotValid,
			fmt.Sprintf("Listener port %d is not supported, must be 443", l.Port))
	}

	if l.TLS != nil {
		return rejectedCond(gatewayv1.GatewayReasonListenersNotValid,
			"spec.listeners[0].tls is not supported; Cloudflare handles TLS termination")
	}

	if l.Hostname != nil {
		return rejectedCond(gatewayv1.GatewayReasonListenersNotValid,
			"spec.listeners[0].hostname is not supported; use HTTPRoute hostnames instead")
	}

	if l.AllowedRoutes != nil && len(l.AllowedRoutes.Kinds) > 0 {
		for _, k := range l.AllowedRoutes.Kinds {
			group := gatewayv1.Group(gatewayv1.GroupName)
			if k.Group != nil {
				group = *k.Group
			}
			if group != gatewayv1.Group(gatewayv1.GroupName) || string(k.Kind) != apiv1.KindHTTPRoute {
				msg := fmt.Sprintf("Only HTTPRoute kind is supported in spec.listeners[0].allowedRoutes.kinds, got %s/%s", group, k.Kind)
				return rejectedCond(gatewayv1.GatewayReasonListenersNotValid, msg)
			}
		}
	}

	if val, ok := gw.Annotations[apiv1.AnnotationReconcileEvery]; ok {
		if _, err := time.ParseDuration(val); err != nil {
			msg := fmt.Sprintf("Annotation %s has invalid duration %q", apiv1.AnnotationReconcileEvery, val)
			return rejectedCond(gatewayv1.GatewayReasonInvalidParameters, msg)
		}
	}

	return nil
}

// validateParameters checks that the CloudflareGatewayParameters spec is valid.
func validateParameters(params *apiv1.CloudflareGatewayParameters) *gatewayValidationError {
	if params != nil && params.Spec.DNS != nil && len(params.Spec.DNS.Zones) > 0 {
		// Reject duplicate zone names.
		seen := make(map[string]struct{}, len(params.Spec.DNS.Zones))
		for _, z := range params.Spec.DNS.Zones {
			if _, ok := seen[z.Name]; ok {
				msg := fmt.Sprintf("duplicate zone name %q in dns.zones", z.Name)
				return &gatewayValidationError{
					err: reconcile.TerminalError(fmt.Errorf("%s", msg)),
					cond: metav1.Condition{
						Type:    string(gatewayv1.GatewayConditionAccepted),
						Status:  metav1.ConditionFalse,
						Reason:  string(gatewayv1.GatewayReasonInvalidParameters),
						Message: msg,
					},
				}
			}
			seen[z.Name] = struct{}{}
		}
	}

	// Reject duplicate replica names (defense-in-depth, mirrors CEL XValidation).
	if params != nil && params.Spec.Tunnel != nil && len(params.Spec.Tunnel.Replicas) > 0 {
		seen := make(map[string]struct{}, len(params.Spec.Tunnel.Replicas))
		for _, r := range params.Spec.Tunnel.Replicas {
			if _, ok := seen[r.Name]; ok {
				msg := fmt.Sprintf("duplicate replica name %q in tunnel.replicas", r.Name)
				return &gatewayValidationError{
					err: reconcile.TerminalError(fmt.Errorf("%s", msg)),
					cond: metav1.Condition{
						Type:    string(gatewayv1.GatewayConditionAccepted),
						Status:  metav1.ConditionFalse,
						Reason:  string(gatewayv1.GatewayReasonInvalidParameters),
						Message: msg,
					},
				}
			}
			seen[r.Name] = struct{}{}
		}
	}

	return nil
}

// listGatewayRoutes filters non-deleting HTTPRoutes from the pre-fetched list that
// reference the given Gateway via spec.parentRefs. Cross-namespace routes are only
// included if the Gateway's listener allowedRoutes configuration permits the route's
// namespace. Denied routes are returned separately so the caller can report them.
// Routes that appear in allRoutes but do not have a matching parentRef (e.g. stale
// status.parents entries from a previous parentRef) are returned as stale.
func listGatewayRoutes(ctx context.Context, r client.Client, allRoutes *gatewayv1.HTTPRouteList, gw *gatewayv1.Gateway) (allowed, denied, stale []*gatewayv1.HTTPRoute, err error) {
	for i := range allRoutes.Items {
		hr := &allRoutes.Items[i]

		if !hr.DeletionTimestamp.IsZero() {
			continue
		}

		// Check if the route actually has a parentRef to this Gateway.
		hasParentRef := false
		for _, ref := range hr.Spec.ParentRefs {
			if parentRefMatches(ref, gw, hr.Namespace) {
				hasParentRef = true
				break
			}
		}
		if !hasParentRef {
			// Route appeared in the index via a stale status.parents entry.
			stale = append(stale, hr)
			continue
		}

		ok, checkErr := routeNamespaceAllowed(ctx, r, gw, hr.Namespace)
		if checkErr != nil {
			return nil, nil, nil, fmt.Errorf("checking allowedRoutes for HTTPRoute %s/%s: %w", hr.Namespace, hr.Name, checkErr)
		}
		if !ok {
			denied = append(denied, hr)
			continue
		}
		allowed = append(allowed, hr)
	}
	slices.SortFunc(allowed, func(a, b *gatewayv1.HTTPRoute) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	slices.SortFunc(denied, func(a, b *gatewayv1.HTTPRoute) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	return allowed, denied, stale, nil
}

// routeNamespaceAllowed checks whether an HTTPRoute in routeNamespace is allowed
// to attach to the given Gateway based on the Gateway's listener allowedRoutes
// configuration. Returns true if any listener permits the route's namespace.
func routeNamespaceAllowed(ctx context.Context, r client.Reader, gw *gatewayv1.Gateway, routeNamespace string) (bool, error) {
	if routeNamespace == gw.Namespace {
		return true, nil
	}
	for _, l := range gw.Spec.Listeners {
		if l.AllowedRoutes == nil || l.AllowedRoutes.Namespaces == nil || l.AllowedRoutes.Namespaces.From == nil {
			continue // default is Same
		}
		switch *l.AllowedRoutes.Namespaces.From {
		case gatewayv1.NamespacesFromAll:
			return true, nil
		case gatewayv1.NamespacesFromSelector:
			if l.AllowedRoutes.Namespaces.Selector == nil {
				continue
			}
			selector, err := metav1.LabelSelectorAsSelector(l.AllowedRoutes.Namespaces.Selector)
			if err != nil {
				return false, reconcile.TerminalError(fmt.Errorf("parsing allowedRoutes namespace selector on listener %q: %w", l.Name, err))
			}
			var ns corev1.Namespace
			if err := r.Get(ctx, types.NamespacedName{Name: routeNamespace}, &ns); err != nil {
				return false, fmt.Errorf("getting namespace %q: %w", routeNamespace, err)
			}
			if selector.Matches(labels.Set(ns.Labels)) {
				return true, nil
			}
		}
	}
	return false, nil
}

// buildListenerStatuses builds the desired listener statuses for each Gateway listener,
// setting Accepted/Programmed conditions based on the attached route count.
func buildListenerStatuses(gw *gatewayv1.Gateway, attachedRoutes map[gatewayv1.SectionName]int32) []gatewayv1.ListenerStatus {
	now := metav1.Now()
	statuses := make([]gatewayv1.ListenerStatus, 0, len(gw.Spec.Listeners))
	for _, l := range gw.Spec.Listeners {
		// Protocol is validated in validateGateway before we reach here.
		statuses = append(statuses, gatewayv1.ListenerStatus{
			Name: l.Name,
			SupportedKinds: []gatewayv1.RouteGroupKind{
				{
					Group: new(gatewayv1.Group(gatewayv1.GroupVersion.Group)),
					Kind:  apiv1.KindHTTPRoute,
				},
			},
			AttachedRoutes: attachedRoutes[l.Name],
			Conditions: []metav1.Condition{
				{
					Type:               string(gatewayv1.ListenerConditionAccepted),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gw.Generation,
					LastTransitionTime: now,
					Reason:             string(gatewayv1.ListenerReasonAccepted),
					Message:            "Listener is accepted",
				},
				{
					Type:               string(gatewayv1.ListenerConditionProgrammed),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gw.Generation,
					LastTransitionTime: now,
					Reason:             string(gatewayv1.ListenerReasonProgrammed),
					Message:            "Listener is programmed",
				},
				{
					Type:               string(gatewayv1.ListenerConditionConflicted),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: gw.Generation,
					LastTransitionTime: now,
					Reason:             string(gatewayv1.ListenerReasonNoConflicts),
					Message:            "No conflicts",
				},
				{
					Type:               string(gatewayv1.ListenerConditionResolvedRefs),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gw.Generation,
					LastTransitionTime: now,
					Reason:             string(gatewayv1.ListenerReasonResolvedRefs),
					Message:            "References resolved",
				},
			},
		})
	}
	return statuses
}

// listenersChanged reports whether the desired listener statuses differ from
// the existing ones. It checks listener count, attached routes per listener,
// and each listener's conditions.
func listenersChanged(existing, desired []gatewayv1.ListenerStatus) bool {
	if len(existing) != len(desired) {
		return true
	}
	for _, d := range desired {
		var e *gatewayv1.ListenerStatus
		for i := range existing {
			if existing[i].Name == d.Name {
				e = &existing[i]
				break
			}
		}
		if e == nil {
			return true
		}
		if e.AttachedRoutes != d.AttachedRoutes {
			return true
		}
		for _, dc := range d.Conditions {
			if conditions.Changed(e.Conditions, dc.Type, dc.Status, dc.Reason, dc.Message, dc.ObservedGeneration) {
				return true
			}
		}
	}
	return false
}

// addressesChanged reports whether the desired addresses differ from the existing ones.
func addressesChanged(existing, desired []gatewayv1.GatewayStatusAddress) bool {
	if len(existing) != len(desired) {
		return true
	}
	for i := range desired {
		if existing[i].Value != desired[i].Value {
			return true
		}
		eType := gatewayv1.IPAddressType
		if existing[i].Type != nil {
			eType = *existing[i].Type
		}
		dType := gatewayv1.IPAddressType
		if desired[i].Type != nil {
			dType = *desired[i].Type
		}
		if eType != dType {
			return true
		}
	}
	return false
}

// countAttachedRoutes counts the number of allowed HTTPRoutes attached to each
// listener of the given Gateway. Routes without a sectionName count toward all listeners.
func countAttachedRoutes(routes []*gatewayv1.HTTPRoute, gw *gatewayv1.Gateway) map[gatewayv1.SectionName]int32 {
	counts := make(map[gatewayv1.SectionName]int32)
	for _, hr := range routes {
		for _, ref := range hr.Spec.ParentRefs {
			if !parentRefMatches(ref, gw, hr.Namespace) {
				continue
			}
			if ref.SectionName != nil {
				counts[*ref.SectionName]++
			} else {
				// Attach to all listeners.
				for _, l := range gw.Spec.Listeners {
					counts[l.Name]++
				}
			}
		}
	}
	return counts
}

// parentRefMatches reports whether the given parentRef points to the specified Gateway,
// defaulting group to gateway.networking.k8s.io, kind to Gateway, and namespace to
// the route's namespace when unset.
func parentRefMatches(ref gatewayv1.ParentReference, gw *gatewayv1.Gateway, routeNamespace string) bool {
	if ref.Group != nil && *ref.Group != gatewayv1.Group(gatewayv1.GroupName) {
		return false
	}
	if ref.Kind != nil && *ref.Kind != gatewayv1.Kind(apiv1.KindGateway) {
		return false
	}
	if string(ref.Name) != gw.Name {
		return false
	}
	refNS := routeNamespace
	if ref.Namespace != nil {
		refNS = string(*ref.Namespace)
	}
	return refNS == gw.Namespace
}

// findRouteParentStatus finds the RouteParentStatus entry for the given Gateway
// managed by our controller.
func findRouteParentStatus(statuses []gatewayv1.RouteParentStatus, gw *gatewayv1.Gateway) *gatewayv1.RouteParentStatus {
	for i, s := range statuses {
		if s.ControllerName != apiv1.ControllerName {
			continue
		}
		if string(s.ParentRef.Name) != gw.Name {
			continue
		}
		if s.ParentRef.Namespace != nil && string(*s.ParentRef.Namespace) == gw.Namespace {
			return &statuses[i]
		}
	}
	return nil
}

// removeRouteStatuses removes this Gateway's entry from status.parents on all
// HTTPRoutes that reference it. This is called during Gateway finalization.
func (r *GatewayReconciler) removeRouteStatuses(ctx context.Context, gw *gatewayv1.Gateway) error {
	var routes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routes, client.MatchingFields{
		indexHTTPRouteParentGateway: gw.Namespace + "/" + gw.Name,
	}); err != nil {
		return fmt.Errorf("listing HTTPRoutes for gateway %s/%s: %w", gw.Namespace, gw.Name, err)
	}
	for i := range routes.Items {
		if err := r.removeRouteStatus(ctx, gw, &routes.Items[i]); err != nil {
			return err
		}
	}
	return nil
}

// removeRouteStatus removes this Gateway's entry from status.parents on a
// single HTTPRoute. This is used for stale routes (parentRef removed) and
// during Gateway finalization.
func (r *GatewayReconciler) removeRouteStatus(ctx context.Context, gw *gatewayv1.Gateway, route *gatewayv1.HTTPRoute) error {
	routeKey := client.ObjectKeyFromObject(route)
	patched := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, routeKey, route); err != nil {
			return client.IgnoreNotFound(err)
		}
		if findRouteParentStatus(route.Status.Parents, gw) == nil {
			return nil
		}
		patch := client.MergeFromWithOptions(route.DeepCopy(), client.MergeFromWithOptimisticLock{})
		filtered := make([]gatewayv1.RouteParentStatus, 0, len(route.Status.Parents))
		for _, s := range route.Status.Parents {
			if s.ControllerName == apiv1.ControllerName &&
				string(s.ParentRef.Name) == gw.Name &&
				s.ParentRef.Namespace != nil && string(*s.ParentRef.Namespace) == gw.Namespace {
				continue
			}
			filtered = append(filtered, s)
		}
		route.Status.Parents = filtered
		patched = true
		if err := r.Status().Patch(ctx, route, patch); err != nil {
			return fmt.Errorf("removing status entry from HTTPRoute %s/%s: %w", route.Namespace, route.Name, err)
		}
		return nil
	})
	if patched {
		log.FromContext(ctx).V(1).Info("Removed status entry from HTTPRoute", "httproute", routeKey)
		r.Eventf(route, gw, corev1.EventTypeNormal, apiv1.ReasonReconciliationSucceeded,
			apiv1.EventActionReconcile, "Removed status entry for Gateway %s/%s", gw.Namespace, gw.Name)
	}
	return err
}

// updateDeniedRouteStatus sets Accepted=False/NotAllowedByListeners on the
// status.parents entry for this Gateway on a denied HTTPRoute (cross-namespace
// route not permitted by the Gateway's listener allowedRoutes configuration).
// Any stale conditions from a previous reconciliation (e.g. DNS) are removed.
func (r *GatewayReconciler) updateDeniedRouteStatus(ctx context.Context, gw *gatewayv1.Gateway, route *gatewayv1.HTTPRoute) error {
	acceptedType := string(gatewayv1.RouteConditionAccepted)
	acceptedStatus := metav1.ConditionFalse
	acceptedReason := string(gatewayv1.RouteReasonNotAllowedByListeners)
	acceptedMsg := fmt.Sprintf("Route namespace %q not allowed by any listener on Gateway %s/%s", route.Namespace, gw.Namespace, gw.Name)

	routeKey := client.ObjectKeyFromObject(route)
	patched := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, routeKey, route); err != nil {
			return client.IgnoreNotFound(err)
		}

		existing := findRouteParentStatus(route.Status.Parents, gw)

		// Skip if the entry already has exactly the right condition.
		if existing != nil &&
			len(existing.Conditions) == 1 &&
			!conditions.Changed(existing.Conditions, acceptedType, acceptedStatus,
				acceptedReason, acceptedMsg, route.Generation) {
			return nil
		}

		patch := client.MergeFromWithOptions(route.DeepCopy(), client.MergeFromWithOptimisticLock{})
		now := metav1.Now()

		if existing == nil {
			route.Status.Parents = append(route.Status.Parents, gatewayv1.RouteParentStatus{
				ParentRef: gatewayv1.ParentReference{
					Group:     new(gatewayv1.Group(gatewayv1.GroupName)),
					Kind:      new(gatewayv1.Kind(apiv1.KindGateway)),
					Namespace: new(gatewayv1.Namespace(gw.Namespace)),
					Name:      gatewayv1.ObjectName(gw.Name),
				},
				ControllerName: apiv1.ControllerName,
			})
			existing = &route.Status.Parents[len(route.Status.Parents)-1]
		}

		// Replace all conditions with just Accepted=False/NotAllowedByListeners,
		// removing any stale DNS or other conditions from a previous
		// reconciliation when the route was allowed.
		existing.Conditions = conditions.Set(existing.Conditions, []metav1.Condition{
			{
				Type:               acceptedType,
				Status:             acceptedStatus,
				ObservedGeneration: route.Generation,
				LastTransitionTime: now,
				Reason:             acceptedReason,
				Message:            acceptedMsg,
			},
		})

		patched = true
		return r.Status().Patch(ctx, route, patch)
	})
	if patched {
		log.FromContext(ctx).V(1).Info("Patched denied HTTPRoute status", "httproute", routeKey)
		r.Eventf(route, gw, corev1.EventTypeWarning, acceptedReason,
			apiv1.EventActionReconcile, acceptedMsg)
	}
	return err
}

// validateHTTPRoute checks that the HTTPRoute only uses features supported by
// Cloudflare tunnels. When sidecarEnabled is false, multiple backendRefs per
// rule are rejected because cloudflared ingress rules only support a single
// service per hostname+path. Returns a list of unsupported feature descriptions,
// or nil if valid.
func validateHTTPRoute(route *gatewayv1.HTTPRoute, sidecarEnabled bool) []string {
	var issues []string
	for i, ref := range route.Spec.ParentRefs {
		if ref.Port != nil {
			issues = append(issues, fmt.Sprintf("spec.parentRefs[%d].port is not supported", i))
		}
	}
	for i, rule := range route.Spec.Rules {
		if len(rule.Filters) > 0 {
			issues = append(issues, fmt.Sprintf("spec.rules[%d].filters is not supported", i))
		}
		if rule.Timeouts != nil {
			issues = append(issues, fmt.Sprintf("spec.rules[%d].timeouts is not supported", i))
		}
		if rule.Retry != nil {
			issues = append(issues, fmt.Sprintf("spec.rules[%d].retry is not supported", i))
		}
		if sp := rule.SessionPersistence; sp != nil {
			if !sidecarEnabled {
				issues = append(issues, fmt.Sprintf(
					"spec.rules[%d].sessionPersistence is not supported when sidecar is disabled", i))
			}
			if sp.IdleTimeout != nil && sp.Type != nil && *sp.Type == gatewayv1.HeaderBasedSessionPersistence {
				issues = append(issues, fmt.Sprintf(
					"spec.rules[%d].sessionPersistence.idleTimeout is not supported for header-based sessions", i))
			}
		}
		if len(rule.BackendRefs) == 0 {
			issues = append(issues, fmt.Sprintf("spec.rules[%d].backendRefs: at least one backend is required", i))
		}
		if len(rule.BackendRefs) > 1 && !sidecarEnabled {
			issues = append(issues, fmt.Sprintf("spec.rules[%d].backendRefs: multiple backends are not supported when sidecar is disabled; use a Kubernetes Service instead", i))
		}
		for j, ref := range rule.BackendRefs {
			if len(ref.Filters) > 0 {
				issues = append(issues, fmt.Sprintf("spec.rules[%d].backendRefs[%d].filters is not supported", i, j))
			}
			group := ""
			if ref.Group != nil {
				group = string(*ref.Group)
			}
			kind := apiv1.KindService
			if ref.Kind != nil {
				kind = string(*ref.Kind)
			}
			if group != "" || kind != apiv1.KindService {
				issues = append(issues, fmt.Sprintf("spec.rules[%d].backendRefs[%d]: only core Service backends are supported, got group=%q kind=%q", i, j, group, kind))
			}
		}
		for j, m := range rule.Matches {
			if len(m.Headers) > 0 {
				issues = append(issues, fmt.Sprintf("spec.rules[%d].matches[%d].headers is not supported", i, j))
			}
			if len(m.QueryParams) > 0 {
				issues = append(issues, fmt.Sprintf("spec.rules[%d].matches[%d].queryParams is not supported", i, j))
			}
			if m.Method != nil {
				issues = append(issues, fmt.Sprintf("spec.rules[%d].matches[%d].method is not supported", i, j))
			}
			if m.Path != nil && m.Path.Type != nil && *m.Path.Type != gatewayv1.PathMatchPathPrefix {
				issues = append(issues, fmt.Sprintf("spec.rules[%d].matches[%d].path.type %q is not supported; only PathPrefix is supported", i, j, *m.Path.Type))
			}
		}
	}
	return issues
}

// updateInvalidRouteStatus sets Accepted=False/UnsupportedValue on the
// status.parents entry for this Gateway on an HTTPRoute that uses unsupported
// features. Any stale conditions from a previous reconciliation are removed.
func (r *GatewayReconciler) updateInvalidRouteStatus(ctx context.Context, gw *gatewayv1.Gateway, route *gatewayv1.HTTPRoute, issues []string) error {
	acceptedType := string(gatewayv1.RouteConditionAccepted)
	acceptedStatus := metav1.ConditionFalse
	acceptedReason := string(gatewayv1.RouteReasonUnsupportedValue)
	acceptedMsg := "Unsupported features:\n- " + strings.Join(issues, "\n- ")

	routeKey := client.ObjectKeyFromObject(route)
	patched := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, routeKey, route); err != nil {
			return client.IgnoreNotFound(err)
		}

		existing := findRouteParentStatus(route.Status.Parents, gw)

		if existing != nil &&
			len(existing.Conditions) == 1 &&
			!conditions.Changed(existing.Conditions, acceptedType, acceptedStatus,
				acceptedReason, acceptedMsg, route.Generation) {
			return nil
		}

		patch := client.MergeFromWithOptions(route.DeepCopy(), client.MergeFromWithOptimisticLock{})
		now := metav1.Now()

		if existing == nil {
			route.Status.Parents = append(route.Status.Parents, gatewayv1.RouteParentStatus{
				ParentRef: gatewayv1.ParentReference{
					Group:     new(gatewayv1.Group(gatewayv1.GroupName)),
					Kind:      new(gatewayv1.Kind(apiv1.KindGateway)),
					Namespace: new(gatewayv1.Namespace(gw.Namespace)),
					Name:      gatewayv1.ObjectName(gw.Name),
				},
				ControllerName: apiv1.ControllerName,
			})
			existing = &route.Status.Parents[len(route.Status.Parents)-1]
		}

		existing.Conditions = conditions.Set(existing.Conditions, []metav1.Condition{
			{
				Type:               acceptedType,
				Status:             acceptedStatus,
				ObservedGeneration: route.Generation,
				LastTransitionTime: now,
				Reason:             acceptedReason,
				Message:            acceptedMsg,
			},
		})

		patched = true
		return r.Status().Patch(ctx, route, patch)
	})
	if patched {
		log.FromContext(ctx).V(1).Info("Patched invalid HTTPRoute status", "httproute", routeKey)
		r.Eventf(route, gw, corev1.EventTypeWarning, acceptedReason,
			apiv1.EventActionReconcile, acceptedMsg)
	}
	return err
}

// updateRouteStatuses iterates over allowed HTTPRoutes and delegates to
// updateRouteStatus for each one, collecting non-fatal errors.
func (r *GatewayReconciler) updateRouteStatuses(ctx context.Context, gw *gatewayv1.Gateway, routes []*gatewayv1.HTTPRoute, routesWithDeniedRefs map[types.NamespacedName][]string, dns dnsPolicy, dnsErr *string, readyStatus metav1.ConditionStatus, readyReason, readyMsg string) []string {
	var errs []string
	for _, route := range routes {
		deniedRefs := routesWithDeniedRefs[types.NamespacedName{Namespace: route.Namespace, Name: route.Name}]
		if err := r.updateRouteStatus(ctx, gw, route, deniedRefs, dns, dnsErr, readyStatus, readyReason, readyMsg); err != nil {
			errs = append(errs, fmt.Sprintf("failed to update HTTPRoute %s/%s status: %v", route.Namespace, route.Name, err))
		}
	}
	return errs
}

// updateRouteStatus updates the status.parents entry for this Gateway on the
// given HTTPRoute using merge-patch. Condition values are pre-computed by
// buildRouteConditions; this method handles the RetryOnConflict + patch logic.
// If the entry already exists with up-to-date conditions, no patch is issued.
func (r *GatewayReconciler) updateRouteStatus(ctx context.Context, gw *gatewayv1.Gateway, route *gatewayv1.HTTPRoute, deniedRefs []string, dns dnsPolicy, dnsErr *string, readyStatus metav1.ConditionStatus, readyReason, readyMsg string) error {
	acceptedType := string(gatewayv1.RouteConditionAccepted)
	resolvedRefsType := string(gatewayv1.RouteConditionResolvedRefs)
	rc := buildRouteConditions(route, deniedRefs, dns, dnsErr, readyStatus, readyReason, readyMsg)

	routeKey := client.ObjectKeyFromObject(route)
	patched := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, routeKey, route); err != nil {
			// NotFound must NOT be ignored: the route was deleted mid-reconciliation
			// and the controller needs a fresh Reconcile() to clean up DNS records
			// and tunnel ingress rules associated with this route.
			return err
		}

		existing := findRouteParentStatus(route.Status.Parents, gw)

		// Check if update is needed.
		if existing != nil {
			changed := conditions.Changed(existing.Conditions, acceptedType, metav1.ConditionTrue,
				string(gatewayv1.RouteReasonAccepted), "HTTPRoute is accepted", route.Generation) ||
				conditions.Changed(existing.Conditions, resolvedRefsType, rc.resolvedRefsStatus,
					rc.resolvedRefsReason, rc.resolvedRefsMsg, route.Generation)
			if rc.dnsEnabled {
				changed = changed || conditions.Changed(existing.Conditions, apiv1.ConditionDNSRecordsApplied,
					rc.dnsStatus, rc.dnsReason, rc.dnsMessage, route.Generation)
			} else {
				// DNS was disabled — check if we need to remove a stale condition.
				changed = changed || conditions.Find(existing.Conditions, apiv1.ConditionDNSRecordsApplied) != nil
			}
			changed = changed || conditions.Changed(existing.Conditions, apiv1.ConditionReady,
				rc.readyStatus, rc.readyReason, rc.readyMsg, route.Generation)
			if !changed {
				return nil
			}
		}

		patch := client.MergeFromWithOptions(route.DeepCopy(), client.MergeFromWithOptimisticLock{})
		now := metav1.Now()

		if existing == nil {
			route.Status.Parents = append(route.Status.Parents, gatewayv1.RouteParentStatus{
				ParentRef: gatewayv1.ParentReference{
					Group:     new(gatewayv1.Group(gatewayv1.GroupName)),
					Kind:      new(gatewayv1.Kind(apiv1.KindGateway)),
					Namespace: new(gatewayv1.Namespace(gw.Namespace)),
					Name:      gatewayv1.ObjectName(gw.Name),
				},
				ControllerName: apiv1.ControllerName,
			})
			existing = &route.Status.Parents[len(route.Status.Parents)-1]
		}

		existing.Conditions = conditions.Upsert(existing.Conditions, metav1.Condition{
			Type: acceptedType, Status: metav1.ConditionTrue,
			ObservedGeneration: route.Generation, LastTransitionTime: now,
			Reason: string(gatewayv1.RouteReasonAccepted), Message: "HTTPRoute is accepted",
		})
		existing.Conditions = conditions.Upsert(existing.Conditions, metav1.Condition{
			Type: resolvedRefsType, Status: rc.resolvedRefsStatus,
			ObservedGeneration: route.Generation, LastTransitionTime: now,
			Reason: rc.resolvedRefsReason, Message: rc.resolvedRefsMsg,
		})

		if rc.dnsEnabled {
			existing.Conditions = conditions.Upsert(existing.Conditions, metav1.Condition{
				Type: apiv1.ConditionDNSRecordsApplied, Status: rc.dnsStatus,
				ObservedGeneration: route.Generation, LastTransitionTime: now,
				Reason: rc.dnsReason, Message: rc.dnsMessage,
			})
		} else {
			existing.Conditions = conditions.Remove(existing.Conditions, apiv1.ConditionDNSRecordsApplied)
		}

		existing.Conditions = conditions.Upsert(existing.Conditions, metav1.Condition{
			Type: apiv1.ConditionReady, Status: rc.readyStatus,
			ObservedGeneration: route.Generation, LastTransitionTime: now,
			Reason: rc.readyReason, Message: rc.readyMsg,
		})

		patched = true
		return r.Status().Patch(ctx, route, patch)
	})
	if patched {
		log.FromContext(ctx).V(1).Info("Patched HTTPRoute status", "httproute", routeKey)
		eventType := corev1.EventTypeNormal
		if rc.readyStatus != metav1.ConditionTrue {
			eventType = corev1.EventTypeWarning
		}
		r.Eventf(route, gw, eventType, rc.readyReason,
			apiv1.EventActionReconcile, "Ready=%s: %s", rc.readyStatus, rc.readyMsg)
	}
	return err
}

// buildRouteConditions computes the desired condition values for an HTTPRoute's
// status.parents entry: ResolvedRefs, DNS (if enabled), and Ready. The Ready
// condition starts from the Gateway's readiness and is downgraded when DNS errors
// exist.
func buildRouteConditions(route *gatewayv1.HTTPRoute, deniedRefs []string, dns dnsPolicy, dnsErr *string, readyStatus metav1.ConditionStatus, readyReason, readyMsg string) routeConditions {
	rc := routeConditions{
		resolvedRefsStatus: metav1.ConditionTrue,
		resolvedRefsReason: string(gatewayv1.RouteReasonResolvedRefs),
		resolvedRefsMsg:    "References resolved",
		dnsEnabled:         dns.enabled,
		readyStatus:        readyStatus,
		readyReason:        readyReason,
		readyMsg:           readyMsg,
	}

	if len(deniedRefs) > 0 {
		rc.resolvedRefsStatus = metav1.ConditionFalse
		rc.resolvedRefsReason = string(gatewayv1.RouteReasonRefNotPermitted)
		rc.resolvedRefsMsg = "BackendRefs not permitted by ReferenceGrant:\n"
		for _, ref := range deniedRefs {
			rc.resolvedRefsMsg += "- " + ref + "\n"
		}
		rc.resolvedRefsMsg = strings.TrimSuffix(rc.resolvedRefsMsg, "\n")
	}

	if rc.dnsEnabled {
		if dnsErr != nil {
			rc.dnsStatus = metav1.ConditionUnknown
			rc.dnsReason = apiv1.ReasonProgressingWithRetry
			rc.dnsMessage = *dnsErr
		} else {
			rc.dnsStatus = metav1.ConditionTrue
			rc.dnsReason = apiv1.ReasonReconciliationSucceeded
			rc.dnsMessage = routeDNSMessage(route, dns)
		}
	}

	// Downgrade route Ready to Unknown/ProgressingWithRetry if there's a DNS error.
	if readyStatus == metav1.ConditionTrue && dnsErr != nil {
		rc.readyStatus = metav1.ConditionUnknown
		rc.readyReason = apiv1.ReasonProgressingWithRetry
		rc.readyMsg = *dnsErr
	}

	return rc
}

// routeDNSMessage builds a per-route DNS condition message listing the
// hostnames that were applied and those that were skipped (not in any
// configured zone).
func routeDNSMessage(route *gatewayv1.HTTPRoute, dns dnsPolicy) string {
	// When all zones are managed, every hostname is applied.
	if dns.allZones() {
		var msg strings.Builder
		msg.WriteString("Applied hostnames:")
		for _, h := range route.Spec.Hostnames {
			fmt.Fprintf(&msg, "\n- %s", string(h))
		}
		if len(route.Spec.Hostnames) == 0 {
			msg.WriteString("\n(none)")
		}
		return msg.String()
	}

	var applied, skipped []string
	for _, h := range route.Spec.Hostnames {
		hostname := string(h)
		matched := false
		for _, zoneName := range dns.zones {
			if hostnameInZone(hostname, zoneName) {
				matched = true
				break
			}
		}
		if matched {
			applied = append(applied, hostname)
		} else {
			skipped = append(skipped, hostname)
		}
	}
	var msg strings.Builder
	msg.WriteString("Applied hostnames:")
	if len(applied) == 0 {
		msg.WriteString("\n(none)")
	} else {
		for _, h := range applied {
			fmt.Fprintf(&msg, "\n- %s", h)
		}
	}
	msg.WriteString("\nSkipped hostnames (not in any configured zone):")
	if len(skipped) == 0 {
		msg.WriteString("\n(none)")
	} else {
		for _, h := range skipped {
			fmt.Fprintf(&msg, "\n- %s", h)
		}
	}
	return msg.String()
}
