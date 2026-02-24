#!/usr/bin/env bash
# Copyright 2026 Matheus Pimenta.
# SPDX-License-Identifier: AGPL-3.0
#
# End-to-end tests for the TrafficSplitting load balancer topology (no AZs).
# Uses a single-node kind cluster. Each unique Service in an HTTPRoute
# backendRef gets its own tunnel, Deployment, and pool. Pool weights are
# derived from HTTPRoute backendRef weights.
#
# Required environment variables:
#   TEST_ZONE_NAME  — Cloudflare DNS zone name for testing
#
# Optional: IMAGE, CREDENTIALS_FILE, CFGWCTL, REUSE_CLUSTER,
#           REUSE_CONTROLLER, RELOAD_CONTROLLER, TEST

set -euo pipefail

# Defaults.
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cfgw-e2e-ts}"
TEST_NS="${TEST_NS:-cfgw-e2e-ts}"

# Source shared library.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/e2e-lib.sh"

validate_prerequisites
setup_cluster
register_cleanup
install_controller
start_controller_log_stream
create_test_namespace
ensure_gatewayclass

# ─── Shared state across tests ───────────────────────────────────────────────
# These are populated by test_ts_basic and used by subsequent tests.
GW_NAME="ts-gw"
TUNNEL_NAME_ALPHA=""
TUNNEL_NAME_BETA=""
TUNNEL_NAME_GAMMA=""
POOL_NAME_ALPHA=""
POOL_NAME_BETA=""
POOL_NAME_GAMMA=""
MONITOR_NAME=""
ZONE_ID=""
HOSTNAME_A=""
HOSTNAME_B=""

# ─── Test functions ───────────────────────────────────────────────────────────

test_ts_basic() {
    log "Creating CloudflareGatewayParameters with TrafficSplitting topology..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: ts-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zone:
      name: "$TEST_ZONE_NAME"
  loadBalancer:
    topology: TrafficSplitting
    steeringPolicy: Random
    monitor:
      type: HTTPS
      path: /healthz
EOF

    log "Creating Gateway 'ts-gw'..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: ts-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: ts-params
  listeners:
  - name: http
    protocol: HTTP
    port: 80
EOF

    log "Waiting for Gateway to be Programmed..."
    retry 120 2 kubectl wait gateway/ts-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "ts-gw did not become Programmed"
    pass "ts-gw is Programmed"

    TUNNEL_NAME_ALPHA=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "$GW_NAME" "$TEST_NS" "svc-alpha")
    TUNNEL_NAME_BETA=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "$GW_NAME" "$TEST_NS" "svc-beta")
    TUNNEL_NAME_GAMMA=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "$GW_NAME" "$TEST_NS" "svc-gamma")
    POOL_NAME_ALPHA="$TUNNEL_NAME_ALPHA"
    POOL_NAME_BETA="$TUNNEL_NAME_BETA"
    POOL_NAME_GAMMA="$TUNNEL_NAME_GAMMA"
    MONITOR_NAME=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "$GW_NAME" "monitor")

    HOSTNAME_A="ts-a-${TS: -6}.${TEST_ZONE_NAME}"

    log "Creating Services svc-alpha and svc-beta..."
    for svc in svc-alpha svc-beta; do
        kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: $svc
  namespace: $TEST_NS
spec:
  ports:
  - port: 80
    protocol: TCP
EOF
    done

    log "Creating HTTPRoute with hostname '$HOSTNAME_A' and 2 weighted backendRefs..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: ts-route-a
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: ts-gw
  hostnames:
  - "$HOSTNAME_A"
  rules:
  - backendRefs:
    - name: svc-alpha
      port: 80
      weight: 70
    - name: svc-beta
      port: 80
      weight: 30
