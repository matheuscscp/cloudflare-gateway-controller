#!/usr/bin/env bash
# Copyright 2026 Matheus Pimenta.
# SPDX-License-Identifier: AGPL-3.0
#
# End-to-end tests for the TrafficSplitting load balancer topology with AZs.
# Creates a multi-node kind cluster with labeled workers to simulate AZs.
# Each unique Service in an HTTPRoute backendRef gets its own pool, with one
# origin per AZ (one tunnel per Service x AZ). Pool weights are derived from
# HTTPRoute backendRef weights.
#
# Required environment variables:
#   TEST_ZONE_NAME  — Cloudflare DNS zone name for testing
#
# Optional: IMAGE, CREDENTIALS_FILE, CFGWCTL, REUSE_CLUSTER,
#           REUSE_CONTROLLER, RELOAD_CONTROLLER, TEST

set -euo pipefail

# Defaults.
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cfgw-e2e-ts-az}"
TEST_NS="${TEST_NS:-cfgw-e2e-ts-az}"

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
GW_NAME="tsaz-gw"
MONITOR_NAME=""
ZONE_ID=""
HOSTNAME_A=""
HOSTNAME_B=""

# ─── Test functions ───────────────────────────────────────────────────────────

test_tsaz_basic() {
    log "Creating CloudflareGatewayParameters with TrafficSplitting topology + AZs..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: tsaz-params
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
    topology: TrafficSplitting
    steeringPolicy: Random
    monitor:
      type: HTTPS
      path: /healthz
EOF

    log "Creating Gateway 'tsaz-gw'..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: tsaz-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: tsaz-params
  listeners:
  - name: http
    protocol: HTTP
    port: 80
EOF

    log "Waiting for Gateway to be Programmed..."
    retry 120 2 kubectl wait gateway/tsaz-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "tsaz-gw did not become Programmed"
    pass "tsaz-gw is Programmed"

    MONITOR_NAME=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "$GW_NAME" "monitor")

    HOSTNAME_A="tsaz-a-${TS: -6}.${TEST_ZONE_NAME}"

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
  name: tsaz-route-a
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: tsaz-gw
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

    # Verify 4 tunnels (2 services x 2 AZs).
    local tunnel_ids=()
    for svc in svc-alpha svc-beta; do
        for az in az-a az-b; do
            local tunnel_name
            tunnel_name=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "$GW_NAME" "$TEST_NS" "$svc" "$az")
            log "Verifying tunnel '${svc}-${az}'..."
            retry 60 3 bash -c "
                id=\$(cfgwctl tunnel get-id --name '$tunnel_name' | jq -r '.tunnelId')
                [ -n \"\$id\" ] && [ \"\$id\" != 'null' ]
            " || fail "tunnel ${svc}-${az} not found"
            local tid
            tid=$(cfgwctl tunnel get-id --name "$tunnel_name" | jq -r '.tunnelId')
            tunnel_ids+=("$tid")
            pass "Tunnel ${svc}-${az} exists: $tid"
        done
    done

    # Verify 4 Deployments with correct node affinity.
    log "Verifying 4 cloudflared Deployments..."
    for svc in svc-alpha svc-beta; do
        for az in az-a az-b; do
            local deploy_name="cloudflared-tsaz-gw-${svc}-${az}"
            retry 60 3 kubectl get deployment "$deploy_name" -n "$TEST_NS" \
                || fail "Deployment $deploy_name not found"

            local actual_zone
            actual_zone=$(kubectl get deployment "$deploy_name" -n "$TEST_NS" \
                -o jsonpath='{.spec.template.spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values[0]}')
            [ "$actual_zone" = "$az" ] || fail "$deploy_name has wrong zone affinity: $actual_zone, want $az"
            pass "Deployment $deploy_name has correct node affinity ($az)"
        done
    done

    # Verify each tunnel config routes to its own service only.
    log "Verifying tunnel ingress rules are service-specific..."
    local tn_alpha_a tn_beta_a
    tn_alpha_a=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "$GW_NAME" "$TEST_NS" "svc-alpha" "az-a")
    tn_beta_a=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "$GW_NAME" "$TEST_NS" "svc-beta" "az-a")
    local tunnel_id_alpha_a tunnel_id_beta_a
    tunnel_id_alpha_a=$(cfgwctl tunnel get-id --name "$tn_alpha_a" | jq -r '.tunnelId')
    tunnel_id_beta_a=$(cfgwctl tunnel get-id --name "$tn_beta_a" | jq -r '.tunnelId')

    retry 60 3 bash -c "cfgwctl tunnel get-config --tunnel-id '$tunnel_id_alpha_a' | jq -e '.[] | select(.hostname == \"$HOSTNAME_A\")' >/dev/null" \
        || fail "tunnel svc-alpha-az-a missing hostname $HOSTNAME_A"
    local alpha_service
    alpha_service=$(cfgwctl tunnel get-config --tunnel-id "$tunnel_id_alpha_a" | jq -r ".[] | select(.hostname == \"$HOSTNAME_A\") | .service")
    echo "$alpha_service" | grep -q "svc-alpha" || fail "svc-alpha tunnel routes to wrong service: $alpha_service"
    pass "Tunnel svc-alpha-az-a routes to svc-alpha"

    retry 60 3 bash -c "cfgwctl tunnel get-config --tunnel-id '$tunnel_id_beta_a' | jq -e '.[] | select(.hostname == \"$HOSTNAME_A\")' >/dev/null" \
        || fail "tunnel svc-beta-az-a missing hostname $HOSTNAME_A"
    local beta_service
    beta_service=$(cfgwctl tunnel get-config --tunnel-id "$tunnel_id_beta_a" | jq -r ".[] | select(.hostname == \"$HOSTNAME_A\") | .service")
    echo "$beta_service" | grep -q "svc-beta" || fail "svc-beta tunnel routes to wrong service: $beta_service"
    pass "Tunnel svc-beta-az-a routes to svc-beta"

    # Verify monitor exists.
    log "Verifying monitor exists..."
    retry 60 3 bash -c "
        id=\$(cfgwctl lb get-monitor --name '$MONITOR_NAME' | jq -r '.monitorId')
        [ -n \"\$id\" ] && [ \"\$id\" != '' ]
    " || fail "monitor not found"
    local monitor_id
    monitor_id=$(cfgwctl lb get-monitor --name "$MONITOR_NAME" | jq -r '.monitorId')
    pass "Monitor exists: $monitor_id"

    # Verify 2 pools (one per service), each with 2 origins (one per AZ).
    log "Verifying pools exist..."
    retry 60 3 bash -c "[ \$(cfgwctl lb list-pools --prefix 'gw-' | jq 'length') -eq 2 ]" \
        || fail "expected 2 pools"
    pass "2 pools exist"

    # Verify pool svc-alpha has 2 origins.
    local pn_alpha pn_beta
    pn_alpha=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "$GW_NAME" "$TEST_NS" "svc-alpha")
    pn_beta=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "$GW_NAME" "$TEST_NS" "svc-beta")
    log "Verifying pool svc-alpha has 2 origins..."
    retry 60 3 bash -c "[ \$(cfgwctl lb get-pool --name '$pn_alpha' | jq '.origins | length') -eq 2 ]" \
        || fail "expected 2 origins in pool svc-alpha"
    pass "Pool svc-alpha has 2 origins"

    # Verify pool svc-beta has 2 origins.
    log "Verifying pool svc-beta has 2 origins..."
    retry 60 3 bash -c "[ \$(cfgwctl lb get-pool --name '$pn_beta' | jq '.origins | length') -eq 2 ]" \
        || fail "expected 2 origins in pool svc-beta"
    pass "Pool svc-beta has 2 origins"

    # Verify pool origins reference the correct tunnels.
    log "Verifying pool svc-alpha origins reference correct tunnels..."
    local alpha_az_a_tid alpha_az_b_tid
    alpha_az_a_tid=$(cfgwctl tunnel get-id --name "$tn_alpha_a" | jq -r '.tunnelId')
    local tn_alpha_b
    tn_alpha_b=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "$GW_NAME" "$TEST_NS" "svc-alpha" "az-b")
    alpha_az_b_tid=$(cfgwctl tunnel get-id --name "$tn_alpha_b" | jq -r '.tunnelId')

    retry 60 3 bash -c "cfgwctl lb get-pool --name '$pn_alpha' \
        | jq -e '.origins[] | select(.address == \"${alpha_az_a_tid}.cfargotunnel.com\")' >/dev/null" \
        || fail "pool svc-alpha missing origin for az-a"
    retry 60 3 bash -c "cfgwctl lb get-pool --name '$pn_alpha' \
        | jq -e '.origins[] | select(.address == \"${alpha_az_b_tid}.cfargotunnel.com\")' >/dev/null" \
        || fail "pool svc-alpha missing origin for az-b"
    pass "Pool svc-alpha origins correct"

    # Look up zone ID for later tests.
    ZONE_ID=$(cfgwctl dns find-zone --hostname "test.${TEST_ZONE_NAME}" | jq -r '.zoneId')
    [ -n "$ZONE_ID" ] && [ "$ZONE_ID" != "null" ] || fail "zone ID not found"

    # Verify LB exists for the hostname.
    log "Verifying load balancer for '$HOSTNAME_A'..."
    retry 60 3 bash -c "cfgwctl lb list-hostnames --zone-id '$ZONE_ID' | jq -e '.hostnames[] | select(. == \"$HOSTNAME_A\")' >/dev/null" \
        || fail "LB for $HOSTNAME_A not found"
    pass "Load balancer exists for $HOSTNAME_A"
}

