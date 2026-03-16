// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
)

// dnsPolicy describes how DNS CNAME record management is configured for a
// Gateway during reconciliation.
type dnsPolicy struct {
	// enabled is true when DNS management is active.
	enabled bool
	// zones restricts CNAME management to hostnames matching these zones.
	// When empty and enabled is true, all hostnames are managed.
	zones []string
}

// allZones returns true when DNS is enabled for all hostnames (no zone filter).
func (d dnsPolicy) allZones() bool { return d.enabled && len(d.zones) == 0 }

// buildDNSPolicy constructs the DNS policy from parameters. When params or
// params.Spec.DNS is nil, DNS management is enabled for all zones.
func buildDNSPolicy(params *apiv1.CloudflareGatewayParameters) dnsPolicy {
	if params == nil || params.Spec.DNS == nil {
		return dnsPolicy{enabled: true}
	}
	if len(params.Spec.DNS.Zones) > 0 {
		zones := make([]string, len(params.Spec.DNS.Zones))
		for i, z := range params.Spec.DNS.Zones {
			zones[i] = z.Name
		}
		return dnsPolicy{enabled: true, zones: zones}
	}
	return dnsPolicy{}
}

// gatewayTunnelHosts returns the hostname addresses currently programmed on the
// Gateway. These are used as cache index keys to find Gateways by tunnel host.
func gatewayTunnelHosts(gw *gatewayv1.Gateway) []string {
	seen := make(map[string]struct{})
	var hosts []string
	for _, addr := range gw.Status.Addresses {
		if addr.Value == "" {
			continue
		}
		if addr.Type != nil && *addr.Type != gatewayv1.HostnameAddressType {
			continue
		}
		if _, ok := seen[addr.Value]; ok {
			continue
		}
		seen[addr.Value] = struct{}{}
		hosts = append(hosts, addr.Value)
	}
	return hosts
}

// computeDesiredDNSHostnames resolves the set of hostnames that should receive
// managed CNAMEs for the current DNS policy, returning hostname -> zoneID.
func computeDesiredDNSHostnames(ctx context.Context, tc cloudflare.Client, dns dnsPolicy, routeHostnames []gatewayv1.Hostname) (map[string]string, *string) {
	desired := make(map[string]string) // hostname -> zoneID

	if dns.allZones() {
		for _, h := range routeHostnames {
			hostname := string(h)
			zoneID, err := tc.FindZoneIDByHostname(ctx, hostname)
			if err != nil {
				log.FromContext(ctx).V(1).Info("Skipping hostname: zone lookup failed", "hostname", hostname, "error", err)
				continue
			}
			desired[hostname] = zoneID
		}
		return desired, nil
	}

	zoneMap := make(map[string]string, len(dns.zones)) // zoneName -> zoneID
	for _, zoneName := range dns.zones {
		zoneID, err := tc.FindZoneIDByHostname(ctx, zoneName)
		if err != nil {
			return nil, new(fmt.Sprintf("Failed to find zone ID for %q: %v", zoneName, err))
		}
		zoneMap[zoneName] = zoneID
	}
	for _, h := range routeHostnames {
		hostname := string(h)
		for _, zoneName := range dns.zones {
			if hostnameInZone(hostname, zoneName) {
				desired[hostname] = zoneMap[zoneName]
			}
		}
	}
	return desired, nil
}

// findHostnameRouteOwners returns the route owners currently programmed for
// hostname in the Gateway's route ConfigMap.
func (r *GatewayReconciler) findHostnameRouteOwners(ctx context.Context, gw *gatewayv1.Gateway, hostname string) ([]routeOwner, error) {
	cfg, err := r.getExistingRouteConfig(ctx, gw)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, nil
	}

	seen := make(map[string]struct{})
	var owners []routeOwner
	for _, route := range cfg.Routes {
		owner, ok := parseRouteOwner(route.OwnerKind, route.Owner)
		if route.Hostname != hostname || !ok {
			continue
		}
		ownerKey := route.OwnerKind + ":" + route.Owner
		if _, ok := seen[ownerKey]; ok {
			continue
		}
		seen[ownerKey] = struct{}{}
		owners = append(owners, owner)
	}
	sort.Slice(owners, func(i, j int) bool {
		if owners[i].kind != owners[j].kind {
			return owners[i].kind < owners[j].kind
		}
		if owners[i].namespace != owners[j].namespace {
			return owners[i].namespace < owners[j].namespace
		}
		return owners[i].name < owners[j].name
	})
	return owners, nil
}

