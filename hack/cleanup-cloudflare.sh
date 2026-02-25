#!/usr/bin/env bash
# Copyright 2026 Matheus Pimenta.
# SPDX-License-Identifier: AGPL-3.0
#
# Cleanup script for removing ALL Cloudflare resources in the test account:
# load balancers, pools, monitors, tunnels, and DNS CNAME records.
#
# This account is dedicated to this project's e2e tests, so we clean
# everything without worrying about naming conventions.
#
# Optional environment variables:
#   CREDENTIALS_FILE   — path to credentials file (default: ./api.token)
#   CFGWCTL            — path to cfgwctl binary (default: ./bin/cfgwctl)

set -euo pipefail

CREDENTIALS_FILE="${CREDENTIALS_FILE:-./api.token}"
CFGWCTL="${CFGWCTL:-./bin/cfgwctl}"

# Validate prerequisites.
for cmd in jq "$CFGWCTL"; do
    command -v "$cmd" >/dev/null 2>&1 || { echo "FATAL: required command not found: $cmd"; exit 1; }
done
[ -f "$CREDENTIALS_FILE" ] || { echo "FATAL: credentials file not found: $CREDENTIALS_FILE"; exit 1; }

# cfgwctl shorthand.
cfgwctl() { "$CFGWCTL" --credentials-file "$CREDENTIALS_FILE" "$@"; }

echo "==> Listing all zones..."
ZONE_IDS=$(cfgwctl dns list-zones | jq -r '.zoneIds[]')

# ─── Load Balancer cleanup ────────────────────────────────────────────────────
# Order matters: delete LBs first (they reference pools), then pools, then monitors.

echo "==> Listing all pools..."
POOLS=$(cfgwctl lb list-pools --prefix "")
POOL_COUNT=$(echo "$POOLS" | jq 'length')
echo "==> Found $POOL_COUNT pool(s)"

# Collect unique monitor IDs from pools before deleting them.
MONITOR_IDS=""
if [ "$POOL_COUNT" -gt 0 ]; then
    for pool_name in $(echo "$POOLS" | jq -r '.[].name'); do
        mid=$(cfgwctl lb get-pool --name "$pool_name" | jq -r '.monitorId // empty')
        if [ -n "$mid" ]; then
            MONITOR_IDS="$MONITOR_IDS $mid"
        fi
    done
    # Deduplicate.
    MONITOR_IDS=$(echo "$MONITOR_IDS" | tr ' ' '\n' | sort -u | tr '\n' ' ')
fi

# Delete LBs across all zones.
for ZONE_ID in $ZONE_IDS; do
    HOSTNAMES=$(cfgwctl lb list-hostnames --zone-id "$ZONE_ID" | jq -r '.hostnames[]' 2>/dev/null || true)
    for HOSTNAME in $HOSTNAMES; do
        echo "    Deleting LB '$HOSTNAME' in zone $ZONE_ID..."
        cfgwctl lb delete --zone-id "$ZONE_ID" --hostname "$HOSTNAME" || true
    done
done

# Delete pools.
if [ "$POOL_COUNT" -gt 0 ]; then
    echo "$POOLS" | jq -c '.[]' | while read -r pool; do
        POOL_ID=$(echo "$pool" | jq -r '.id')
        POOL_NAME=$(echo "$pool" | jq -r '.name')
        echo "    Deleting pool '$POOL_NAME' ($POOL_ID)..."
        cfgwctl lb delete-pool --pool-id "$POOL_ID" || true
    done
fi

# Delete monitors.
for MID in $MONITOR_IDS; do
    [ -n "$MID" ] || continue
    echo "    Deleting monitor $MID..."
    cfgwctl lb delete-monitor --monitor-id "$MID" || true
done

# ─── Tunnel and DNS cleanup ──────────────────────────────────────────────────

echo "==> Listing all tunnels..."
TUNNELS=$(cfgwctl tunnel list)
echo "$TUNNELS" | jq -r '.[] | "  \(.name) (\(.id))"'

TUNNEL_COUNT=$(echo "$TUNNELS" | jq 'length')
echo "==> Found $TUNNEL_COUNT tunnel(s)"

if [ "$TUNNEL_COUNT" -gt 0 ]; then
    # For each tunnel, clean up DNS CNAMEs across all zones, then delete the tunnel.
    echo "$TUNNELS" | jq -c '.[]' | while read -r tunnel; do
        TUNNEL_ID=$(echo "$tunnel" | jq -r '.id')
        TUNNEL_NAME=$(echo "$tunnel" | jq -r '.name')
        TUNNEL_TARGET="${TUNNEL_ID}.cfargotunnel.com"

        echo "==> Cleaning up tunnel '$TUNNEL_NAME' ($TUNNEL_ID)..."

        # Delete DNS CNAME records pointing to this tunnel across all zones.
        for ZONE_ID in $ZONE_IDS; do
            CNAMES=$(cfgwctl dns list-cnames --zone-id "$ZONE_ID" --target "$TUNNEL_TARGET" | jq -r '.hostnames[]' 2>/dev/null || true)
            for CNAME in $CNAMES; do
                echo "    Deleting DNS CNAME '$CNAME' in zone $ZONE_ID..."
                cfgwctl dns delete-cname --zone-id "$ZONE_ID" --hostname "$CNAME"
            done
        done

        # Clean up connections and delete the tunnel.
        echo "    Cleaning up connections..."
        cfgwctl tunnel cleanup-connections --tunnel-id "$TUNNEL_ID"
        echo "    Deleting tunnel..."
        cfgwctl tunnel delete --tunnel-id "$TUNNEL_ID"
        echo "    Done"
    done
fi

echo ""
echo "==> Cleanup complete"
