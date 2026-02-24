// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	apiv1 "github.com/matheuscscp/cloudflare-gateway-controller/api/v1"
	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
)

// reconcileDNS reconciles DNS CNAME records for the Gateway's zone. When
// zoneName is empty, all CNAME records pointing to the tunnel across all
// account zones are deleted. Returns a non-nil *string on failure (using
// new(fmt.Sprintf(...)) to allocate the error message inline), which is
// reported on each HTTPRoute's DNSRecordsApplied condition.
func (r *GatewayReconciler) reconcileDNS(ctx context.Context, tc cloudflare.Client, gw *gatewayv1.Gateway, tunnelID, zoneName string, routes []*gatewayv1.HTTPRoute) ([]string, *string) {
	l := log.FromContext(ctx)

	if zoneName == "" {
		changes, err := r.cleanupAllDNS(ctx, tc, tunnelID)
		if err != nil {
			return nil, new(fmt.Sprintf("Failed to clean up DNS records: %v", err))
		}
		return changes, nil
	}

	zoneID, err := tc.FindZoneIDByHostname(ctx, zoneName)
	if err != nil {
		return nil, new(fmt.Sprintf("Failed to find zone ID for %q: %v", zoneName, err))
	}

	// Compute desired hostnames from all attached HTTPRoutes.
	desired := make(map[string]struct{})
	for _, route := range routes {
		for _, h := range route.Spec.Hostnames {
			hostname := string(h)
			if hostnameInZone(hostname, zoneName) {
				desired[hostname] = struct{}{}
			}
		}
	}

	// Query actual CNAME records pointing to our tunnel.
	tunnelTarget := cloudflare.TunnelTarget(tunnelID)
	actual, err := tc.ListDNSCNAMEsByTarget(ctx, zoneID, tunnelTarget)
	if err != nil {
		return nil, new(fmt.Sprintf("Failed to list DNS CNAMEs: %v", err))
	}
	actualSet := make(map[string]struct{}, len(actual))
	for _, h := range actual {
		actualSet[h] = struct{}{}
	}

	// Create missing records.
	dnsComment := apiv1.ResourceDescription(gw)
	var dnsChanges []string
	for h := range desired {
		if _, ok := actualSet[h]; !ok {
			if err := tc.EnsureDNSCNAME(ctx, zoneID, h, tunnelTarget, dnsComment); err != nil {
				return dnsChanges, new(fmt.Sprintf("Failed to ensure DNS CNAME for %q: %v", h, err))
			}
			dnsChanges = append(dnsChanges, fmt.Sprintf("created DNS CNAME for %s", h))
			l.V(1).Info("Created DNS CNAME", "hostname", h)
		}
	}

	// Delete stale records.
	for h := range actualSet {
		if _, ok := desired[h]; !ok {
			if err := tc.DeleteDNSCNAME(ctx, zoneID, h); err != nil {
				return dnsChanges, new(fmt.Sprintf("Failed to delete stale DNS CNAME for %q: %v", h, err))
			}
			dnsChanges = append(dnsChanges, fmt.Sprintf("deleted DNS CNAME for %s", h))
			l.V(1).Info("Deleted stale DNS CNAME", "hostname", h)
		}
	}

	return dnsChanges, nil
}

// cleanupAllDNS deletes all CNAME records pointing to the tunnel across all
// account zones. This is used when DNS config is removed or during finalization.
// Returns a change message for each deleted record.
func (r *GatewayReconciler) cleanupAllDNS(ctx context.Context, tc cloudflare.Client, tunnelID string) ([]string, error) {
	l := log.FromContext(ctx)
	tunnelTarget := cloudflare.TunnelTarget(tunnelID)

	zoneIDs, err := tc.ListZoneIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing zones: %w", err)
	}

	var changes []string
	for _, zoneID := range zoneIDs {
		hostnames, err := tc.ListDNSCNAMEsByTarget(ctx, zoneID, tunnelTarget)
		if err != nil {
			return changes, fmt.Errorf("listing DNS CNAMEs in zone %s: %w", zoneID, err)
		}
		for _, h := range hostnames {
			if err := tc.DeleteDNSCNAME(ctx, zoneID, h); err != nil {
				return changes, fmt.Errorf("deleting DNS CNAME %q in zone %s: %w", h, zoneID, err)
			}
			changes = append(changes, fmt.Sprintf("deleted DNS CNAME for %s", h))
			l.V(1).Info("Deleted DNS CNAME", "hostname", h, "zoneID", zoneID)
		}
	}

	return changes, nil
}

// cleanupDNSInZone deletes all CNAME records pointing to the tunnel in a
// specific zone. Used when the DNS zone changes to clean up records in the
// old zone before creating them in the new one.
func (r *GatewayReconciler) cleanupDNSInZone(ctx context.Context, tc cloudflare.Client, tunnelID, zoneName, zoneID string) ([]string, error) {
	l := log.FromContext(ctx)

	if zoneID == "" {
		var err error
		zoneID, err = tc.FindZoneIDByHostname(ctx, zoneName)
		if err != nil {
			return nil, fmt.Errorf("finding zone ID for %q: %w", zoneName, err)
		}
	}

	tunnelTarget := cloudflare.TunnelTarget(tunnelID)
	hostnames, err := tc.ListDNSCNAMEsByTarget(ctx, zoneID, tunnelTarget)
	if err != nil {
		return nil, fmt.Errorf("listing DNS CNAMEs in zone %s: %w", zoneName, err)
	}

	var changes []string
	for _, h := range hostnames {
		if err := tc.DeleteDNSCNAME(ctx, zoneID, h); err != nil {
			return changes, fmt.Errorf("deleting DNS CNAME %q in zone %s: %w", h, zoneName, err)
		}
		changes = append(changes, fmt.Sprintf("deleted DNS CNAME for %s (zone changed)", h))
		l.V(1).Info("Deleted DNS CNAME (zone changed)", "hostname", h, "zone", zoneName)
	}

	return changes, nil
}

// hostnameInZone reports whether a hostname is a direct (single-level)
// subdomain of zoneName. For example, "app.example.com" matches zone
// "example.com", but "deep.sub.example.com" and "example.com" do not.
func hostnameInZone(hostname, zoneName string) bool {
	prefix, ok := strings.CutSuffix(hostname, "."+zoneName)
	return ok && !strings.Contains(prefix, ".")
}