test_tsaz_multi_hostname() {
    HOSTNAME_B="tsaz-b-${TS: -6}.${TEST_ZONE_NAME}"

    log "Creating second HTTPRoute with hostname '$HOSTNAME_B' referencing svc-alpha only..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: tsaz-route-b
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: tsaz-gw
  hostnames:
  - "$HOSTNAME_B"
  rules:
  - backendRefs:
    - name: svc-alpha
      port: 80
EOF

    # No new tunnels/pools needed — svc-alpha already has tunnels and a pool.
    # Verify svc-alpha tunnel az-a has both hostnames.
    local tn_alpha_a
    tn_alpha_a=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "$GW_NAME" "$TEST_NS" "svc-alpha" "az-a")
    local tunnel_id_alpha_a
    tunnel_id_alpha_a=$(cfgwctl tunnel get-id --name "$tn_alpha_a" | jq -r '.tunnelId')
    log "Verifying svc-alpha-az-a tunnel has both hostnames..."
    retry 60 3 bash -c "cfgwctl tunnel get-config --tunnel-id '$tunnel_id_alpha_a' | jq -e '.[] | select(.hostname == \"$HOSTNAME_B\")' >/dev/null" \
        || fail "tunnel svc-alpha-az-a missing hostname $HOSTNAME_B"
    cfgwctl tunnel get-config --tunnel-id "$tunnel_id_alpha_a" | jq -e ".[] | select(.hostname == \"$HOSTNAME_A\")" >/dev/null \
        || fail "tunnel svc-alpha-az-a lost hostname $HOSTNAME_A"
    pass "Tunnel svc-alpha-az-a has both hostnames"

    # Still 2 pools (no new service was added).
    log "Verifying still 2 pools..."
    local pool_count
    pool_count=$(cfgwctl lb list-pools --prefix 'gw-' | jq 'length')
    [ "$pool_count" -eq 2 ] || fail "expected 2 pools, got $pool_count"
    pass "2 pools still exist"

    # Verify 2 LBs exist (one per hostname).
    log "Verifying 2 load balancers exist..."
    retry 60 3 bash -c "cfgwctl lb list-hostnames --zone-id '$ZONE_ID' | jq -e '.hostnames[] | select(. == \"$HOSTNAME_B\")' >/dev/null" \
        || fail "LB for $HOSTNAME_B not found"
    cfgwctl lb list-hostnames --zone-id "$ZONE_ID" | jq -e ".hostnames[] | select(. == \"$HOSTNAME_A\")" >/dev/null \
        || fail "LB for $HOSTNAME_A disappeared"
    pass "Both load balancers exist"
}