// findManagedDNSHostnameConflicts detects desired DNS hostnames that already
// point to another Gateway's tunnel host. Only conflicts with Gateways known to
// this controller are reported so stale/unmanaged records don't block routing.
func (r *GatewayReconciler) findManagedDNSHostnameConflicts(
	ctx context.Context,
	gw *gatewayv1.Gateway,
	tc cloudflare.Client,
	tunnelID string,
	dns dnsPolicy,
	routes []routeObject,
) (map[string]string, error) {
	if !dns.enabled {
		return nil, nil
	}

	var routeHostnames []gatewayv1.Hostname
	for _, route := range routes {
		routeHostnames = append(routeHostnames, route.hostnames()...)
	}
	desired, dnsErr := computeDesiredDNSHostnames(ctx, tc, dns, routeHostnames)
	if dnsErr != nil {
		// Zone lookup failures will be caught later during DNS reconciliation
		// where they produce proper route status updates. Skip conflict detection.
		log.FromContext(ctx).V(1).Info("Skipping DNS conflict detection: zone lookup failed", "error", *dnsErr)
		return nil, nil
	}

	conflicts := make(map[string]string)
	currentTunnelHost := cloudflare.TunnelTarget(tunnelID)
	for hostname, zoneID := range desired {
		target, exists, err := tc.GetDNSCNAMETarget(ctx, zoneID, hostname)
		if err != nil {
			return nil, fmt.Errorf("looking up DNS CNAME for %q: %w", hostname, err)
		}
		if !exists || target == "" || target == currentTunnelHost {
			continue
		}

		var gateways gatewayv1.GatewayList
		if err := r.List(ctx, &gateways, client.MatchingFields{
			indexGatewayTunnelHost: target,
		}); err != nil {
			return nil, fmt.Errorf("listing Gateways for tunnel host %q: %w", target, err)
		}

		var owners []string
		for _, other := range gateways.Items {
			if other.Namespace == gw.Namespace && other.Name == gw.Name {
				continue
			}

			routeOwners, err := r.findHostnameRouteOwners(ctx, &other, hostname)
			if err != nil {
				return nil, fmt.Errorf("reading route owners for hostname %q on Gateway %s/%s: %w",
					hostname, other.Namespace, other.Name, err)
			}
			if len(routeOwners) == 0 {
				owners = append(owners, fmt.Sprintf("Gateway %s/%s", other.Namespace, other.Name))
				continue
			}
			for _, routeOwner := range routeOwners {
				owners = append(owners, fmt.Sprintf("%s on Gateway %s/%s", formatRouteOwner(routeOwner), other.Namespace, other.Name))
			}
		}
		if len(owners) == 0 {
			continue
		}
		sort.Strings(owners)

		conflicts[hostname] = fmt.Sprintf("%s (claimed by %s)", hostname, strings.Join(owners, ", "))
	}
	return conflicts, nil
}

// filterManagedDNSHostnameConflicts rejects routes whose DNS-managed hostnames
// are already claimed by another Gateway, reusing the existing route conflict
// status path (Accepted=False with conflict details in the message).
func (r *GatewayReconciler) filterManagedDNSHostnameConflicts(
	ctx context.Context,
	gw *gatewayv1.Gateway,
	tc cloudflare.Client,
	tunnelID string,
	dns dnsPolicy,
	routes []routeObject,
) ([]routeObject, []string, error) {
	conflicts, err := r.findManagedDNSHostnameConflicts(ctx, gw, tc, tunnelID, dns, routes)
	if err != nil {
		return nil, nil, err
	}
	if len(conflicts) == 0 {
		return routes, nil, nil
	}

	var filtered []routeObject
	var errs []string
	for _, route := range routes {
		var issues []string
		for _, hostname := range route.hostnames() {
			if issue, ok := conflicts[string(hostname)]; ok {
				issues = append(issues, issue)
			}
		}
		if len(issues) == 0 {
			filtered = append(filtered, route)
			continue
		}

		msg := "Conflicting hostname (already claimed by another Gateway):\n- " + strings.Join(issues, "\n- ")
		if err := r.rejectRouteStatus(ctx, gw, route,
			string(gatewayv1.RouteReasonUnsupportedValue), msg); err != nil {
			errs = append(errs, fmt.Sprintf("failed to update conflicting %s %s status: %v", route.routeKind(), route.key(), err))
		}
	}
	return filtered, errs, nil
}

