#!/usr/bin/env bash
# Copyright 2026 Matheus Pimenta.
# SPDX-License-Identifier: AGPL-3.0
#
# Cleanup script for removing leftover Cloudflare resources (tunnels and DNS
# CNAME records) created by the controller or e2e tests.
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

echo "==> Listing all tunnels..."
TUNNELS=$(cfgwctl tunnel list)
echo "$TUNNELS" | jq -r '.[] | "  \(.name) (\(.id))"'

TUNNEL_COUNT=$(echo "$TUNNELS" | jq 'length')
echo "==> Found $TUNNEL_COUNT tunnel(s)"

if [ "$TUNNEL_COUNT" -eq 0 ]; then
    echo "==> Nothing to clean up"
    exit 0
fi

echo "==> Listing all zones..."
ZONE_IDS=$(cfgwctl dns list-zones | jq -r '.[]')

# For each tunnel, clean up DNS CNAMEs across all zones, then delete the tunnel.
echo "$TUNNELS" | jq -c '.[]' | while read -r tunnel; do
    TUNNEL_ID=$(echo "$tunnel" | jq -r '.id')
    TUNNEL_NAME=$(echo "$tunnel" | jq -r '.name')
    TUNNEL_TARGET="${TUNNEL_ID}.cfargotunnel.com"

    echo "==> Cleaning up tunnel '$TUNNEL_NAME' ($TUNNEL_ID)..."

    # Delete DNS CNAME records pointing to this tunnel across all zones.
    for ZONE_ID in $ZONE_IDS; do
        CNAMES=$(cfgwctl dns list-cnames --zone-id "$ZONE_ID" --target "$TUNNEL_TARGET" | jq -r '.hostnames[]')
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

echo ""
echo "==> Cleanup complete"