test_tsaz_route_deletion() {
    log "Deleting first HTTPRoute (tsaz-route-a with svc-alpha + svc-beta)..."
    kubectl delete httproute tsaz-route-a -n "$TEST_NS"

    # svc-beta is no longer referenced → all its tunnels + Deployments should be deleted.
    log "Verifying svc-beta tunnels deleted..."
    for az in az-a az-b; do
        local tn_beta_az
        tn_beta_az=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "$GW_NAME" "$TEST_NS" "svc-beta" "$az")
        retry 60 3 bash -c "
            id=\$(cfgwctl tunnel get-id --name '$tn_beta_az' | jq -r '.tunnelId')
            [ -z \"\$id\" ] || [ \"\$id\" = 'null' ]
        " || fail "tunnel svc-beta-${az} still exists"
        pass "Tunnel svc-beta-${az} deleted"
    done

    log "Verifying Deployments for svc-beta deleted..."
    for az in az-a az-b; do
        retry 60 3 bash -c "! kubectl get deployment cloudflared-tsaz-gw-svc-beta-${az} -n '$TEST_NS' 2>/dev/null" \
            || fail "Deployment svc-beta-${az} still exists"
        pass "Deployment svc-beta-${az} deleted"
    done

    # svc-alpha tunnels should still exist (still referenced by route-b).
    log "Verifying svc-alpha tunnels still exist..."
    for az in az-a az-b; do
        local tn_alpha_az
        tn_alpha_az=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "$GW_NAME" "$TEST_NS" "svc-alpha" "$az")
        local tid
        tid=$(cfgwctl tunnel get-id --name "$tn_alpha_az" | jq -r '.tunnelId')
        [ -n "$tid" ] && [ "$tid" != "null" ] || fail "tunnel svc-alpha-${az} disappeared"
    done
    pass "All svc-alpha tunnels still exist"

    # Verify svc-alpha tunnel only has hostname B now.
    local tn_alpha_a
    tn_alpha_a=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "$GW_NAME" "$TEST_NS" "svc-alpha" "az-a")
    local tunnel_id_alpha_a
    tunnel_id_alpha_a=$(cfgwctl tunnel get-id --name "$tn_alpha_a" | jq -r '.tunnelId')
    log "Verifying svc-alpha-az-a tunnel has only hostname B..."
    retry 60 3 bash -c "! cfgwctl tunnel get-config --tunnel-id '$tunnel_id_alpha_a' | jq -e '.[] | select(.hostname == \"$HOSTNAME_A\")' >/dev/null 2>&1" \
        || fail "tunnel svc-alpha-az-a still has hostname $HOSTNAME_A"
    cfgwctl tunnel get-config --tunnel-id "$tunnel_id_alpha_a" | jq -e ".[] | select(.hostname == \"$HOSTNAME_B\")" >/dev/null \
        || fail "tunnel svc-alpha-az-a lost hostname $HOSTNAME_B"
    pass "Tunnel svc-alpha-az-a has correct hostnames"

    # Verify LB for hostname A deleted, hostname B still exists.
    log "Verifying LB for hostname A deleted..."
    retry 60 3 bash -c "! cfgwctl lb list-hostnames --zone-id '$ZONE_ID' | jq -e '.hostnames[] | select(. == \"$HOSTNAME_A\")' >/dev/null 2>&1" \
        || fail "LB for $HOSTNAME_A still exists"
    pass "LB for hostname A deleted"

    log "Verifying LB for hostname B still exists..."
    cfgwctl lb list-hostnames --zone-id "$ZONE_ID" | jq -e ".hostnames[] | select(. == \"$HOSTNAME_B\")" >/dev/null \
        || fail "LB for $HOSTNAME_B disappeared"
    pass "LB for hostname B still exists"

    # Verify pool svc-beta deleted, pool svc-alpha still exists.
    log "Verifying pool svc-beta deleted..."
    local pn_beta
    pn_beta=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "$GW_NAME" "$TEST_NS" "svc-beta")
    local pool_beta_id
    pool_beta_id=$(cfgwctl lb get-pool --name "$pn_beta" | jq -r '.poolId')
    { [ -z "$pool_beta_id" ] || [ "$pool_beta_id" = "" ]; } || fail "pool svc-beta still exists"
    pass "Pool svc-beta deleted"

    # Cleanup remaining route for gateway deletion test.
    kubectl delete httproute tsaz-route-b -n "$TEST_NS"
    # Wait for LB cleanup.
    retry 60 3 bash -c "! cfgwctl lb list-hostnames --zone-id '$ZONE_ID' | jq -e '.hostnames[] | select(. == \"$HOSTNAME_B\")' >/dev/null 2>&1" \
        || fail "LB for $HOSTNAME_B still exists after route deletion"
}