// reconcileDNS reconciles DNS CNAME records for the Gateway. It lists all
// records pointing to the tunnel across all account zones, computes the
// desired set from route hostnames + configured zones, and diffs: creating
// missing records and deleting stale ones. When dns is disabled, all records
// are deleted.
func (r *GatewayReconciler) reconcileDNS(
	ctx context.Context, tc cloudflare.Client,
	tunnelID string, dns dnsPolicy,
	routeHostnames []gatewayv1.Hostname,
) ([]string, *string) {
	l := log.FromContext(ctx)
	tunnelTarget := cloudflare.TunnelTarget(tunnelID)

	// List all account zone IDs.
	accountZoneIDs, err := tc.ListZoneIDs(ctx)
	if err != nil {
		return nil, new(fmt.Sprintf("Failed to list zone IDs: %v", err))
	}

	// List all existing records pointing to our tunnel across all zones.
	type record struct {
		hostname string
		zoneID   string
	}
	var actual []record
	for _, zoneID := range accountZoneIDs {
		hostnames, err := tc.ListDNSCNAMEsByTarget(ctx, zoneID, tunnelTarget)
		if err != nil {
			return nil, new(fmt.Sprintf("Failed to list DNS CNAMEs in zone %q: %v", zoneID, err))
		}
		for _, h := range hostnames {
			actual = append(actual, record{hostname: h, zoneID: zoneID})
		}
	}

	// When DNS is disabled, delete everything.
	if !dns.enabled {
		var changes []string
		for _, r := range actual {
			if err := tc.DeleteDNSCNAME(ctx, r.zoneID, r.hostname); err != nil {
				return changes, new(fmt.Sprintf("Failed to delete DNS CNAME for %q: %v", r.hostname, err))
			}
			changes = append(changes, fmt.Sprintf("deleted DNS CNAME for %s", r.hostname))
			l.V(1).Info("Deleted DNS CNAME", "hostname", r.hostname, "zoneID", r.zoneID)
		}
		return changes, nil
	}

	// Compute desired hostnames from all attached HTTPRoutes.
	// Each desired hostname is mapped to the zone ID it should live in.
	desired, dnsErr := computeDesiredDNSHostnames(ctx, tc, dns, routeHostnames)
	if dnsErr != nil {
		return nil, dnsErr
	}

	actualSet := make(map[string]struct{}, len(actual))
	for _, r := range actual {
		actualSet[r.hostname] = struct{}{}
	}

	var dnsChanges []string

	// Create missing records.
	for hostname, zoneID := range desired {
		if _, ok := actualSet[hostname]; !ok {
			if err := tc.EnsureDNSCNAME(ctx, zoneID, hostname, tunnelTarget); err != nil {
				return dnsChanges, new(fmt.Sprintf("Failed to ensure DNS CNAME for %q: %v", hostname, err))
			}
			dnsChanges = append(dnsChanges, fmt.Sprintf("created DNS CNAME for %s", hostname))
			l.V(1).Info("Created DNS CNAME", "hostname", hostname, "zoneID", zoneID)
		}
	}

	// Delete stale records: anything pointing to our tunnel that isn't desired.
	for _, r := range actual {
		if _, ok := desired[r.hostname]; !ok {
			if err := tc.DeleteDNSCNAME(ctx, r.zoneID, r.hostname); err != nil {
				return dnsChanges, new(fmt.Sprintf("Failed to delete stale DNS CNAME for %q: %v", r.hostname, err))
			}
			dnsChanges = append(dnsChanges, fmt.Sprintf("deleted DNS CNAME for %s", r.hostname))
			l.V(1).Info("Deleted stale DNS CNAME", "hostname", r.hostname, "zoneID", r.zoneID)
		}
	}

	return dnsChanges, nil
}

// hostnameInZone reports whether a hostname is a direct (single-level)
// subdomain of zoneName. For example, "app.example.com" matches zone
// "example.com", but "deep.sub.example.com" and "example.com" do not.
func hostnameInZone(hostname, zoneName string) bool {
	prefix, ok := strings.CutSuffix(hostname, "."+zoneName)
	return ok && !strings.Contains(prefix, ".")
}