EOF

    # Verify 2 tunnels (one per service).
    local tunnel_id_alpha tunnel_id_beta
    log "Verifying tunnel for svc-alpha..."
    retry 60 3 bash -c "
        id=\$(cfgwctl tunnel get-id --name '$TUNNEL_NAME_ALPHA' | jq -r '.tunnelId')
        [ -n \"\$id\" ] && [ \"\$id\" != 'null' ]
    " || fail "tunnel svc-alpha not found"
    tunnel_id_alpha=$(cfgwctl tunnel get-id --name "$TUNNEL_NAME_ALPHA" | jq -r '.tunnelId')
    pass "Tunnel svc-alpha exists: $tunnel_id_alpha"

    log "Verifying tunnel for svc-beta..."
    retry 60 3 bash -c "
        id=\$(cfgwctl tunnel get-id --name '$TUNNEL_NAME_BETA' | jq -r '.tunnelId')
        [ -n \"\$id\" ] && [ \"\$id\" != 'null' ]
    " || fail "tunnel svc-beta not found"
    tunnel_id_beta=$(cfgwctl tunnel get-id --name "$TUNNEL_NAME_BETA" | jq -r '.tunnelId')
    pass "Tunnel svc-beta exists: $tunnel_id_beta"

    # Verify 2 Deployments.
    log "Verifying cloudflared Deployments..."
    retry 60 3 kubectl get deployment cloudflared-ts-gw-svc-alpha -n "$TEST_NS" \
        || fail "Deployment for svc-alpha not found"
    retry 60 3 kubectl get deployment cloudflared-ts-gw-svc-beta -n "$TEST_NS" \
        || fail "Deployment for svc-beta not found"
    pass "Both Deployments exist"

    # Verify each tunnel config routes to its own service only.
    log "Verifying svc-alpha tunnel config routes to svc-alpha..."
    retry 60 3 bash -c "cfgwctl tunnel get-config --tunnel-id '$tunnel_id_alpha' | jq -e '.[] | select(.hostname == \"$HOSTNAME_A\")' >/dev/null" \
        || fail "tunnel svc-alpha missing hostname $HOSTNAME_A"
    local alpha_service
    alpha_service=$(cfgwctl tunnel get-config --tunnel-id "$tunnel_id_alpha" | jq -r ".[] | select(.hostname == \"$HOSTNAME_A\") | .service")
    echo "$alpha_service" | grep -q "svc-alpha" || fail "svc-alpha tunnel routes to wrong service: $alpha_service"
    pass "Tunnel svc-alpha routes to svc-alpha"

    log "Verifying svc-beta tunnel config routes to svc-beta..."
    retry 60 3 bash -c "cfgwctl tunnel get-config --tunnel-id '$tunnel_id_beta' | jq -e '.[] | select(.hostname == \"$HOSTNAME_A\")' >/dev/null" \
        || fail "tunnel svc-beta missing hostname $HOSTNAME_A"
    local beta_service
    beta_service=$(cfgwctl tunnel get-config --tunnel-id "$tunnel_id_beta" | jq -r ".[] | select(.hostname == \"$HOSTNAME_A\") | .service")
    echo "$beta_service" | grep -q "svc-beta" || fail "svc-beta tunnel routes to wrong service: $beta_service"
    pass "Tunnel svc-beta routes to svc-beta"

    # Verify monitor exists.
    log "Verifying monitor exists..."
    retry 60 3 bash -c "
        id=\$(cfgwctl lb get-monitor --name '$MONITOR_NAME' | jq -r '.monitorId')
        [ -n \"\$id\" ] && [ \"\$id\" != '' ]
    " || fail "monitor not found"
    local monitor_id
    monitor_id=$(cfgwctl lb get-monitor --name "$MONITOR_NAME" | jq -r '.monitorId')
    pass "Monitor exists: $monitor_id"

    # Verify 2 pools (one per service).
    log "Verifying pools exist..."
    retry 60 3 bash -c "[ \$(cfgwctl lb list-pools --prefix 'gw-' | jq 'length') -eq 2 ]" \
        || fail "expected 2 pools"
    pass "2 pools exist"

    # Verify pool origins point to correct tunnels.
    log "Verifying pool svc-alpha origin..."
    retry 60 3 bash -c "
        origin=\$(cfgwctl lb get-pool --name '$POOL_NAME_ALPHA' | jq -r '.origins[0].address')
        [ \"\$origin\" = '${tunnel_id_alpha}.cfargotunnel.com' ]
    " || fail "pool svc-alpha origin mismatch"
    pass "Pool svc-alpha origin correct"

    log "Verifying pool svc-beta origin..."
    retry 60 3 bash -c "
        origin=\$(cfgwctl lb get-pool --name '$POOL_NAME_BETA' | jq -r '.origins[0].address')
        [ \"\$origin\" = '${tunnel_id_beta}.cfargotunnel.com' ]
    " || fail "pool svc-beta origin mismatch"
    pass "Pool svc-beta origin correct"

    # Look up zone ID for later tests.
    ZONE_ID=$(cfgwctl dns find-zone --hostname "test.${TEST_ZONE_NAME}" | jq -r '.zoneId')
    [ -n "$ZONE_ID" ] && [ "$ZONE_ID" != "null" ] || fail "zone ID not found"

    # Verify LB exists for the hostname.
    log "Verifying load balancer for '$HOSTNAME_A'..."
    retry 60 3 bash -c "cfgwctl lb list-hostnames --zone-id '$ZONE_ID' | jq -e '.hostnames[] | select(. == \"$HOSTNAME_A\")' >/dev/null" \
        || fail "LB for $HOSTNAME_A not found"
    pass "Load balancer exists for $HOSTNAME_A"
}

test_ts_multi_hostname() {
    HOSTNAME_B="ts-b-${TS: -6}.${TEST_ZONE_NAME}"

    log "Creating Service svc-gamma..."
    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: svc-gamma
  namespace: $TEST_NS
spec:
  ports:
  - port: 80
    protocol: TCP
EOF

    log "Creating second HTTPRoute with hostname '$HOSTNAME_B' referencing svc-alpha and svc-gamma..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: ts-route-b
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: ts-gw
  hostnames:
  - "$HOSTNAME_B"
  rules:
  - backendRefs:
    - name: svc-alpha
      port: 80
      weight: 60
    - name: svc-gamma
      port: 80
      weight: 40
EOF

    # Verify 3rd tunnel created for svc-gamma.
    log "Verifying tunnel for svc-gamma..."
    retry 60 3 bash -c "
        id=\$(cfgwctl tunnel get-id --name '$TUNNEL_NAME_GAMMA' | jq -r '.tunnelId')
        [ -n \"\$id\" ] && [ \"\$id\" != 'null' ]
    " || fail "tunnel svc-gamma not found"
    local tunnel_id_gamma
    tunnel_id_gamma=$(cfgwctl tunnel get-id --name "$TUNNEL_NAME_GAMMA" | jq -r '.tunnelId')
    pass "Tunnel svc-gamma exists: $tunnel_id_gamma"

    # Verify 3rd Deployment created.
    log "Verifying Deployment for svc-gamma..."
    retry 60 3 kubectl get deployment cloudflared-ts-gw-svc-gamma -n "$TEST_NS" \
        || fail "Deployment for svc-gamma not found"
    pass "Deployment svc-gamma exists"

    # Verify 3 pools total.
    log "Verifying 3 pools exist..."
    retry 60 3 bash -c "[ \$(cfgwctl lb list-pools --prefix 'gw-' | jq 'length') -eq 3 ]" \
        || fail "expected 3 pools"
    pass "3 pools exist"

    # Verify svc-gamma pool origin.
    log "Verifying pool svc-gamma origin..."
    local pool_gamma_origin
    pool_gamma_origin=$(cfgwctl lb get-pool --name "$POOL_NAME_GAMMA" | jq -r '.origins[0].address')
    [ "$pool_gamma_origin" = "${tunnel_id_gamma}.cfargotunnel.com" ] \
        || fail "pool svc-gamma origin mismatch: got $pool_gamma_origin, want ${tunnel_id_gamma}.cfargotunnel.com"
    pass "Pool svc-gamma origin correct"

    # Verify svc-alpha tunnel config now has BOTH hostnames.
    local tunnel_id_alpha
    tunnel_id_alpha=$(cfgwctl tunnel get-id --name "$TUNNEL_NAME_ALPHA" | jq -r '.tunnelId')
    log "Verifying svc-alpha tunnel has both hostnames..."
    retry 60 3 bash -c "cfgwctl tunnel get-config --tunnel-id '$tunnel_id_alpha' | jq -e '.[] | select(.hostname == \"$HOSTNAME_B\")' >/dev/null" \
        || fail "tunnel svc-alpha missing hostname $HOSTNAME_B"
    cfgwctl tunnel get-config --tunnel-id "$tunnel_id_alpha" | jq -e ".[] | select(.hostname == \"$HOSTNAME_A\")" >/dev/null \
        || fail "tunnel svc-alpha lost hostname $HOSTNAME_A"
    pass "Tunnel svc-alpha has both hostnames"

    # Verify 2 LBs exist.
    log "Verifying 2 load balancers exist..."
    retry 60 3 bash -c "cfgwctl lb list-hostnames --zone-id '$ZONE_ID' | jq -e '.hostnames[] | select(. == \"$HOSTNAME_B\")' >/dev/null" \
        || fail "LB for $HOSTNAME_B not found"
    cfgwctl lb list-hostnames --zone-id "$ZONE_ID" | jq -e ".hostnames[] | select(. == \"$HOSTNAME_A\")" >/dev/null \
        || fail "LB for $HOSTNAME_A disappeared"
    pass "Both load balancers exist"
}

test_ts_route_deletion() {
    log "Deleting first HTTPRoute (ts-route-a with svc-alpha + svc-beta)..."
    kubectl delete httproute ts-route-a -n "$TEST_NS"

    # svc-beta is no longer referenced by any route → tunnel + Deployment should be deleted.
    log "Verifying svc-beta tunnel deleted..."
    retry 60 3 bash -c "
        id=\$(cfgwctl tunnel get-id --name '$TUNNEL_NAME_BETA' | jq -r '.tunnelId')
        [ -z \"\$id\" ] || [ \"\$id\" = 'null' ]
    " || fail "tunnel svc-beta still exists"
    pass "Tunnel svc-beta deleted"

    log "Verifying Deployment svc-beta deleted..."
    retry 60 3 bash -c "! kubectl get deployment cloudflared-ts-gw-svc-beta -n '$TEST_NS' 2>/dev/null" \
        || fail "Deployment svc-beta still exists"
    pass "Deployment svc-beta deleted"

    # svc-alpha is still referenced by ts-route-b → should still exist.
    log "Verifying svc-alpha tunnel still exists..."
    local tunnel_id_alpha
    tunnel_id_alpha=$(cfgwctl tunnel get-id --name "$TUNNEL_NAME_ALPHA" | jq -r '.tunnelId')
    [ -n "$tunnel_id_alpha" ] && [ "$tunnel_id_alpha" != "null" ] || fail "tunnel svc-alpha disappeared"
    pass "Tunnel svc-alpha still exists"

    # svc-alpha tunnel should now only have hostname B.
    # Use retry because tunnel ingress reconfiguration runs after stale tunnel cleanup.
    log "Verifying svc-alpha tunnel has only hostname B..."
    retry 60 3 bash -c "
        ! cfgwctl tunnel get-config --tunnel-id '$tunnel_id_alpha' \
            | jq -e '.[] | select(.hostname == \"$HOSTNAME_A\")' >/dev/null 2>&1
    " || fail "svc-alpha tunnel still has hostname $HOSTNAME_A"
    cfgwctl tunnel get-config --tunnel-id "$tunnel_id_alpha" \
        | jq -e ".[] | select(.hostname == \"$HOSTNAME_B\")" >/dev/null \
        || fail "svc-alpha tunnel lost hostname $HOSTNAME_B"
    pass "Tunnel svc-alpha has correct hostnames"

    # Verify first LB deleted, second still exists.
    log "Verifying LB for hostname A deleted..."
    retry 60 3 bash -c "! cfgwctl lb list-hostnames --zone-id '$ZONE_ID' | jq -e '.hostnames[] | select(. == \"$HOSTNAME_A\")' >/dev/null 2>&1" \
        || fail "LB for $HOSTNAME_A still exists"
    pass "LB for hostname A deleted"

    log "Verifying LB for hostname B still exists..."
    cfgwctl lb list-hostnames --zone-id "$ZONE_ID" | jq -e ".hostnames[] | select(. == \"$HOSTNAME_B\")" >/dev/null \
        || fail "LB for $HOSTNAME_B disappeared"
    pass "LB for hostname B still exists"

    # Verify pool svc-beta deleted.
    log "Verifying pool svc-beta deleted..."
    local pool_beta_id
    pool_beta_id=$(cfgwctl lb get-pool --name "$POOL_NAME_BETA" | jq -r '.poolId')
    { [ -z "$pool_beta_id" ] || [ "$pool_beta_id" = "" ]; } || fail "pool svc-beta still exists"
    pass "Pool svc-beta deleted"

    # Cleanup remaining route for gateway deletion test.
    kubectl delete httproute ts-route-b -n "$TEST_NS"
    # Wait for LB cleanup.
    retry 60 3 bash -c "! cfgwctl lb list-hostnames --zone-id '$ZONE_ID' | jq -e '.hostnames[] | select(. == \"$HOSTNAME_B\")' >/dev/null 2>&1" \
        || fail "LB for $HOSTNAME_B still exists after route deletion"
}

test_ts_gateway_deletion() {
    log "Deleting Gateway 'ts-gw'..."
    kubectl delete gateway ts-gw -n "$TEST_NS"

    log "Waiting for Gateway to be fully deleted..."
    retry 60 3 bash -c "! kubectl get gateway ts-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "ts-gw still exists"
    pass "Gateway deleted"

    # Verify all LBs deleted.
    log "Verifying all load balancers deleted..."
    local lb_count
    lb_count=$(cfgwctl lb list-hostnames --zone-id "$ZONE_ID" | jq '.hostnames | length')
    [ "$lb_count" -eq 0 ] || fail "expected 0 load balancers, got $lb_count"
    pass "All load balancers deleted"

    # Verify all pools deleted.
    log "Verifying all pools deleted..."
    local pool_count
    pool_count=$(cfgwctl lb list-pools --prefix 'gw-' | jq 'length')
    [ "$pool_count" -eq 0 ] || fail "expected 0 pools, got $pool_count"
    pass "All pools deleted"

    # Verify monitor deleted.
    log "Verifying monitor deleted..."
    local monitor_id
    monitor_id=$(cfgwctl lb get-monitor --name "$MONITOR_NAME" | jq -r '.monitorId')
    [ -z "$monitor_id" ] || [ "$monitor_id" = "" ] || fail "monitor still exists: $monitor_id"
    pass "Monitor deleted"

    # Verify all tunnels deleted.
    log "Verifying all tunnels deleted..."
    local tunnel_alpha tunnel_gamma
    tunnel_alpha=$(cfgwctl tunnel get-id --name "$TUNNEL_NAME_ALPHA" | jq -r '.tunnelId')
    tunnel_gamma=$(cfgwctl tunnel get-id --name "$TUNNEL_NAME_GAMMA" | jq -r '.tunnelId')
    { [ -z "$tunnel_alpha" ] || [ "$tunnel_alpha" = "null" ]; } || fail "tunnel svc-alpha still exists"
    { [ -z "$tunnel_gamma" ] || [ "$tunnel_gamma" = "null" ]; } || fail "tunnel svc-gamma still exists"
    pass "All tunnels deleted"

    # Verify Deployments deleted.
    log "Verifying Deployments deleted..."
    ! kubectl get deployment cloudflared-ts-gw-svc-alpha -n "$TEST_NS" 2>/dev/null \
        || fail "Deployment svc-alpha still exists"
    ! kubectl get deployment cloudflared-ts-gw-svc-gamma -n "$TEST_NS" 2>/dev/null \
        || fail "Deployment svc-gamma still exists"
    pass "All Deployments deleted"

    # Cleanup.
    kubectl delete cloudflaregatewayparameters ts-params -n "$TEST_NS" --ignore-not-found
    kubectl delete service svc-alpha svc-beta svc-gamma -n "$TEST_NS" --ignore-not-found
}

# ─── Run ──────────────────────────────────────────────────────────────────────

run_tests \
    test_ts_basic \
    test_ts_multi_hostname \
    test_ts_route_deletion \
    test_ts_gateway_deletion
