// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package controller

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

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

// reconcileDNS reconciles DNS CNAME records for the Gateway. It lists all
// records pointing to the tunnel across all account zones, computes the
// desired set from route hostnames + configured zones + extra hostnames, and
// diffs: creating missing records and deleting stale ones. When dns is
// disabled, all records are deleted.
func (r *GatewayReconciler) reconcileDNS(
	ctx context.Context, tc cloudflare.Client,
	tunnelID string, dns dnsPolicy,
	routeHostnames []gatewayv1.Hostname,
	extraHostnames []string,
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
	desired := make(map[string]string) // hostname -> zoneID

	if dns.allZones() {
		// All-zones mode: resolve each hostname's zone dynamically.
		for _, h := range routeHostnames {
			hostname := string(h)
			zoneID, err := tc.FindZoneIDByHostname(ctx, hostname)
			if err != nil {
				// Skip hostnames whose zone cannot be resolved — they may
				// not belong to any zone in the account.
				l.V(1).Info("Skipping hostname: zone lookup failed", "hostname", hostname, "error", err)
				continue
			}
			desired[hostname] = zoneID
		}
		for _, hostname := range extraHostnames {
			zoneID, err := tc.FindZoneIDByHostname(ctx, hostname)
			if err != nil {
				l.V(1).Info("Skipping extra hostname: zone lookup failed", "hostname", hostname, "error", err)
				continue
			}
			desired[hostname] = zoneID
		}
	} else {
		// Specific-zones mode: resolve each configured zone name to a zone ID
		// and filter hostnames.
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
		for _, hostname := range extraHostnames {
			for _, zoneName := range dns.zones {
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
