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

// reconcileDNS reconciles DNS CNAME records for the Gateway. It lists all
// records pointing to the tunnel across all account zones, computes the
// desired set from routes + configured zones, and diffs: creating missing
// records and deleting stale ones. When zoneNames is empty, all records
// are deleted.
func (r *GatewayReconciler) reconcileDNS(
	ctx context.Context, tc cloudflare.Client, gw *gatewayv1.Gateway,
	tunnelID string, zoneNames []string,
	routes []*gatewayv1.HTTPRoute,
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

	// When no zones are configured, delete everything.
	if len(zoneNames) == 0 {
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

	// Resolve each configured zone name to a zone ID.
	zoneMap := make(map[string]string, len(zoneNames)) // zoneName -> zoneID
	for _, zoneName := range zoneNames {
		zoneID, err := tc.FindZoneIDByHostname(ctx, zoneName)
		if err != nil {
			return nil, new(fmt.Sprintf("Failed to find zone ID for %q: %v", zoneName, err))
		}
		zoneMap[zoneName] = zoneID
	}

	// Compute desired hostnames from all attached HTTPRoutes.
	// Each desired hostname is mapped to the zone ID it should live in.
	desired := make(map[string]string) // hostname -> zoneID
	for _, route := range routes {
		for _, h := range route.Spec.Hostnames {
			hostname := string(h)
			for _, zoneName := range zoneNames {
				if hostnameInZone(hostname, zoneName) {
					desired[hostname] = zoneMap[zoneName]
				}
			}
		}
	}

	actualSet := make(map[string]struct{}, len(actual))
	for _, r := range actual {
		actualSet[r.hostname] = struct{}{}
	}

	dnsComment := apiv1.ResourceDescription(gw)
	var dnsChanges []string

	// Create missing records.
	for hostname, zoneID := range desired {
		if _, ok := actualSet[hostname]; !ok {
			if err := tc.EnsureDNSCNAME(ctx, zoneID, hostname, tunnelTarget, dnsComment); err != nil {
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
