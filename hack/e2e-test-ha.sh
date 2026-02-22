#!/usr/bin/env bash
# Copyright 2026 Matheus Pimenta.
# SPDX-License-Identifier: AGPL-3.0
#
# End-to-end tests for the HighAvailability load balancer topology.
# Creates a multi-node kind cluster with labeled workers to simulate AZs,
# then validates monitor, pool, and load balancer resources in Cloudflare.
#
# Required environment variables:
#   TEST_ZONE_NAME  — Cloudflare DNS zone name for testing
#
# Optional: IMAGE, CREDENTIALS_FILE, CFGWCTL, REUSE_CLUSTER,
#           REUSE_CONTROLLER, RELOAD_CONTROLLER, TEST

set -euo pipefail

# Defaults.
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cfgw-e2e-ha}"
TEST_NS="${TEST_NS:-cfgw-e2e-ha}"

# Multi-node cluster config for AZ simulation.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KIND_CONFIG="${KIND_CONFIG:-$SCRIPT_DIR/kind-multi-node.yaml}"

# Source shared library.
source "$SCRIPT_DIR/e2e-lib.sh"

validate_prerequisites
setup_cluster
register_cleanup
install_controller
start_controller_log_stream
create_test_namespace
ensure_gatewayclass

# ─── Shared state across tests ───────────────────────────────────────────────
# These are populated by test_ha_basic and used by subsequent tests.
GW_UID=""
TUNNEL_PREFIX=""
ZONE_ID=""
HOSTNAME_A=""
HOSTNAME_B=""

# ─── Test functions ───────────────────────────────────────────────────────────