test_tsaz_gateway_deletion() {
    log "Deleting Gateway 'tsaz-gw'..."
    kubectl delete gateway tsaz-gw -n "$TEST_NS"

    log "Waiting for Gateway to be fully deleted..."
    retry 60 3 bash -c "! kubectl get gateway tsaz-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "tsaz-gw still exists"
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
    for svc in svc-alpha; do
        for az in az-a az-b; do
            local tn
            tn=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "$GW_NAME" "$TEST_NS" "$svc" "$az")
            local tid
            tid=$(cfgwctl tunnel get-id --name "$tn" | jq -r '.tunnelId')
            { [ -z "$tid" ] || [ "$tid" = "null" ]; } || fail "tunnel ${svc}-${az} still exists"
        done
    done
    pass "All tunnels deleted"

    # Verify all Deployments deleted.
    log "Verifying all Deployments deleted..."
    for svc in svc-alpha; do
        for az in az-a az-b; do
            ! kubectl get deployment "cloudflared-tsaz-gw-${svc}-${az}" -n "$TEST_NS" 2>/dev/null \
                || fail "Deployment ${svc}-${az} still exists"
        done
    done
    pass "All Deployments deleted"

    # Cleanup.
    kubectl delete cloudflaregatewayparameters tsaz-params -n "$TEST_NS" --ignore-not-found
    kubectl delete service svc-alpha svc-beta -n "$TEST_NS" --ignore-not-found
}

# ─── Run ──────────────────────────────────────────────────────────────────────

run_tests \
    test_tsaz_basic \
    test_tsaz_multi_hostname \
    test_tsaz_route_deletion \
    test_tsaz_gateway_deletion