test_ha_basic() {
    log "Creating CloudflareGatewayParameters with HA topology..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: ha-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zone:
      name: "$TEST_ZONE_NAME"
  tunnels:
    availabilityZones:
    - name: az-a
      zone: az-a
    - name: az-b
      zone: az-b
  loadBalancer:
    topology: HighAvailability
    steeringPolicy: Geographic
    monitor:
      type: HTTPS
      path: /healthz
EOF

    log "Creating Gateway 'ha-gw'..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: ha-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: ha-params
  listeners:
  - name: http
    protocol: HTTP
    port: 80
EOF

    log "Waiting for Gateway to be Programmed..."
    retry 120 2 kubectl wait gateway/ha-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "ha-gw did not become Programmed"
    pass "ha-gw is Programmed"

    GW_UID=$(kubectl get gateway ha-gw -n "$TEST_NS" -o jsonpath='{.metadata.uid}')
    TUNNEL_PREFIX="gateway-${GW_UID}"

    # Verify 2 tunnels (one per AZ).
    local tunnel_id_a tunnel_id_b
    log "Verifying tunnel for az-a..."
    tunnel_id_a=$(cfgwctl tunnel get-id --name "${TUNNEL_PREFIX}-az-a" | jq -r '.tunnelId')
    [ -n "$tunnel_id_a" ] && [ "$tunnel_id_a" != "null" ] || fail "tunnel az-a not found"
    pass "Tunnel az-a exists: $tunnel_id_a"

    log "Verifying tunnel for az-b..."
    tunnel_id_b=$(cfgwctl tunnel get-id --name "${TUNNEL_PREFIX}-az-b" | jq -r '.tunnelId')
    [ -n "$tunnel_id_b" ] && [ "$tunnel_id_b" != "null" ] || fail "tunnel az-b not found"
    pass "Tunnel az-b exists: $tunnel_id_b"

    # Verify 2 cloudflared Deployments with correct node affinity.
    log "Verifying cloudflared Deployments..."
    local az_a_zone az_b_zone
    az_a_zone=$(kubectl get deployment cloudflared-ha-gw-az-a -n "$TEST_NS" \
        -o jsonpath='{.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values[0]}')
    [ "$az_a_zone" = "az-a" ] || fail "cloudflared-ha-gw-az-a has wrong zone affinity: $az_a_zone"
    pass "Deployment az-a has correct node affinity"

    az_b_zone=$(kubectl get deployment cloudflared-ha-gw-az-b -n "$TEST_NS" \
        -o jsonpath='{.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values[0]}')
    [ "$az_b_zone" = "az-b" ] || fail "cloudflared-ha-gw-az-b has wrong zone affinity: $az_b_zone"
    pass "Deployment az-b has correct node affinity"

    # Verify monitor exists.
    log "Verifying monitor exists..."
    retry 60 3 bash -c "
        id=\$(cfgwctl lb get-monitor --name '$TUNNEL_PREFIX' | jq -r '.monitorId')
        [ -n \"\$id\" ] && [ \"\$id\" != '' ]
    " || fail "monitor not found"
    local monitor_id
    monitor_id=$(cfgwctl lb get-monitor --name "$TUNNEL_PREFIX" | jq -r '.monitorId')
    pass "Monitor exists: $monitor_id"

    # Verify 2 pools.
    log "Verifying pools exist..."
    retry 60 3 bash -c "[ \$(cfgwctl lb list-pools --prefix '$TUNNEL_PREFIX' | jq 'length') -eq 2 ]" \
        || fail "expected 2 pools"
    pass "2 pools exist"

    # Verify pool origins point to correct tunnels.
    log "Verifying pool az-a origin..."
    retry 60 3 bash -c "
        origin=\$(cfgwctl lb get-pool --name '${TUNNEL_PREFIX}-az-a' | jq -r '.origins[0].address')
        [ \"\$origin\" = '${tunnel_id_a}.cfargotunnel.com' ]
    " || fail "pool az-a origin mismatch"
    pass "Pool az-a origin correct"

    log "Verifying pool az-b origin..."
    retry 60 3 bash -c "
        origin=\$(cfgwctl lb get-pool --name '${TUNNEL_PREFIX}-az-b' | jq -r '.origins[0].address')
        [ \"\$origin\" = '${tunnel_id_b}.cfargotunnel.com' ]
    " || fail "pool az-b origin mismatch"
    pass "Pool az-b origin correct"

    # Look up zone ID for later tests.
    ZONE_ID=$(cfgwctl dns find-zone --hostname "test.${TEST_ZONE_NAME}" | jq -r '.zoneId')
    [ -n "$ZONE_ID" ] && [ "$ZONE_ID" != "null" ] || fail "zone ID not found"
}

test_ha_httproute() {
    HOSTNAME_A="ha-a-${TS: -6}.${TEST_ZONE_NAME}"

    log "Creating Service and HTTPRoute with hostname '$HOSTNAME_A'..."
    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: ha-backend-a
  namespace: $TEST_NS
spec:
  ports:
  - port: 80
    protocol: TCP
EOF

    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: ha-route-a
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: ha-gw
  hostnames:
  - "$HOSTNAME_A"
  rules:
  - backendRefs:
    - name: ha-backend-a
      port: 80
EOF

    # Verify both tunnel configs have the hostname (HA: full ingress on every tunnel).
    local tunnel_id_a tunnel_id_b
    tunnel_id_a=$(cfgwctl tunnel get-id --name "${TUNNEL_PREFIX}-az-a" | jq -r '.tunnelId')
    tunnel_id_b=$(cfgwctl tunnel get-id --name "${TUNNEL_PREFIX}-az-b" | jq -r '.tunnelId')

    log "Verifying tunnel az-a has hostname..."
    retry 60 3 bash -c "cfgwctl tunnel get-config --tunnel-id '$tunnel_id_a' | jq -e '.[] | select(.hostname == \"$HOSTNAME_A\")' >/dev/null" \
        || fail "tunnel az-a missing hostname $HOSTNAME_A"
    pass "Tunnel az-a has hostname"

    log "Verifying tunnel az-b has hostname..."
    retry 60 3 bash -c "cfgwctl tunnel get-config --tunnel-id '$tunnel_id_b' | jq -e '.[] | select(.hostname == \"$HOSTNAME_A\")' >/dev/null" \
        || fail "tunnel az-b missing hostname $HOSTNAME_A"
    pass "Tunnel az-b has hostname"

    # Verify LB exists.
    log "Verifying load balancer for '$HOSTNAME_A'..."
    retry 60 3 bash -c "cfgwctl lb list-hostnames --zone-id '$ZONE_ID' | jq -e '.hostnames[] | select(. == \"$HOSTNAME_A\")' >/dev/null" \
        || fail "LB for $HOSTNAME_A not found"
    pass "Load balancer exists for $HOSTNAME_A"
}

test_ha_multi_hostname() {
    HOSTNAME_B="ha-b-${TS: -6}.${TEST_ZONE_NAME}"

    log "Creating second Service and HTTPRoute with hostname '$HOSTNAME_B'..."
    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: ha-backend-b
  namespace: $TEST_NS
spec:
  ports:
  - port: 80
    protocol: TCP
EOF

    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: ha-route-b
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: ha-gw
  hostnames:
  - "$HOSTNAME_B"
  rules:
  - backendRefs:
    - name: ha-backend-b
      port: 80
EOF

    # Verify both tunnels have both hostnames.
    local tunnel_id_a tunnel_id_b
    tunnel_id_a=$(cfgwctl tunnel get-id --name "${TUNNEL_PREFIX}-az-a" | jq -r '.tunnelId')
    tunnel_id_b=$(cfgwctl tunnel get-id --name "${TUNNEL_PREFIX}-az-b" | jq -r '.tunnelId')

    log "Verifying tunnels have both hostnames..."
    retry 60 3 bash -c "cfgwctl tunnel get-config --tunnel-id '$tunnel_id_a' | jq -e '.[] | select(.hostname == \"$HOSTNAME_B\")' >/dev/null" \
        || fail "tunnel az-a missing hostname $HOSTNAME_B"
    retry 60 3 bash -c "cfgwctl tunnel get-config --tunnel-id '$tunnel_id_b' | jq -e '.[] | select(.hostname == \"$HOSTNAME_B\")' >/dev/null" \
        || fail "tunnel az-b missing hostname $HOSTNAME_B"
    pass "Both tunnels have both hostnames"

    # Verify 2 LBs exist.
    log "Verifying 2 load balancers exist..."
    retry 60 3 bash -c "cfgwctl lb list-hostnames --zone-id '$ZONE_ID' | jq -e '.hostnames[] | select(. == \"$HOSTNAME_B\")' >/dev/null" \
        || fail "LB for $HOSTNAME_B not found"
    cfgwctl lb list-hostnames --zone-id "$ZONE_ID" | jq -e ".hostnames[] | select(. == \"$HOSTNAME_A\")" >/dev/null \
        || fail "LB for $HOSTNAME_A disappeared"
    pass "Both load balancers exist"
}

test_ha_route_deletion() {
    log "Deleting first HTTPRoute (ha-route-a)..."
    kubectl delete httproute ha-route-a -n "$TEST_NS"

    local tunnel_id_a tunnel_id_b
    tunnel_id_a=$(cfgwctl tunnel get-id --name "${TUNNEL_PREFIX}-az-a" | jq -r '.tunnelId')
    tunnel_id_b=$(cfgwctl tunnel get-id --name "${TUNNEL_PREFIX}-az-b" | jq -r '.tunnelId')

    log "Verifying hostname A removed from tunnel configs..."
    retry 60 3 bash -c "! cfgwctl tunnel get-config --tunnel-id '$tunnel_id_a' | jq -e '.[] | select(.hostname == \"$HOSTNAME_A\")' >/dev/null 2>&1" \
        || fail "tunnel az-a still has hostname $HOSTNAME_A"
    retry 60 3 bash -c "! cfgwctl tunnel get-config --tunnel-id '$tunnel_id_b' | jq -e '.[] | select(.hostname == \"$HOSTNAME_A\")' >/dev/null 2>&1" \
        || fail "tunnel az-b still has hostname $HOSTNAME_A"
    pass "Hostname A removed from tunnels"

    log "Verifying LB for hostname A deleted..."
    retry 60 3 bash -c "! cfgwctl lb list-hostnames --zone-id '$ZONE_ID' | jq -e '.hostnames[] | select(. == \"$HOSTNAME_A\")' >/dev/null 2>&1" \
        || fail "LB for $HOSTNAME_A still exists"
    pass "LB for hostname A deleted"

    log "Verifying LB for hostname B still exists..."
    cfgwctl lb list-hostnames --zone-id "$ZONE_ID" | jq -e ".hostnames[] | select(. == \"$HOSTNAME_B\")" >/dev/null \
        || fail "LB for $HOSTNAME_B disappeared"
    pass "LB for hostname B still exists"

    # Cleanup remaining route for gateway deletion test.
    kubectl delete httproute ha-route-b -n "$TEST_NS"
    # Wait for LB cleanup.
    retry 60 3 bash -c "! cfgwctl lb list-hostnames --zone-id '$ZONE_ID' | jq -e '.hostnames[] | select(. == \"$HOSTNAME_B\")' >/dev/null 2>&1" \
        || fail "LB for $HOSTNAME_B still exists after route deletion"
}

test_ha_gateway_deletion() {
    log "Deleting Gateway 'ha-gw'..."
    kubectl delete gateway ha-gw -n "$TEST_NS"

    log "Waiting for Gateway to be fully deleted..."
    retry 60 3 bash -c "! kubectl get gateway ha-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "ha-gw still exists"
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
    pool_count=$(cfgwctl lb list-pools --prefix "$TUNNEL_PREFIX" | jq 'length')
    [ "$pool_count" -eq 0 ] || fail "expected 0 pools, got $pool_count"
    pass "All pools deleted"

    # Verify monitor deleted.
    log "Verifying monitor deleted..."
    local monitor_id
    monitor_id=$(cfgwctl lb get-monitor --name "$TUNNEL_PREFIX" | jq -r '.monitorId')
    [ -z "$monitor_id" ] || [ "$monitor_id" = "" ] || fail "monitor still exists: $monitor_id"
    pass "Monitor deleted"

    # Verify all tunnels deleted.
    log "Verifying all tunnels deleted..."
    local tunnel_a tunnel_b
    tunnel_a=$(cfgwctl tunnel get-id --name "${TUNNEL_PREFIX}-az-a" | jq -r '.tunnelId')
    tunnel_b=$(cfgwctl tunnel get-id --name "${TUNNEL_PREFIX}-az-b" | jq -r '.tunnelId')
    { [ -z "$tunnel_a" ] || [ "$tunnel_a" = "null" ]; } || fail "tunnel az-a still exists"
    { [ -z "$tunnel_b" ] || [ "$tunnel_b" = "null" ]; } || fail "tunnel az-b still exists"
    pass "All tunnels deleted"

    # Verify Deployments deleted.
    log "Verifying Deployments deleted..."
    ! kubectl get deployment cloudflared-ha-gw-az-a -n "$TEST_NS" 2>/dev/null \
        || fail "Deployment az-a still exists"
    ! kubectl get deployment cloudflared-ha-gw-az-b -n "$TEST_NS" 2>/dev/null \
        || fail "Deployment az-b still exists"
    pass "All Deployments deleted"

    # Cleanup.
    kubectl delete cloudflaregatewayparameters ha-params -n "$TEST_NS" --ignore-not-found
    kubectl delete service ha-backend-a ha-backend-b -n "$TEST_NS" --ignore-not-found
}

# ─── Run ──────────────────────────────────────────────────────────────────────

run_tests \
    test_ha_basic \
    test_ha_httproute \
    test_ha_multi_hostname \
    test_ha_route_deletion \
    test_ha_gateway_deletion
