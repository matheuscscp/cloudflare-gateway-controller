#!/usr/bin/env bash
# Copyright 2026 Matheus Pimenta.
# SPDX-License-Identifier: AGPL-3.0
#
# End-to-end test script.
# Creates a kind cluster, installs the controller via Helm, and validates
# the full reconciliation loop against a real Cloudflare account.
#
# Required environment variables:
#   TEST_ZONE_NAME  — Cloudflare DNS zone name for testing (e.g. example.com)
#
# Optional environment variables:
#   IMAGE              — controller image (default: cloudflare-gateway-controller:dev)
#   CREDENTIALS_FILE   — path to credentials file (default: ./api.token)
#   KIND_CLUSTER_NAME  — kind cluster name (default: cfgw-e2e)
#   CFGWCTL            — path to cfgwctl binary (default: ./bin/cfgwctl)
#   REUSE_CLUSTER      — if "1", reuse existing kind cluster
#   REUSE_CONTROLLER   — if "1", skip controller install if already running
#   RELOAD_CONTROLLER  — if "1", reload image and restart controller
#   TEST               — if set, run only the named test function
#   TEST_ZONE_NAME_2          — second DNS zone for multi-zone tests (optional; tests skipped if unset)
#   TEST_ZONE_NAME_3          — third DNS zone for multi-zone tests (optional; tests skipped if unset)
#   TEST_TRAFFIC_ZONE_NAME    — zone for traffic tests needing TLS (default: cloudflare-gateway-controller.dev)

set -euo pipefail

# Defaults.
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cfgw-e2e}"
TEST_NS="${TEST_NS:-cfgw-e2e-test}"
TEST_ZONE_NAME="${TEST_ZONE_NAME:-dev.cloudflare-gateway-controller.dev}"
# Traffic tests need TLS-capable hostnames (*.zone). Cloudflare free Universal SSL
# only covers single-level wildcards, so hostnames must be direct subdomains of
# the Cloudflare zone (e.g. lb-123.cloudflare-gateway-controller.dev, NOT
# lb-123.dev.cloudflare-gateway-controller.dev which is 4 levels).
TEST_TRAFFIC_ZONE_NAME="${TEST_TRAFFIC_ZONE_NAME:-cloudflare-gateway-controller.dev}"

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

# ─── Test functions ───────────────────────────────────────────────────────────

test_gateway_lifecycle() {
    local test_hostname="e2e-${TS: -6}.${TEST_ZONE_NAME}"

    log "Creating CloudflareGatewayParameters 'test-params'..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: test-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones:
    - name: "$TEST_ZONE_NAME"
EOF

    log "Creating Gateway 'test-gateway'..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: test-gateway
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: test-params
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    log "Waiting for Gateway to be Programmed..."
    retry 60 2 kubectl wait gateway/test-gateway -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "Gateway did not become Programmed"
    pass "Gateway is Programmed"

    # Get deterministic tunnel name.
    local tunnel_name tunnel_id
    tunnel_name=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "test-gateway")

    log "Verifying tunnel '$tunnel_name' exists..."
    tunnel_id=$(cfgwctl tunnel get-id --name "$tunnel_name" | jq -r '.tunnelId')
    [ -n "$tunnel_id" ] && [ "$tunnel_id" != "null" ] || fail "tunnel not found"
    pass "Tunnel exists"

    # --- HTTPRoute lifecycle ---
    log "Creating test Service..."
    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: test-backend
  namespace: $TEST_NS
spec:
  ports:
  - port: 80
    protocol: TCP
EOF

    log "Creating HTTPRoute with hostname '$test_hostname'..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: test-route
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: test-gateway
  hostnames:
  - "$test_hostname"
  rules:
  - backendRefs:
    - name: test-backend
      port: 80
EOF

    log "Waiting for route config to include '$test_hostname'..."
    retry 60 3 bash -c "route_config_has_hostname 'test-gateway' '$test_hostname'" \
        || fail "route config does not contain hostname"
    pass "Route config has hostname"

    log "Finding zone ID for '$TEST_ZONE_NAME'..."
    local zone_id tunnel_target
    zone_id=$(cfgwctl dns find-zone --hostname "$test_hostname" | jq -r '.zoneId')
    [ -n "$zone_id" ] && [ "$zone_id" != "null" ] || fail "zone ID not found"
    tunnel_target="${tunnel_id}.cfargotunnel.com"

    log "Verifying DNS CNAME for '$test_hostname'..."
    check_dns_cname() {
        cfgwctl dns list-cnames --zone-id "$zone_id" --target "$tunnel_target" \
            | jq -e ".hostnames[] | select(. == \"$test_hostname\")" >/dev/null
    }
    retry 60 3 check_dns_cname || fail "DNS CNAME not found"
    pass "DNS CNAME exists"

    # --- HTTPRoute deletion ---
    log "Deleting HTTPRoute 'test-route'..."
    kubectl delete httproute test-route -n "$TEST_NS"

    log "Waiting for route config to remove '$test_hostname'..."
    retry 60 3 bash -c "! route_config_has_hostname 'test-gateway' '$test_hostname'" \
        || fail "route config still contains hostname"
    pass "Route config updated after route deletion"

    log "Verifying DNS CNAME removed for '$test_hostname'..."
    check_dns_cname_removed() {
        ! cfgwctl dns list-cnames --zone-id "$zone_id" --target "$tunnel_target" \
            | jq -e ".hostnames[] | select(. == \"$test_hostname\")" >/dev/null 2>&1
    }
    retry 60 3 check_dns_cname_removed || fail "DNS CNAME still exists"
    pass "DNS CNAME removed"

    # --- Gateway deletion ---
    log "Deleting Gateway 'test-gateway'..."
    kubectl delete gateway test-gateway -n "$TEST_NS"

    log "Waiting for Gateway to be fully deleted..."
    retry 60 3 bash -c "! kubectl get gateway test-gateway -n '$TEST_NS' 2>/dev/null" \
        || fail "Gateway still exists"
    pass "Gateway deleted"

    log "Verifying tunnel deleted..."
    check_tunnel_deleted() {
        local id
        id=$(cfgwctl tunnel get-id --name "$tunnel_name" | jq -r '.tunnelId')
        [ -z "$id" ] || [ "$id" = "null" ]
    }
    retry 60 3 check_tunnel_deleted || fail "tunnel still exists"
    pass "Tunnel deleted"

    # Cleanup test resources.
    kubectl delete cloudflaregatewayparameters test-params -n "$TEST_NS" --ignore-not-found
    kubectl delete service test-backend -n "$TEST_NS" --ignore-not-found
}

test_multi_routes() {
    local hostname_a="a-${TS: -6}.${TEST_ZONE_NAME}"
    local hostname_b="b-${TS: -6}.${TEST_ZONE_NAME}"

    log "Creating CloudflareGatewayParameters 'multi-route-params'..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: multi-route-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones:
    - name: "$TEST_ZONE_NAME"
EOF

    log "Creating Gateway 'multi-route-gw'..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: multi-route-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: multi-route-params
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    retry 60 2 kubectl wait gateway/multi-route-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "multi-route-gw did not become Programmed"
    pass "multi-route-gw is Programmed"

    local mr_tunnel_name mr_tunnel_id mr_tunnel_target
    mr_tunnel_name=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "multi-route-gw")
    mr_tunnel_id=$(cfgwctl tunnel get-id --name "$mr_tunnel_name" | jq -r '.tunnelId')
    [ -n "$mr_tunnel_id" ] && [ "$mr_tunnel_id" != "null" ] || fail "multi-route tunnel not found"
    mr_tunnel_target="${mr_tunnel_id}.cfargotunnel.com"

    log "Creating two Services and two HTTPRoutes..."
    for svc in backend-a backend-b; do
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

    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: route-a
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: multi-route-gw
  hostnames:
  - "$hostname_a"
  rules:
  - backendRefs:
    - name: backend-a
      port: 80
EOF

    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: route-b
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: multi-route-gw
  hostnames:
  - "$hostname_b"
  rules:
  - backendRefs:
    - name: backend-b
      port: 80
EOF

    log "Verifying route config has both hostnames..."
    retry 60 3 bash -c "route_config_has_hostname 'multi-route-gw' '$hostname_a'" \
        || fail "route config missing hostname A"
    retry 60 3 bash -c "route_config_has_hostname 'multi-route-gw' '$hostname_b'" \
        || fail "route config missing hostname B"
    pass "Route config has both hostnames"

    local mr_zone_id
    mr_zone_id=$(cfgwctl dns find-zone --hostname "$hostname_a" | jq -r '.zoneId')

    log "Verifying both DNS CNAMEs exist..."
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$mr_zone_id' --target '$mr_tunnel_target' | jq -e '.hostnames[] | select(. == \"$hostname_a\")' >/dev/null" \
        || fail "DNS CNAME for hostname A not found"
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$mr_zone_id' --target '$mr_tunnel_target' | jq -e '.hostnames[] | select(. == \"$hostname_b\")' >/dev/null" \
        || fail "DNS CNAME for hostname B not found"
    pass "Both DNS CNAMEs exist"

    log "Deleting route-a only..."
    kubectl delete httproute route-a -n "$TEST_NS"

    log "Verifying hostname A removed but hostname B still present..."
    retry 60 3 bash -c "! route_config_has_hostname 'multi-route-gw' '$hostname_a'" \
        || fail "route config still has hostname A"
    route_config_has_hostname "multi-route-gw" "$hostname_b" \
        || fail "route config lost hostname B"
    pass "Route config correctly updated after partial route deletion"

    log "Verifying DNS CNAME A removed but B still exists..."
    retry 60 3 bash -c "! cfgwctl dns list-cnames --zone-id '$mr_zone_id' --target '$mr_tunnel_target' | jq -e '.hostnames[] | select(. == \"$hostname_a\")' >/dev/null 2>&1" \
        || fail "DNS CNAME for A still exists"
    cfgwctl dns list-cnames --zone-id "$mr_zone_id" --target "$mr_tunnel_target" | jq -e ".hostnames[] | select(. == \"$hostname_b\")" >/dev/null \
        || fail "DNS CNAME for B disappeared"
    pass "DNS CNAMEs correctly updated"

    log "Cleaning up multi-route test..."
    kubectl delete httproute route-b -n "$TEST_NS"
    kubectl delete gateway multi-route-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway multi-route-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "multi-route-gw still exists"
    check_mr_tunnel_deleted() {
        local id
        id=$(cfgwctl tunnel get-id --name "$mr_tunnel_name" | jq -r '.tunnelId')
        [ -z "$id" ] || [ "$id" = "null" ]
    }
    retry 60 3 check_mr_tunnel_deleted || fail "multi-route tunnel still exists"
    kubectl delete cloudflaregatewayparameters multi-route-params -n "$TEST_NS" --ignore-not-found
    kubectl delete service backend-a backend-b -n "$TEST_NS" --ignore-not-found
}

test_dns_default_all_zones() {
    local dns_def_hostname="dd-${TS: -6}.${TEST_ZONE_NAME}"

    log "Creating Gateway 'dns-def-gw' without CloudflareGatewayParameters..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: dns-def-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    retry 60 2 kubectl wait gateway/dns-def-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "dns-def-gw did not become Programmed"
    pass "dns-def-gw is Programmed"

    log "Verifying DNSManagement=True with 'All hostnames'..."
    check_dns_all_hostnames() {
        local msg
        msg=$(kubectl get gateway dns-def-gw -n "$TEST_NS" -o jsonpath='{.status.conditions[?(@.type=="DNSManagement")].message}')
        [ "$msg" = "All hostnames" ]
    }
    retry 30 2 check_dns_all_hostnames || fail "DNSManagement condition not 'All hostnames'"
    pass "DNSManagement=True/Enabled with 'All hostnames'"

    local dd_tunnel_name dd_tunnel_id dd_tunnel_target
    dd_tunnel_name=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "dns-def-gw")
    dd_tunnel_id=$(cfgwctl tunnel get-id --name "$dd_tunnel_name" | jq -r '.tunnelId')
    [ -n "$dd_tunnel_id" ] && [ "$dd_tunnel_id" != "null" ] || fail "dns-def tunnel not found"
    dd_tunnel_target="${dd_tunnel_id}.cfargotunnel.com"

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: dns-def-backend
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
  name: dns-def-route
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: dns-def-gw
  hostnames:
  - "$dns_def_hostname"
  rules:
  - backendRefs:
    - name: dns-def-backend
      port: 80
EOF

    log "Verifying DNS CNAME created for '$dns_def_hostname' (all-zones mode)..."
    local dd_zone_id
    dd_zone_id=$(cfgwctl dns find-zone --hostname "$dns_def_hostname" | jq -r '.zoneId')
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$dd_zone_id' --target '$dd_tunnel_target' | jq -e '.hostnames[] | select(. == \"$dns_def_hostname\")' >/dev/null" \
        || fail "DNS CNAME not created in all-zones mode"
    pass "DNS CNAME created in all-zones mode"

    log "Verifying HTTPRoute DNSRecordsApplied condition has no 'Skipped' section..."
    check_no_skipped() {
        local msg
        msg=$(kubectl get httproute dns-def-route -n "$TEST_NS" \
            -o jsonpath='{.status.parents[0].conditions[?(@.type=="DNSRecordsApplied")].message}')
        [ -n "$msg" ] && echo "$msg" | grep -q "Applied hostnames" && ! echo "$msg" | grep -q "Skipped"
    }
    retry 30 2 check_no_skipped || fail "DNSRecordsApplied message has unexpected 'Skipped' section"
    pass "HTTPRoute DNSRecordsApplied shows all hostnames applied (no skipped)"

    log "Cleaning up dns-default-all-zones test..."
    kubectl delete httproute dns-def-route -n "$TEST_NS"
    kubectl delete gateway dns-def-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway dns-def-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "dns-def-gw still exists"
    kubectl delete service dns-def-backend -n "$TEST_NS" --ignore-not-found
}

test_no_dns() {
    local no_dns_hostname="nd-${TS: -6}.${TEST_ZONE_NAME}"

    log "Creating CloudflareGatewayParameters 'no-dns-params' with DNS disabled (empty zones)..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: no-dns-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones: []
EOF

    log "Creating Gateway 'no-dns-gw' with DNS disabled..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: no-dns-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: no-dns-params
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    retry 60 2 kubectl wait gateway/no-dns-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "no-dns-gw did not become Programmed"
    pass "no-dns-gw is Programmed"

    local nd_tunnel_name nd_tunnel_id
    nd_tunnel_name=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "no-dns-gw")
    nd_tunnel_id=$(cfgwctl tunnel get-id --name "$nd_tunnel_name" | jq -r '.tunnelId')
    [ -n "$nd_tunnel_id" ] && [ "$nd_tunnel_id" != "null" ] || fail "no-dns tunnel not found"

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: no-dns-backend
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
  name: no-dns-route
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: no-dns-gw
  hostnames:
  - "$no_dns_hostname"
  rules:
  - backendRefs:
    - name: no-dns-backend
      port: 80
EOF

    log "Verifying route config has hostname..."
    retry 60 3 bash -c "route_config_has_hostname 'no-dns-gw' '$no_dns_hostname'" \
        || fail "no-dns route config missing hostname"
    pass "Route config has hostname"

    log "Verifying NO DNS CNAME was created..."
    local nd_zone_id nd_tunnel_target
    nd_zone_id=$(cfgwctl dns find-zone --hostname "$no_dns_hostname" | jq -r '.zoneId')
    nd_tunnel_target="${nd_tunnel_id}.cfargotunnel.com"
    ! cfgwctl dns list-cnames --zone-id "$nd_zone_id" --target "$nd_tunnel_target" \
        | jq -e ".hostnames[] | select(. == \"$no_dns_hostname\")" >/dev/null 2>&1 \
        || fail "DNS CNAME exists when it should not"
    sleep 5
    ! cfgwctl dns list-cnames --zone-id "$nd_zone_id" --target "$nd_tunnel_target" \
        | jq -e ".hostnames[] | select(. == \"$no_dns_hostname\")" >/dev/null 2>&1 \
        || fail "DNS CNAME appeared when it should not"
    pass "No DNS CNAME created (as expected)"

    log "Cleaning up no-DNS test..."
    kubectl delete httproute no-dns-route -n "$TEST_NS"
    kubectl delete gateway no-dns-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway no-dns-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "no-dns-gw still exists"
    kubectl delete cloudflaregatewayparameters no-dns-params -n "$TEST_NS" --ignore-not-found
    kubectl delete service no-dns-backend -n "$TEST_NS" --ignore-not-found
}

test_disabled_reconciliation() {
    local disabled_hostname="dis-${TS: -6}.${TEST_ZONE_NAME}"

    log "Creating CloudflareGatewayParameters 'disabled-params'..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: disabled-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones:
    - name: "$TEST_ZONE_NAME"
EOF

    log "Creating Gateway 'disabled-gw'..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: disabled-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: disabled-params
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    retry 60 2 kubectl wait gateway/disabled-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "disabled-gw did not become Programmed"
    pass "disabled-gw is Programmed"

    local dis_tunnel_name dis_tunnel_id dis_tunnel_target
    dis_tunnel_name=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "disabled-gw")
    dis_tunnel_id=$(cfgwctl tunnel get-id --name "$dis_tunnel_name" | jq -r '.tunnelId')
    [ -n "$dis_tunnel_id" ] && [ "$dis_tunnel_id" != "null" ] || fail "disabled tunnel not found"
    dis_tunnel_target="${dis_tunnel_id}.cfargotunnel.com"

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: disabled-backend
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
  name: disabled-route
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: disabled-gw
  hostnames:
  - "$disabled_hostname"
  rules:
  - backendRefs:
    - name: disabled-backend
      port: 80
EOF

    log "Waiting for DNS CNAME for disabled-gw..."
    local dis_zone_id
    dis_zone_id=$(cfgwctl dns find-zone --hostname "$disabled_hostname" | jq -r '.zoneId')
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$dis_zone_id' --target '$dis_tunnel_target' | jq -e '.hostnames[] | select(. == \"$disabled_hostname\")' >/dev/null" \
        || fail "DNS CNAME for disabled-gw not found"
    pass "DNS CNAME exists for disabled-gw"

    log "Setting reconcile=disabled and deleting Gateway..."
    kubectl annotate gateway disabled-gw -n "$TEST_NS" \
        cloudflare-gateway-controller.io/reconcile=disabled --overwrite

    kubectl delete httproute disabled-route -n "$TEST_NS"
    kubectl delete gateway disabled-gw -n "$TEST_NS"

    log "Waiting for Gateway to be fully deleted..."
    retry 60 3 bash -c "! kubectl get gateway disabled-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "disabled-gw still exists"
    pass "disabled-gw deleted"

    log "Verifying gateway Deployment still exists (orphaned)..."
    kubectl get deployment gateway-disabled-gw-primary -n "$TEST_NS" >/dev/null 2>&1 \
        || fail "gateway Deployment was deleted (should be orphaned)"
    pass "Gateway Deployment is orphaned"

    log "Verifying tunnel still exists in Cloudflare..."
    local dis_tunnel_check
    dis_tunnel_check=$(cfgwctl tunnel get-id --name "$dis_tunnel_name" | jq -r '.tunnelId')
    [ "$dis_tunnel_check" = "$dis_tunnel_id" ] || fail "tunnel was deleted (should still exist)"
    pass "Tunnel still exists"

    log "Verifying DNS CNAME still exists..."
    cfgwctl dns list-cnames --zone-id "$dis_zone_id" --target "$dis_tunnel_target" \
        | jq -e ".hostnames[] | select(. == \"$disabled_hostname\")" >/dev/null \
        || fail "DNS CNAME was deleted (should still exist)"
    pass "DNS CNAME still exists"

    log "Manual cleanup of orphaned resources..."
    kubectl delete deployment gateway-disabled-gw-primary -n "$TEST_NS" --ignore-not-found
    retry 60 3 bash -c "! kubectl get deployment gateway-disabled-gw-primary -n '$TEST_NS' 2>/dev/null"
    kubectl delete secret gateway-disabled-gw -n "$TEST_NS" --ignore-not-found
    cfgwctl dns delete-cname --zone-id "$dis_zone_id" --hostname "$disabled_hostname"
    cfgwctl tunnel cleanup-connections --tunnel-id "$dis_tunnel_id"
    cfgwctl tunnel delete --tunnel-id "$dis_tunnel_id"
    kubectl delete cloudflaregatewayparameters disabled-params -n "$TEST_NS" --ignore-not-found
    kubectl delete service disabled-backend -n "$TEST_NS" --ignore-not-found
}

test_dns_config_removal() {
    local zone_rm_hostname="zrm-${TS: -6}.${TEST_ZONE_NAME}"

    log "Creating CloudflareGatewayParameters 'zone-rm-params'..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: zone-rm-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones:
    - name: "$TEST_ZONE_NAME"
EOF

    log "Creating Gateway 'zone-rm-gw'..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: zone-rm-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: zone-rm-params
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    retry 60 2 kubectl wait gateway/zone-rm-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "zone-rm-gw did not become Programmed"
    pass "zone-rm-gw is Programmed"

    local zr_tunnel_name zr_tunnel_id zr_tunnel_target
    zr_tunnel_name=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "zone-rm-gw")
    zr_tunnel_id=$(cfgwctl tunnel get-id --name "$zr_tunnel_name" | jq -r '.tunnelId')
    [ -n "$zr_tunnel_id" ] && [ "$zr_tunnel_id" != "null" ] || fail "zone-rm tunnel not found"
    zr_tunnel_target="${zr_tunnel_id}.cfargotunnel.com"

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: zone-rm-backend
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
  name: zone-rm-route
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: zone-rm-gw
  hostnames:
  - "$zone_rm_hostname"
  rules:
  - backendRefs:
    - name: zone-rm-backend
      port: 80
EOF

    log "Waiting for DNS CNAME..."
    local zr_zone_id
    zr_zone_id=$(cfgwctl dns find-zone --hostname "$zone_rm_hostname" | jq -r '.zoneId')
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$zr_zone_id' --target '$zr_tunnel_target' | jq -e '.hostnames[] | select(. == \"$zone_rm_hostname\")' >/dev/null" \
        || fail "DNS CNAME for zone-rm not found"
    pass "DNS CNAME exists"

    log "Disabling DNS by setting empty zones list..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: zone-rm-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones: []
EOF

    log "Verifying DNS CNAME removed..."
    retry 60 3 bash -c "! cfgwctl dns list-cnames --zone-id '$zr_zone_id' --target '$zr_tunnel_target' | jq -e '.hostnames[] | select(. == \"$zone_rm_hostname\")' >/dev/null 2>&1" \
        || fail "DNS CNAME still exists after DNS disabled"
    pass "DNS CNAME removed after DNS disabled"

    log "Verifying route config still has hostname (route config unaffected)..."
    route_config_has_hostname "zone-rm-gw" "$zone_rm_hostname" \
        || fail "route config lost hostname after DNS config removal"
    pass "Route config still has hostname"

    log "Cleaning up DNS config removal test..."
    kubectl delete httproute zone-rm-route -n "$TEST_NS"
    kubectl delete gateway zone-rm-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway zone-rm-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "zone-rm-gw still exists"
    kubectl delete cloudflaregatewayparameters zone-rm-params -n "$TEST_NS" --ignore-not-found
    kubectl delete service zone-rm-backend -n "$TEST_NS" --ignore-not-found
}

test_cluster_recreation() {
    local test_hostname="rec-${TS: -6}.${TEST_ZONE_NAME}"

    log "Phase 1: Create Gateway + HTTPRoute in current cluster..."

    log "Creating CloudflareGatewayParameters 'recreate-params'..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: recreate-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones:
    - name: "$TEST_ZONE_NAME"
EOF

    log "Creating Gateway 'recreate-gw'..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: recreate-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: recreate-params
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    retry 60 2 kubectl wait gateway/recreate-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "recreate-gw did not become Programmed"
    pass "recreate-gw is Programmed"

    local tunnel_name tunnel_id_before
    tunnel_name=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "recreate-gw")

    log "Verifying tunnel '$tunnel_name' exists..."
    tunnel_id_before=$(cfgwctl tunnel get-id --name "$tunnel_name" | jq -r '.tunnelId')
    [ -n "$tunnel_id_before" ] && [ "$tunnel_id_before" != "null" ] || fail "tunnel not found"
    pass "Tunnel exists: $tunnel_id_before"

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: recreate-backend
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
  name: recreate-route
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: recreate-gw
  hostnames:
  - "$test_hostname"
  rules:
  - backendRefs:
    - name: recreate-backend
      port: 80
EOF

    log "Waiting for DNS CNAME for '$test_hostname'..."
    local zone_id tunnel_target
    zone_id=$(cfgwctl dns find-zone --hostname "$test_hostname" | jq -r '.zoneId')
    [ -n "$zone_id" ] && [ "$zone_id" != "null" ] || fail "zone ID not found"
    tunnel_target="${tunnel_id_before}.cfargotunnel.com"

    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$zone_id' --target '$tunnel_target' | jq -e '.hostnames[] | select(. == \"$test_hostname\")' >/dev/null" \
        || fail "DNS CNAME not found"
    pass "DNS CNAME exists before cluster recreation"

    log "Phase 2: Delete and recreate kind cluster..."

    # Stop log stream before cluster deletion.
    if [ -n "$_LOG_STREAM_PID" ]; then
        kill "$_LOG_STREAM_PID" 2>/dev/null || true
        wait "$_LOG_STREAM_PID" 2>/dev/null || true
        _LOG_STREAM_PID=""
    fi

    kind delete cluster --name "$KIND_CLUSTER_NAME"
    _CREATED_CLUSTER=0

    # Force fresh cluster and controller install regardless of env vars.
    local saved_reuse_cluster="${REUSE_CLUSTER:-}"
    local saved_reuse_controller="${REUSE_CONTROLLER:-}"
    local saved_reload_controller="${RELOAD_CONTROLLER:-}"
    REUSE_CLUSTER=""
    REUSE_CONTROLLER=""
    RELOAD_CONTROLLER=""

    setup_cluster
    install_controller
    start_controller_log_stream
    create_test_namespace
    ensure_gatewayclass

    # Restore env vars.
    REUSE_CLUSTER="$saved_reuse_cluster"
    REUSE_CONTROLLER="$saved_reuse_controller"
    RELOAD_CONTROLLER="$saved_reload_controller"

    log "Phase 3: Recreate same Gateway + HTTPRoute..."

    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: recreate-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones:
    - name: "$TEST_ZONE_NAME"
EOF

    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: recreate-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: recreate-params
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    retry 60 2 kubectl wait gateway/recreate-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "recreate-gw did not become Programmed after cluster recreation"
    pass "recreate-gw is Programmed after cluster recreation"

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: recreate-backend
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
  name: recreate-route
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: recreate-gw
  hostnames:
  - "$test_hostname"
  rules:
  - backendRefs:
    - name: recreate-backend
      port: 80
EOF

    log "Phase 4: Verify tunnel adoption..."

    local tunnel_id_after
    tunnel_id_after=$(cfgwctl tunnel get-id --name "$tunnel_name" | jq -r '.tunnelId')
    [ -n "$tunnel_id_after" ] && [ "$tunnel_id_after" != "null" ] || fail "tunnel not found after recreation"
    [ "$tunnel_id_before" = "$tunnel_id_after" ] \
        || fail "tunnel ID changed: before=$tunnel_id_before after=$tunnel_id_after (resource leaked!)"
    pass "Tunnel adopted: same ID $tunnel_id_before"

    log "Verifying route config has hostname after adoption..."
    retry 60 3 bash -c "route_config_has_hostname 'recreate-gw' '$test_hostname'" \
        || fail "route config missing hostname after adoption"
    pass "Route config has hostname after adoption"

    log "Verifying DNS CNAME still exists after adoption..."
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$zone_id' --target '$tunnel_target' | jq -e '.hostnames[] | select(. == \"$test_hostname\")' >/dev/null" \
        || fail "DNS CNAME not found after adoption"
    pass "DNS CNAME exists after adoption"

    log "Phase 5: Clean up via normal Gateway finalization..."

    kubectl delete httproute recreate-route -n "$TEST_NS"
    kubectl delete gateway recreate-gw -n "$TEST_NS"

    retry 60 3 bash -c "! kubectl get gateway recreate-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "recreate-gw still exists"
    pass "Gateway deleted"

    log "Verifying tunnel deleted by finalizer..."
    check_tunnel_deleted() {
        local id
        id=$(cfgwctl tunnel get-id --name "$tunnel_name" | jq -r '.tunnelId')
        [ -z "$id" ] || [ "$id" = "null" ]
    }
    retry 60 3 check_tunnel_deleted || fail "tunnel still exists after finalization"
    pass "Tunnel deleted by finalizer"

    log "Verifying DNS CNAME deleted..."
    check_cname_deleted() {
        ! cfgwctl dns list-cnames --zone-id "$zone_id" --target "$tunnel_target" \
            | jq -e ".hostnames[] | select(. == \"$test_hostname\")" >/dev/null 2>&1
    }
    retry 60 3 check_cname_deleted || fail "DNS CNAME still exists after finalization"
    pass "DNS CNAME deleted by finalizer"

    kubectl delete cloudflaregatewayparameters recreate-params -n "$TEST_NS" --ignore-not-found
    kubectl delete service recreate-backend -n "$TEST_NS" --ignore-not-found
}

test_multi_zone_dns() {
    if [ -z "${TEST_ZONE_NAME_2:-}" ] || [ -z "${TEST_ZONE_NAME_3:-}" ]; then
        log "Skipping test_multi_zone_dns: TEST_ZONE_NAME_2 and TEST_ZONE_NAME_3 are required"
        return
    fi

    # Hostnames in each of the 3 zones + one hostname matching no zone.
    local hostname_z1="mz1-${TS: -6}.${TEST_ZONE_NAME}"
    local hostname_z2="mz2-${TS: -6}.${TEST_ZONE_NAME_2}"
    local hostname_z3="mz3-${TS: -6}.${TEST_ZONE_NAME_3}"
    local hostname_z1b="mz1b-${TS: -6}.${TEST_ZONE_NAME}"
    local hostname_none="mz-${TS: -6}.unmanaged.example"

    # --- Phase 1: Start with zones 1 and 2 ---
    log "Creating CloudflareGatewayParameters 'multi-zone-params' with zones 1 and 2..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: multi-zone-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones:
    - name: "$TEST_ZONE_NAME"
    - name: "$TEST_ZONE_NAME_2"
EOF

    log "Creating Gateway 'multi-zone-gw'..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: multi-zone-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: multi-zone-params
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    retry 60 2 kubectl wait gateway/multi-zone-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "multi-zone-gw did not become Programmed"
    pass "multi-zone-gw is Programmed"

    local mz_tunnel_name mz_tunnel_id mz_tunnel_target
    mz_tunnel_name=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "multi-zone-gw")
    mz_tunnel_id=$(cfgwctl tunnel get-id --name "$mz_tunnel_name" | jq -r '.tunnelId')
    [ -n "$mz_tunnel_id" ] && [ "$mz_tunnel_id" != "null" ] || fail "multi-zone tunnel not found"
    mz_tunnel_target="${mz_tunnel_id}.cfargotunnel.com"

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: multi-zone-backend
  namespace: $TEST_NS
spec:
  ports:
  - port: 80
    protocol: TCP
EOF

    log "Creating HTTPRoute with hostnames from zones 1, 2, 3, and an unmanaged domain..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: multi-zone-route
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: multi-zone-gw
  hostnames:
  - "$hostname_z1"
  - "$hostname_z1b"
  - "$hostname_z2"
  - "$hostname_z3"
  - "$hostname_none"
  rules:
  - backendRefs:
    - name: multi-zone-backend
      port: 80
EOF

    # Resolve zone IDs.
    local zone1_id zone2_id zone3_id
    zone1_id=$(cfgwctl dns find-zone --hostname "$hostname_z1" | jq -r '.zoneId')
    [ -n "$zone1_id" ] && [ "$zone1_id" != "null" ] || fail "zone 1 ID not found"
    zone2_id=$(cfgwctl dns find-zone --hostname "$hostname_z2" | jq -r '.zoneId')
    [ -n "$zone2_id" ] && [ "$zone2_id" != "null" ] || fail "zone 2 ID not found"
    zone3_id=$(cfgwctl dns find-zone --hostname "$hostname_z3" | jq -r '.zoneId')
    [ -n "$zone3_id" ] && [ "$zone3_id" != "null" ] || fail "zone 3 ID not found"

    # Verify zone 1 CNAMEs created (both hostnames).
    log "Waiting for DNS CNAMEs in zone 1..."
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$zone1_id' --target '$mz_tunnel_target' | jq -e '.hostnames[] | select(. == \"$hostname_z1\")' >/dev/null" \
        || fail "DNS CNAME for $hostname_z1 not found"
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$zone1_id' --target '$mz_tunnel_target' | jq -e '.hostnames[] | select(. == \"$hostname_z1b\")' >/dev/null" \
        || fail "DNS CNAME for $hostname_z1b not found"
    pass "Zone 1 CNAMEs exist (2 hostnames)"

    # Verify zone 2 CNAME created.
    log "Waiting for DNS CNAME in zone 2..."
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$zone2_id' --target '$mz_tunnel_target' | jq -e '.hostnames[] | select(. == \"$hostname_z2\")' >/dev/null" \
        || fail "DNS CNAME for $hostname_z2 not found"
    pass "Zone 2 CNAME exists"

    # Verify zone 3 hostname was NOT created (zone 3 not configured yet).
    log "Verifying zone 3 hostname NOT created (zone 3 not in config)..."
    ! cfgwctl dns list-cnames --zone-id "$zone3_id" --target "$mz_tunnel_target" \
        | jq -e ".hostnames[] | select(. == \"$hostname_z3\")" >/dev/null 2>&1 \
        || fail "DNS CNAME for $hostname_z3 should not exist yet"
    pass "Zone 3 CNAME correctly absent"

    # Verify DNSRecordsApplied condition reports skipped hostnames.
    log "Verifying HTTPRoute DNS condition reports skipped hostnames..."
    retry 30 2 bash -c "
        kubectl get httproute multi-zone-route -n '$TEST_NS' -o json \
            | jq -e '.status.parents[0].conditions[] | select(.type == \"DNSRecordsApplied\") | select(.message | contains(\"not in any configured zone\"))' >/dev/null
    " || fail "DNSRecordsApplied condition does not mention skipped hostnames"
    pass "DNS condition reports skipped hostnames"

    # --- Phase 2: Add zone 3, remove zone 1 ---
    log "Updating DNS config: remove zone 1, add zone 3..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: multi-zone-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones:
    - name: "$TEST_ZONE_NAME_2"
    - name: "$TEST_ZONE_NAME_3"
EOF

    # Verify zone 1 CNAMEs cleaned up.
    log "Verifying zone 1 CNAMEs removed after zone removal..."
    retry 60 3 bash -c "! cfgwctl dns list-cnames --zone-id '$zone1_id' --target '$mz_tunnel_target' | jq -e '.hostnames[] | select(. == \"$hostname_z1\")' >/dev/null 2>&1" \
        || fail "CNAME for $hostname_z1 still exists after zone 1 removed"
    retry 60 3 bash -c "! cfgwctl dns list-cnames --zone-id '$zone1_id' --target '$mz_tunnel_target' | jq -e '.hostnames[] | select(. == \"$hostname_z1b\")' >/dev/null 2>&1" \
        || fail "CNAME for $hostname_z1b still exists after zone 1 removed"
    pass "Zone 1 CNAMEs cleaned up"

    # Verify zone 2 CNAME still present.
    log "Verifying zone 2 CNAME still intact..."
    cfgwctl dns list-cnames --zone-id "$zone2_id" --target "$mz_tunnel_target" \
        | jq -e ".hostnames[] | select(. == \"$hostname_z2\")" >/dev/null \
        || fail "Zone 2 CNAME was incorrectly removed"
    pass "Zone 2 CNAME unaffected"

    # Verify zone 3 CNAME now created.
    log "Waiting for zone 3 CNAME to be created..."
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$zone3_id' --target '$mz_tunnel_target' | jq -e '.hostnames[] | select(. == \"$hostname_z3\")' >/dev/null" \
        || fail "DNS CNAME for $hostname_z3 not found"
    pass "Zone 3 CNAME created after zone addition"

    # --- Phase 3: Add zone 1 back (now all 3 zones) ---
    log "Updating DNS config: add zone 1 back (all 3 zones)..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: multi-zone-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones:
    - name: "$TEST_ZONE_NAME"
    - name: "$TEST_ZONE_NAME_2"
    - name: "$TEST_ZONE_NAME_3"
EOF

    # Verify zone 1 CNAMEs re-created.
    log "Waiting for zone 1 CNAMEs to be re-created..."
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$zone1_id' --target '$mz_tunnel_target' | jq -e '.hostnames[] | select(. == \"$hostname_z1\")' >/dev/null" \
        || fail "CNAME for $hostname_z1 not re-created"
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$zone1_id' --target '$mz_tunnel_target' | jq -e '.hostnames[] | select(. == \"$hostname_z1b\")' >/dev/null" \
        || fail "CNAME for $hostname_z1b not re-created"
    pass "Zone 1 CNAMEs re-created"

    # Verify all other CNAMEs still intact.
    cfgwctl dns list-cnames --zone-id "$zone2_id" --target "$mz_tunnel_target" \
        | jq -e ".hostnames[] | select(. == \"$hostname_z2\")" >/dev/null \
        || fail "Zone 2 CNAME missing"
    cfgwctl dns list-cnames --zone-id "$zone3_id" --target "$mz_tunnel_target" \
        | jq -e ".hostnames[] | select(. == \"$hostname_z3\")" >/dev/null \
        || fail "Zone 3 CNAME missing"
    pass "All 3 zones have their CNAMEs"

    # Cleanup.
    log "Cleaning up multi-zone DNS test..."
    kubectl delete httproute multi-zone-route -n "$TEST_NS"
    kubectl delete gateway multi-zone-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway multi-zone-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "multi-zone-gw still exists"
    kubectl delete cloudflaregatewayparameters multi-zone-params -n "$TEST_NS" --ignore-not-found
    kubectl delete service multi-zone-backend -n "$TEST_NS" --ignore-not-found
}

test_multi_gateway_overlapping_zones() {
    if [ -z "${TEST_ZONE_NAME_2:-}" ] || [ -z "${TEST_ZONE_NAME_3:-}" ]; then
        log "Skipping test_multi_gateway_overlapping_zones: TEST_ZONE_NAME_2 and TEST_ZONE_NAME_3 are required"
        return
    fi

    # 3 Gateways with overlapping zone configurations:
    #   gw-a: zones 1, 2, 3 (all zones)
    #   gw-b: zones 1, 2    (subset)
    #   gw-c: zones 2, 3    (overlaps with both)
    # Each gateway has unique hostnames in each of its zones (no hostname overlap).

    local ha_z1="ova1-${TS: -6}.${TEST_ZONE_NAME}"
    local ha_z2="ova2-${TS: -6}.${TEST_ZONE_NAME_2}"
    local ha_z3="ova3-${TS: -6}.${TEST_ZONE_NAME_3}"
    local hb_z1="ovb1-${TS: -6}.${TEST_ZONE_NAME}"
    local hb_z2="ovb2-${TS: -6}.${TEST_ZONE_NAME_2}"
    local hc_z2="ovc2-${TS: -6}.${TEST_ZONE_NAME_2}"
    local hc_z3="ovc3-${TS: -6}.${TEST_ZONE_NAME_3}"

    log "Creating CloudflareGatewayParameters for 3 gateways with overlapping zones..."

    # gw-a: all 3 zones
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: overlap-params-a
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones:
    - name: "$TEST_ZONE_NAME"
    - name: "$TEST_ZONE_NAME_2"
    - name: "$TEST_ZONE_NAME_3"
EOF

    # gw-b: zones 1, 2
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: overlap-params-b
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones:
    - name: "$TEST_ZONE_NAME"
    - name: "$TEST_ZONE_NAME_2"
EOF

    # gw-c: zones 2, 3
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: overlap-params-c
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones:
    - name: "$TEST_ZONE_NAME_2"
    - name: "$TEST_ZONE_NAME_3"
EOF

    # Create all 3 Gateways.
    for gw_suffix in a b c; do
        kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: overlap-gw-${gw_suffix}
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: overlap-params-${gw_suffix}
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF
    done

    for gw_suffix in a b c; do
        retry 60 2 kubectl wait "gateway/overlap-gw-${gw_suffix}" -n "$TEST_NS" \
            --for=condition=Programmed --timeout=5s \
            || fail "overlap-gw-${gw_suffix} did not become Programmed"
    done
    pass "All 3 overlap gateways Programmed"

    # Resolve tunnel IDs and targets.
    local gwa_tn gwa_tid gwa_tt
    gwa_tn=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "overlap-gw-a")
    gwa_tid=$(cfgwctl tunnel get-id --name "$gwa_tn" | jq -r '.tunnelId')
    [ -n "$gwa_tid" ] && [ "$gwa_tid" != "null" ] || fail "gw-a tunnel not found"
    gwa_tt="${gwa_tid}.cfargotunnel.com"

    local gwb_tn gwb_tid gwb_tt
    gwb_tn=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "overlap-gw-b")
    gwb_tid=$(cfgwctl tunnel get-id --name "$gwb_tn" | jq -r '.tunnelId')
    [ -n "$gwb_tid" ] && [ "$gwb_tid" != "null" ] || fail "gw-b tunnel not found"
    gwb_tt="${gwb_tid}.cfargotunnel.com"

    local gwc_tn gwc_tid gwc_tt
    gwc_tn=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "overlap-gw-c")
    gwc_tid=$(cfgwctl tunnel get-id --name "$gwc_tn" | jq -r '.tunnelId')
    [ -n "$gwc_tid" ] && [ "$gwc_tid" != "null" ] || fail "gw-c tunnel not found"
    gwc_tt="${gwc_tid}.cfargotunnel.com"

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: overlap-backend
  namespace: $TEST_NS
spec:
  ports:
  - port: 80
    protocol: TCP
EOF

    # Create routes for each gateway.
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: overlap-route-a
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: overlap-gw-a
  hostnames:
  - "$ha_z1"
  - "$ha_z2"
  - "$ha_z3"
  rules:
  - backendRefs:
    - name: overlap-backend
      port: 80
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: overlap-route-b
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: overlap-gw-b
  hostnames:
  - "$hb_z1"
  - "$hb_z2"
  rules:
  - backendRefs:
    - name: overlap-backend
      port: 80
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: overlap-route-c
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: overlap-gw-c
  hostnames:
  - "$hc_z2"
  - "$hc_z3"
  rules:
  - backendRefs:
    - name: overlap-backend
      port: 80
EOF

    # Resolve zone IDs.
    local z1_id z2_id z3_id
    z1_id=$(cfgwctl dns find-zone --hostname "$ha_z1" | jq -r '.zoneId')
    z2_id=$(cfgwctl dns find-zone --hostname "$ha_z2" | jq -r '.zoneId')
    z3_id=$(cfgwctl dns find-zone --hostname "$ha_z3" | jq -r '.zoneId')

    # Verify all 7 DNS CNAMEs exist: gw-a has 3, gw-b has 2, gw-c has 2.
    log "Waiting for gw-a DNS CNAMEs (3 hostnames across 3 zones)..."
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$z1_id' --target '$gwa_tt' | jq -e '.hostnames[] | select(. == \"$ha_z1\")' >/dev/null" \
        || fail "gw-a zone1 CNAME not found"
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$z2_id' --target '$gwa_tt' | jq -e '.hostnames[] | select(. == \"$ha_z2\")' >/dev/null" \
        || fail "gw-a zone2 CNAME not found"
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$z3_id' --target '$gwa_tt' | jq -e '.hostnames[] | select(. == \"$ha_z3\")' >/dev/null" \
        || fail "gw-a zone3 CNAME not found"
    pass "gw-a: 3 CNAMEs in 3 zones"

    log "Waiting for gw-b DNS CNAMEs (2 hostnames across 2 zones)..."
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$z1_id' --target '$gwb_tt' | jq -e '.hostnames[] | select(. == \"$hb_z1\")' >/dev/null" \
        || fail "gw-b zone1 CNAME not found"
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$z2_id' --target '$gwb_tt' | jq -e '.hostnames[] | select(. == \"$hb_z2\")' >/dev/null" \
        || fail "gw-b zone2 CNAME not found"
    pass "gw-b: 2 CNAMEs in 2 zones"

    log "Waiting for gw-c DNS CNAMEs (2 hostnames across 2 zones)..."
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$z2_id' --target '$gwc_tt' | jq -e '.hostnames[] | select(. == \"$hc_z2\")' >/dev/null" \
        || fail "gw-c zone2 CNAME not found"
    retry 60 3 bash -c "cfgwctl dns list-cnames --zone-id '$z3_id' --target '$gwc_tt' | jq -e '.hostnames[] | select(. == \"$hc_z3\")' >/dev/null" \
        || fail "gw-c zone3 CNAME not found"
    pass "gw-c: 2 CNAMEs in 2 zones"

    # --- Delete gw-b (zones 1,2) and verify gw-a and gw-c are unaffected ---
    log "Deleting overlap-gw-b..."
    kubectl delete httproute overlap-route-b -n "$TEST_NS"
    kubectl delete gateway overlap-gw-b -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway overlap-gw-b -n '$TEST_NS' 2>/dev/null" \
        || fail "overlap-gw-b still exists"
    pass "overlap-gw-b deleted"

    log "Verifying gw-b CNAMEs removed..."
    retry 60 3 bash -c "! cfgwctl dns list-cnames --zone-id '$z1_id' --target '$gwb_tt' | jq -e '.hostnames[] | select(. == \"$hb_z1\")' >/dev/null 2>&1" \
        || fail "gw-b zone1 CNAME still exists"
    retry 60 3 bash -c "! cfgwctl dns list-cnames --zone-id '$z2_id' --target '$gwb_tt' | jq -e '.hostnames[] | select(. == \"$hb_z2\")' >/dev/null 2>&1" \
        || fail "gw-b zone2 CNAME still exists"
    pass "gw-b CNAMEs cleaned up"

    log "Verifying gw-a and gw-c CNAMEs unaffected by gw-b deletion..."
    cfgwctl dns list-cnames --zone-id "$z1_id" --target "$gwa_tt" | jq -e ".hostnames[] | select(. == \"$ha_z1\")" >/dev/null || fail "gw-a zone1 CNAME gone"
    cfgwctl dns list-cnames --zone-id "$z2_id" --target "$gwa_tt" | jq -e ".hostnames[] | select(. == \"$ha_z2\")" >/dev/null || fail "gw-a zone2 CNAME gone"
    cfgwctl dns list-cnames --zone-id "$z3_id" --target "$gwa_tt" | jq -e ".hostnames[] | select(. == \"$ha_z3\")" >/dev/null || fail "gw-a zone3 CNAME gone"
    cfgwctl dns list-cnames --zone-id "$z2_id" --target "$gwc_tt" | jq -e ".hostnames[] | select(. == \"$hc_z2\")" >/dev/null || fail "gw-c zone2 CNAME gone"
    cfgwctl dns list-cnames --zone-id "$z3_id" --target "$gwc_tt" | jq -e ".hostnames[] | select(. == \"$hc_z3\")" >/dev/null || fail "gw-c zone3 CNAME gone"
    pass "gw-a and gw-c CNAMEs intact after gw-b deletion"

    # --- Remove zone 3 from gw-a config → should clean up gw-a's zone3 CNAME only ---
    log "Removing zone 3 from gw-a config..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: overlap-params-a
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones:
    - name: "$TEST_ZONE_NAME"
    - name: "$TEST_ZONE_NAME_2"
EOF

    log "Verifying gw-a zone3 CNAME removed..."
    retry 60 3 bash -c "! cfgwctl dns list-cnames --zone-id '$z3_id' --target '$gwa_tt' | jq -e '.hostnames[] | select(. == \"$ha_z3\")' >/dev/null 2>&1" \
        || fail "gw-a zone3 CNAME still exists after zone removal"
    pass "gw-a zone3 CNAME cleaned up"

    log "Verifying gw-a zones 1,2 CNAMEs still intact..."
    cfgwctl dns list-cnames --zone-id "$z1_id" --target "$gwa_tt" | jq -e ".hostnames[] | select(. == \"$ha_z1\")" >/dev/null || fail "gw-a zone1 CNAME gone"
    cfgwctl dns list-cnames --zone-id "$z2_id" --target "$gwa_tt" | jq -e ".hostnames[] | select(. == \"$ha_z2\")" >/dev/null || fail "gw-a zone2 CNAME gone"
    pass "gw-a zones 1,2 CNAMEs intact"

    log "Verifying gw-c zone3 CNAME NOT affected by gw-a's zone removal..."
    cfgwctl dns list-cnames --zone-id "$z3_id" --target "$gwc_tt" | jq -e ".hostnames[] | select(. == \"$hc_z3\")" >/dev/null \
        || fail "gw-c zone3 CNAME was incorrectly removed by gw-a zone change"
    cfgwctl dns list-cnames --zone-id "$z2_id" --target "$gwc_tt" | jq -e ".hostnames[] | select(. == \"$hc_z2\")" >/dev/null \
        || fail "gw-c zone2 CNAME was incorrectly removed"
    pass "gw-c CNAMEs completely unaffected"

    # Cleanup.
    log "Cleaning up multi-gateway overlapping zones test..."
    kubectl delete httproute overlap-route-a overlap-route-c -n "$TEST_NS"
    kubectl delete gateway overlap-gw-a overlap-gw-c -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway overlap-gw-a -n '$TEST_NS' 2>/dev/null" \
        || fail "overlap-gw-a still exists"
    retry 60 3 bash -c "! kubectl get gateway overlap-gw-c -n '$TEST_NS' 2>/dev/null" \
        || fail "overlap-gw-c still exists"
    kubectl delete cloudflaregatewayparameters overlap-params-a overlap-params-b overlap-params-c -n "$TEST_NS" --ignore-not-found
    kubectl delete service overlap-backend -n "$TEST_NS" --ignore-not-found
}

test_load_balancing() {
    local hostname="lb-${TS: -6}.${TEST_TRAFFIC_ZONE_NAME}"

    log "Deploying 10-replica test server for load balancing..."
    kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: lb-test
  namespace: $TEST_NS
spec:
  replicas: 10
  selector:
    matchLabels:
      app: lb-test
  template:
    metadata:
      labels:
        app: lb-test
    spec:
      containers:
      - name: server
        image: $IMAGE
        args: ["test", "serve"]
        env:
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        ports:
        - containerPort: 8080
        readinessProbe:
          httpGet:
            path: /_healthz
            port: 8080
          initialDelaySeconds: 1
          periodSeconds: 2
EOF

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: lb-backend
  namespace: $TEST_NS
spec:
  selector:
    app: lb-test
  ports:
  - port: 80
    targetPort: 8080
    protocol: TCP
EOF

    log "Waiting for lb-test rollout..."
    kubectl rollout status deployment/lb-test -n "$TEST_NS" --timeout=120s \
        || fail "lb-test deployment did not become ready"
    pass "lb-test deployment ready"

    log "Creating Gateway 'lb-gw' (bare Secret)..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: lb-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    retry 60 5 kubectl wait gateway/lb-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "lb-gw did not become Programmed"
    pass "lb-gw is Programmed"

    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: lb-route
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: lb-gw
  hostnames:
  - "$hostname"
  rules:
  - backendRefs:
    - name: lb-backend
      port: 80
EOF

    wait_for_https "https://$hostname/"

    log "Running load test..."
    "$CFGWCTL" test load \
        --url "https://$hostname/" \
        --requests 1000 \
        --concurrency 10 \
        --namespace "$TEST_NS" \
        --label-selector app=lb-test \
        --max-cv 0.5 \
        --hostname "$hostname" \
        || fail "load test failed"
    pass "Load balancing distribution check passed"

    log "Cleaning up load balancing test..."
    kubectl delete httproute lb-route -n "$TEST_NS"
    kubectl delete gateway lb-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway lb-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "lb-gw still exists"
    kubectl delete deployment lb-test -n "$TEST_NS" --ignore-not-found
    kubectl delete service lb-backend -n "$TEST_NS" --ignore-not-found
}

test_traffic_splitting() {
    local hostname="ts-${TS: -6}.${TEST_TRAFFIC_ZONE_NAME}"

    log "Deploying 2 separate backends for traffic splitting..."

    # Deploy ts-svc-a (1 replica)
    kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ts-svc-a
  namespace: $TEST_NS
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ts-svc-a
  template:
    metadata:
      labels:
        app: ts-svc-a
    spec:
      containers:
      - name: server
        image: $IMAGE
        args: ["test", "serve"]
        env:
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        ports:
        - containerPort: 8080
        readinessProbe:
          httpGet:
            path: /_healthz
            port: 8080
          initialDelaySeconds: 1
          periodSeconds: 2
EOF

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: ts-svc-a
  namespace: $TEST_NS
spec:
  selector:
    app: ts-svc-a
  ports:
  - port: 80
    targetPort: 8080
    protocol: TCP
EOF

    # Deploy ts-svc-b (1 replica)
    kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ts-svc-b
  namespace: $TEST_NS
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ts-svc-b
  template:
    metadata:
      labels:
        app: ts-svc-b
    spec:
      containers:
      - name: server
        image: $IMAGE
        args: ["test", "serve"]
        env:
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        ports:
        - containerPort: 8080
        readinessProbe:
          httpGet:
            path: /_healthz
            port: 8080
          initialDelaySeconds: 1
          periodSeconds: 2
EOF

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: ts-svc-b
  namespace: $TEST_NS
spec:
  selector:
    app: ts-svc-b
  ports:
  - port: 80
    targetPort: 8080
    protocol: TCP
EOF

    log "Waiting for ts-svc-a rollout..."
    kubectl rollout status deployment/ts-svc-a -n "$TEST_NS" --timeout=120s \
        || fail "ts-svc-a deployment did not become ready"
    log "Waiting for ts-svc-b rollout..."
    kubectl rollout status deployment/ts-svc-b -n "$TEST_NS" --timeout=120s \
        || fail "ts-svc-b deployment did not become ready"
    pass "Both traffic splitting deployments ready"

    log "Creating Gateway 'ts-gw' (bare Secret)..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: ts-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    retry 60 5 kubectl wait gateway/ts-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "ts-gw did not become Programmed"
    pass "ts-gw is Programmed"

    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: ts-route
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: ts-gw
  hostnames:
  - "$hostname"
  rules:
  - backendRefs:
    - name: ts-svc-a
      port: 80
      weight: 80
    - name: ts-svc-b
      port: 80
      weight: 20
EOF

    wait_for_https "https://$hostname/"

    log "Running traffic splitting load test (200 requests)..."
    "$CFGWCTL" test load \
        --url "https://$hostname/" \
        --requests 200 \
        --concurrency 5 \
        --namespace "$TEST_NS" \
        --backend "app=ts-svc-a:80" \
        --backend "app=ts-svc-b:20" \
        --tolerance 0.15 \
        --hostname "$hostname" \
        || fail "traffic splitting load test failed"
    pass "Traffic splitting distribution check passed"

    log "Cleaning up traffic splitting test..."
    kubectl delete httproute ts-route -n "$TEST_NS"
    kubectl delete gateway ts-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway ts-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "ts-gw still exists"
    kubectl delete deployment ts-svc-a -n "$TEST_NS" --ignore-not-found
    kubectl delete deployment ts-svc-b -n "$TEST_NS" --ignore-not-found
    kubectl delete service ts-svc-a -n "$TEST_NS" --ignore-not-found
    kubectl delete service ts-svc-b -n "$TEST_NS" --ignore-not-found
}

test_session_persistence() {
    local hostname="sp-${TS: -6}.${TEST_TRAFFIC_ZONE_NAME}"

    log "Deploying 2 separate backends for session persistence..."

    # Deploy sp-svc-a (1 replica)
    kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sp-svc-a
  namespace: $TEST_NS
spec:
  replicas: 1
  selector:
    matchLabels:
      app: sp-svc-a
  template:
    metadata:
      labels:
        app: sp-svc-a
    spec:
      containers:
      - name: server
        image: $IMAGE
        args: ["test", "serve"]
        env:
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        ports:
        - containerPort: 8080
        readinessProbe:
          httpGet:
            path: /_healthz
            port: 8080
          initialDelaySeconds: 1
          periodSeconds: 2
EOF

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: sp-svc-a
  namespace: $TEST_NS
spec:
  selector:
    app: sp-svc-a
  ports:
  - port: 80
    targetPort: 8080
    protocol: TCP
EOF

    # Deploy sp-svc-b (1 replica)
    kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sp-svc-b
  namespace: $TEST_NS
spec:
  replicas: 1
  selector:
    matchLabels:
      app: sp-svc-b
  template:
    metadata:
      labels:
        app: sp-svc-b
    spec:
      containers:
      - name: server
        image: $IMAGE
        args: ["test", "serve"]
        env:
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        ports:
        - containerPort: 8080
        readinessProbe:
          httpGet:
            path: /_healthz
            port: 8080
          initialDelaySeconds: 1
          periodSeconds: 2
EOF

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: sp-svc-b
  namespace: $TEST_NS
spec:
  selector:
    app: sp-svc-b
  ports:
  - port: 80
    targetPort: 8080
    protocol: TCP
EOF

    log "Waiting for sp-svc-a rollout..."
    kubectl rollout status deployment/sp-svc-a -n "$TEST_NS" --timeout=120s \
        || fail "sp-svc-a deployment did not become ready"
    log "Waiting for sp-svc-b rollout..."
    kubectl rollout status deployment/sp-svc-b -n "$TEST_NS" --timeout=120s \
        || fail "sp-svc-b deployment did not become ready"
    pass "Both session persistence deployments ready"

    log "Creating Gateway 'sp-gw' (bare Secret)..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: sp-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    retry 60 5 kubectl wait gateway/sp-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "sp-gw did not become Programmed"
    pass "sp-gw is Programmed"

    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: sp-route
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: sp-gw
  hostnames:
  - "$hostname"
  rules:
  - sessionPersistence:
      type: Cookie
    backendRefs:
    - name: sp-svc-a
      port: 80
      weight: 50
    - name: sp-svc-b
      port: 80
      weight: 50
EOF

    wait_for_https "https://$hostname/"

    log "Running session persistence test (50 requests)..."
    "$CFGWCTL" test session \
        --url "https://$hostname/" \
        --requests 50 \
        --cookie-name "cgw-session" \
        --hostname "$hostname" \
        || fail "session persistence test failed"
    pass "Session persistence check passed"

    log "Cleaning up session persistence test..."
    kubectl delete httproute sp-route -n "$TEST_NS"
    kubectl delete gateway sp-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway sp-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "sp-gw still exists"
    kubectl delete deployment sp-svc-a -n "$TEST_NS" --ignore-not-found
    kubectl delete deployment sp-svc-b -n "$TEST_NS" --ignore-not-found
    kubectl delete service sp-svc-a -n "$TEST_NS" --ignore-not-found
    kubectl delete service sp-svc-b -n "$TEST_NS" --ignore-not-found
}

test_vpa_autoscaling() {
    log "Installing VPA CRD..."
    kubectl apply -f https://raw.githubusercontent.com/kubernetes/autoscaler/master/vertical-pod-autoscaler/deploy/vpa-v1-crd-gen.yaml \
        || fail "Failed to install VPA CRD"
    retry 10 2 kubectl get crd verticalpodautoscalers.autoscaling.k8s.io \
        || fail "VPA CRD not found after install"
    pass "VPA CRD installed"

    log "Creating CloudflareGatewayParameters 'vpa-params' with autoscaling enabled..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: vpa-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  tunnel:
    autoscaling:
      enabled: true
      minAllowed:
        cpu: 25m
        memory: 32Mi
      maxAllowed:
        cpu: "1"
        memory: 512Mi
      controlledResources: [cpu, memory]
      controlledValues: RequestsAndLimits
EOF

    log "Creating Gateway 'vpa-gw'..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: vpa-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: vpa-params
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    retry 60 2 kubectl wait gateway/vpa-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "vpa-gw did not become Programmed"
    pass "vpa-gw is Programmed"

    log "Verifying VPA exists..."
    retry 30 2 kubectl get vpa gateway-vpa-gw-primary -n "$TEST_NS" \
        || fail "VPA gateway-vpa-gw-primary not found"
    pass "VPA exists"

    log "Verifying VPA spec..."
    local update_mode
    update_mode=$(kubectl get vpa gateway-vpa-gw-primary -n "$TEST_NS" \
        -o jsonpath='{.spec.updatePolicy.updateMode}')
    [ "$update_mode" = "InPlaceOrRecreate" ] \
        || fail "VPA updateMode expected InPlaceOrRecreate, got '$update_mode'"
    pass "VPA updateMode is InPlaceOrRecreate"

    local target_name
    target_name=$(kubectl get vpa gateway-vpa-gw-primary -n "$TEST_NS" \
        -o jsonpath='{.spec.targetRef.name}')
    [ "$target_name" = "gateway-vpa-gw-primary" ] \
        || fail "VPA targetRef.name expected gateway-vpa-gw-primary, got '$target_name'"
    pass "VPA targetRef is correct"

    # Verify container policies: wildcard Off + tunnel Auto.
    # Use -o json (not jsonpath) so jq can parse the output.
    local policies
    policies=$(kubectl get vpa gateway-vpa-gw-primary -n "$TEST_NS" \
        -o json | jq '.spec.resourcePolicy.containerPolicies')

    echo "$policies" | jq -e '.[] | select(.containerName == "*" and .mode == "Off")' >/dev/null \
        || fail "VPA missing wildcard '*' Off policy"
    pass "VPA has wildcard Off policy"

    echo "$policies" | jq -e '.[] | select(.containerName == "tunnel" and .mode == "Auto")' >/dev/null \
        || fail "VPA missing tunnel Auto policy"
    pass "VPA has tunnel Auto policy"

    # Verify tunnel minAllowed/maxAllowed.
    local tn_min_cpu tn_max_cpu
    tn_min_cpu=$(echo "$policies" | jq -r '.[] | select(.containerName == "tunnel") | .minAllowed.cpu')
    tn_max_cpu=$(echo "$policies" | jq -r '.[] | select(.containerName == "tunnel") | .maxAllowed.cpu')
    [ "$tn_min_cpu" = "25m" ] || fail "tunnel minAllowed.cpu expected 25m, got '$tn_min_cpu'"
    [ "$tn_max_cpu" = "1" ] || fail "tunnel maxAllowed.cpu expected 1, got '$tn_max_cpu'"
    pass "tunnel minAllowed/maxAllowed are correct"

    # Verify tunnel controlledValues.
    local tn_cv
    tn_cv=$(echo "$policies" | jq -r '.[] | select(.containerName == "tunnel") | .controlledValues')
    [ "$tn_cv" = "RequestsAndLimits" ] \
        || fail "tunnel controlledValues expected RequestsAndLimits, got '$tn_cv'"
    pass "tunnel controlledValues is correct"

    log "Disabling autoscaling and verifying VPA cleanup..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: vpa-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
EOF

    retry 30 2 bash -c "! kubectl get vpa gateway-vpa-gw-primary -n '$TEST_NS' 2>/dev/null" \
        || fail "VPA gateway-vpa-gw-primary still exists after disabling autoscaling"
    pass "VPA cleaned up after disabling autoscaling"

    log "Cleaning up VPA test..."
    kubectl delete gateway vpa-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway vpa-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "vpa-gw still exists"
    kubectl delete cloudflaregatewayparameters vpa-params -n "$TEST_NS" --ignore-not-found
}

test_suspend_gateway() {
    log "Creating CloudflareGatewayParameters 'suspend-params'..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: suspend-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones:
    - name: "$TEST_ZONE_NAME"
EOF

    log "Creating Gateway 'suspend-gw'..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: suspend-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: suspend-params
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    retry 60 2 kubectl wait gateway/suspend-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "suspend-gw did not become Programmed"
    pass "suspend-gw is Programmed"

    # Path 1: Happy path — suspend a running Gateway.
    log "Suspending Gateway 'suspend-gw'..."
    local output
    output=$("$CFGWCTL" suspend gateway suspend-gw -n "$TEST_NS" 2>&1)
    echo "$output"
    echo "$output" | grep -q "Suspended reconciliation" \
        || fail "expected 'Suspended reconciliation' in output"
    pass "suspend: happy path"

    # Verify annotation set.
    local ann
    ann=$(kubectl get gateway suspend-gw -n "$TEST_NS" \
        -o jsonpath='{.metadata.annotations.cloudflare-gateway-controller\.io/reconcile}')
    [ "$ann" = "disabled" ] || fail "expected annotation 'disabled', got '$ann'"
    pass "suspend: annotation is disabled"

    # Path 2: Already suspended — run suspend again.
    log "Suspending Gateway 'suspend-gw' again (already suspended)..."
    output=$("$CFGWCTL" suspend gateway suspend-gw -n "$TEST_NS" 2>&1)
    echo "$output"
    echo "$output" | grep -q "already suspended" \
        || fail "expected 'already suspended' in output"
    pass "suspend: already-suspended path"

    # Cleanup: remove annotation, delete resources.
    kubectl annotate gateway suspend-gw -n "$TEST_NS" \
        cloudflare-gateway-controller.io/reconcile- --overwrite
    kubectl delete gateway suspend-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway suspend-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "suspend-gw still exists"
    kubectl delete cloudflaregatewayparameters suspend-params -n "$TEST_NS" --ignore-not-found
}

test_resume_gateway() {
    log "Creating CloudflareGatewayParameters 'resume-params'..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: resume-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones:
    - name: "$TEST_ZONE_NAME"
EOF

    log "Creating Gateway 'resume-gw'..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: resume-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: resume-params
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    retry 60 2 kubectl wait gateway/resume-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "resume-gw did not become Programmed"
    pass "resume-gw is Programmed"

    # Path 1: Happy path — suspend first, then resume.
    log "Suspending Gateway 'resume-gw' first..."
    "$CFGWCTL" suspend gateway resume-gw -n "$TEST_NS" \
        || fail "failed to suspend resume-gw"

    log "Resuming Gateway 'resume-gw'..."
    local output
    output=$("$CFGWCTL" resume gateway resume-gw -n "$TEST_NS" 2>&1)
    echo "$output"
    echo "$output" | grep -q "Resumed reconciliation" \
        || fail "expected 'Resumed reconciliation' in output"
    echo "$output" | grep -q "Reconciliation completed" \
        || fail "expected 'Reconciliation completed' in output"
    pass "resume: happy path"

    # Verify annotation is enabled.
    local ann
    ann=$(kubectl get gateway resume-gw -n "$TEST_NS" \
        -o jsonpath='{.metadata.annotations.cloudflare-gateway-controller\.io/reconcile}')
    [ "$ann" = "enabled" ] || fail "expected annotation 'enabled', got '$ann'"
    pass "resume: annotation is enabled"

    # Path 2: Not suspended — resume a non-suspended Gateway.
    log "Resuming Gateway 'resume-gw' again (not suspended)..."
    output=$("$CFGWCTL" resume gateway resume-gw -n "$TEST_NS" 2>&1)
    echo "$output"
    echo "$output" | grep -q "not suspended" \
        || fail "expected 'not suspended' in output"
    pass "resume: not-suspended path"

    # Cleanup.
    kubectl delete gateway resume-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway resume-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "resume-gw still exists"
    kubectl delete cloudflaregatewayparameters resume-params -n "$TEST_NS" --ignore-not-found
}

test_reconcile_gateway() {
    log "Creating CloudflareGatewayParameters 'reconcile-params'..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: reconcile-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones:
    - name: "$TEST_ZONE_NAME"
EOF

    log "Creating Gateway 'reconcile-gw'..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: reconcile-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: reconcile-params
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    retry 60 2 kubectl wait gateway/reconcile-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "reconcile-gw did not become Programmed"
    pass "reconcile-gw is Programmed"

    # Path 1: Happy path — trigger reconcile.
    log "Triggering reconcile for Gateway 'reconcile-gw'..."
    local output
    output=$("$CFGWCTL" reconcile gateway reconcile-gw -n "$TEST_NS" 2>&1)
    echo "$output"
    echo "$output" | grep -q "Requested reconciliation" \
        || fail "expected 'Requested reconciliation' in output"
    echo "$output" | grep -q "Reconciliation completed" \
        || fail "expected 'Reconciliation completed' in output"
    pass "reconcile: happy path"

    # Path 2: Error when suspended.
    log "Suspending Gateway 'reconcile-gw'..."
    "$CFGWCTL" suspend gateway reconcile-gw -n "$TEST_NS" \
        || fail "failed to suspend reconcile-gw"

    log "Triggering reconcile on suspended Gateway (should fail)..."
    if output=$("$CFGWCTL" reconcile gateway reconcile-gw -n "$TEST_NS" 2>&1); then
        fail "expected reconcile to fail on suspended Gateway, but it succeeded"
    fi
    echo "$output"
    echo "$output" | grep -q "suspended" \
        || fail "expected error mentioning 'suspended' in output"
    pass "reconcile: suspended error path"

    # Cleanup: un-suspend, delete resources.
    kubectl annotate gateway reconcile-gw -n "$TEST_NS" \
        cloudflare-gateway-controller.io/reconcile- --overwrite
    kubectl delete gateway reconcile-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway reconcile-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "reconcile-gw still exists"
    kubectl delete cloudflaregatewayparameters reconcile-params -n "$TEST_NS" --ignore-not-found
}

test_rotate_gateway_token() {
    local hostname="rot-${TS: -6}.${TEST_TRAFFIC_ZONE_NAME}"

    log "Deploying test server for token rotation..."
    kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: rot-test
  namespace: $TEST_NS
spec:
  replicas: 1
  selector:
    matchLabels:
      app: rot-test
  template:
    metadata:
      labels:
        app: rot-test
    spec:
      containers:
      - name: server
        image: $IMAGE
        args: ["test", "serve"]
        env:
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        ports:
        - containerPort: 8080
        readinessProbe:
          httpGet:
            path: /_healthz
            port: 8080
          initialDelaySeconds: 1
          periodSeconds: 2
EOF

    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: rot-backend
  namespace: $TEST_NS
spec:
  selector:
    app: rot-test
  ports:
  - port: 80
    targetPort: 8080
    protocol: TCP
EOF

    log "Waiting for rot-test rollout..."
    kubectl rollout status deployment/rot-test -n "$TEST_NS" --timeout=120s \
        || fail "rot-test deployment did not become ready"
    pass "rot-test deployment ready"

    log "Creating CloudflareGatewayParameters 'rot-params' with 3 replicas..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: rot-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  tunnel:
    minReadySeconds: 30
    replicas:
    - name: r1
    - name: r2
    - name: r3
EOF

    log "Creating Gateway 'rot-gw'..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: rot-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: rot-params
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    retry 60 5 kubectl wait gateway/rot-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "rot-gw did not become Programmed"
    pass "rot-gw is Programmed"

    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: rot-route
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: rot-gw
  hostnames:
  - "$hostname"
  rules:
  - backendRefs:
    - name: rot-backend
      port: 80
EOF

    wait_for_https "https://$hostname/"

    # Record tunnel ID and token before rotation.
    local rot_tunnel_name rot_tunnel_id token_before
    rot_tunnel_name=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "rot-gw")
    rot_tunnel_id=$(cfgwctl tunnel get-id --name "$rot_tunnel_name" | jq -r '.tunnelId')
    [ -n "$rot_tunnel_id" ] && [ "$rot_tunnel_id" != "null" ] || fail "rot tunnel not found"
    token_before=$(cfgwctl tunnel get-token --tunnel-id "$rot_tunnel_id" | jq -r '.token')
    [ -n "$token_before" ] && [ "$token_before" != "null" ] || fail "could not get token before rotation"

    # Record pod name(s) before rotation.
    local pod_names_before
    pod_names_before=$(kubectl get pods -n "$TEST_NS" -l app.kubernetes.io/instance=rot-gw \
        -o jsonpath='{.items[*].metadata.name}')

    # Start background load generator (runs until signaled to stop).
    log "Starting background load generator..."
    "$CFGWCTL" test load \
        --url "https://$hostname/" \
        --duration 10m \
        --concurrency 5 \
        --hostname "$hostname" \
        --min-success-rate 0.999 &
    local LOAD_PID=$!

    # Let traffic flow for 10 seconds before rotating.
    log "Waiting 10s for traffic to stabilize..."
    sleep 10

    # Record lastTokenRotatedAt before rotation.
    local last_rotated_before
    last_rotated_before=$(kubectl get cloudflaregatewaystatuses rot-gw -n "$TEST_NS" \
        -o jsonpath='{.status.lastTokenRotatedAt}' 2>/dev/null || true)

    # Path 1: Happy path — rotate token with --watch.
    # minReadySeconds=30 in CGP makes this complete in ~2 minutes.
    log "Rotating token for Gateway 'rot-gw'..."
    local rotate_output_file
    rotate_output_file=$(mktemp)
    "$CFGWCTL" rotate gateway token rot-gw -n "$TEST_NS" \
        --watch --timeout 10m 2>&1 | tee "$rotate_output_file" \
        || fail "rotate command failed"
    pass "rotate: happy path with rolling update timeline validated"

    # Validate exactly 3 Gateway events in the watch output.
    local gw_event_count
    gw_event_count=$(grep -c 'Gateway/' "$rotate_output_file")
    [ "$gw_event_count" -eq 3 ] \
        || fail "expected 3 Gateway events in rotate output, got $gw_event_count"
    grep -q 'Gateway/.*rotation-requested' "$rotate_output_file" \
        || fail "missing 'rotation-requested' Gateway event"
    grep -q 'Gateway/.*conditions-changed Programmed=True Ready=Unknown' "$rotate_output_file" \
        || fail "missing 'conditions-changed Programmed=True Ready=Unknown' Gateway event"
    grep -q 'Gateway/.*conditions-changed Programmed=True Ready=True' "$rotate_output_file" \
        || fail "missing 'conditions-changed Programmed=True Ready=True' Gateway event"
    pass "rotate: Gateway events match expected sequence"
    rm -f "$rotate_output_file"

    # The rotate command watches Deployments until fully rolled out.
    # Verify the Gateway is Ready and that lastTokenRotatedAt changed.
    kubectl wait gateway/rot-gw -n "$TEST_NS" \
        --for=condition=Ready --timeout=0 \
        || fail "rot-gw not Ready=True after rotate command returned"
    local last_rotated_after
    last_rotated_after=$(kubectl get cloudflaregatewaystatuses rot-gw -n "$TEST_NS" \
        -o jsonpath='{.status.lastTokenRotatedAt}')
    [ "$last_rotated_before" != "$last_rotated_after" ] \
        || fail "lastTokenRotatedAt did not change after rotation"
    pass "rotate: Gateway is Ready=True and lastTokenRotatedAt updated"

    # Stop load generator gracefully (SIGTERM triggers results printing).
    log "Stopping load generator..."
    kill "$LOAD_PID" 2>/dev/null || true
    wait "$LOAD_PID" || fail "load test failed during token rotation"
    pass "rotate: no traffic disruption during rotation (100% 2xx)"

    # Verify token changed on Cloudflare API.
    local token_after
    token_after=$(cfgwctl tunnel get-token --tunnel-id "$rot_tunnel_id" | jq -r '.token')
    [ "$token_before" != "$token_after" ] || fail "token did not change after rotation"
    pass "rotate: token changed on Cloudflare API"

    # Verify in-cluster Secret matches Cloudflare API token.
    local token_in_cluster
    token_in_cluster=$(kubectl get secret gateway-rot-gw -n "$TEST_NS" \
        -o jsonpath='{.data.TUNNEL_TOKEN}' | base64 -d)
    [ "$token_in_cluster" = "$token_after" ] \
        || fail "in-cluster token does not match Cloudflare API token"
    pass "rotate: in-cluster Secret matches Cloudflare API token"

    # Verify all tunnel Deployments completed rollout (belt-and-suspenders).
    # We check ReadyReplicas instead of using `kubectl rollout status` because
    # minReadySeconds=600 means AvailableReplicas stays 0 for 10 minutes.
    log "Verifying all tunnel Deployment rollouts completed..."
    for replica in r1 r2 r3; do
        local ready
        ready=$(kubectl get deployment gateway-rot-gw-${replica} -n "$TEST_NS" \
            -o jsonpath='{.status.readyReplicas}')
        [ "${ready:-0}" -ge 1 ] \
            || fail "tunnel Deployment gateway-rot-gw-${replica} rollout not complete after rotate returned (readyReplicas=${ready:-0})"
    done
    local pod_names_after
    pod_names_after=$(kubectl get pods -n "$TEST_NS" -l app.kubernetes.io/instance=rot-gw \
        -o jsonpath='{.items[*].metadata.name}')
    [ "$pod_names_before" != "$pod_names_after" ] || fail "tunnel pods were not replaced after token rotation"
    pass "rotate: all tunnel pods replaced via rolling restart"

    # Path 2: Error when suspended.
    log "Suspending Gateway 'rot-gw'..."
    "$CFGWCTL" suspend gateway rot-gw -n "$TEST_NS" \
        || fail "failed to suspend rot-gw"

    log "Rotating token on suspended Gateway (should fail)..."
    if output=$("$CFGWCTL" rotate gateway token rot-gw -n "$TEST_NS" 2>&1); then
        fail "expected rotate to fail on suspended Gateway, but it succeeded"
    fi
    echo "$output"
    echo "$output" | grep -q "suspended" \
        || fail "expected error mentioning 'suspended' in output"
    pass "rotate: suspended error path"

    # Cleanup: un-suspend, delete resources.
    kubectl annotate gateway rot-gw -n "$TEST_NS" \
        cloudflare-gateway-controller.io/reconcile- --overwrite
    kubectl delete httproute rot-route -n "$TEST_NS"
    kubectl delete gateway rot-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway rot-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "rot-gw still exists"
    kubectl delete deployment rot-test -n "$TEST_NS" --ignore-not-found
    kubectl delete service rot-backend -n "$TEST_NS" --ignore-not-found
    kubectl delete cloudflaregatewayparameters rot-params -n "$TEST_NS" --ignore-not-found
}

test_podinfo() {
    local hostname="pi-${TS: -6}.${TEST_TRAFFIC_ZONE_NAME}"

    command -v podcli >/dev/null 2>&1 || fail "podcli not found in PATH"

    log "Creating Gateway 'pi-gw' (bare Secret)..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: pi-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
EOF

    retry 60 5 kubectl wait gateway/pi-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "pi-gw did not become Programmed"
    pass "pi-gw is Programmed"

    log "Installing podinfo via Helm..."
    helm repo add podinfo https://stefanprodan.github.io/podinfo 2>/dev/null || true
    helm repo update podinfo
    helm upgrade --install podinfo podinfo/podinfo \
        --namespace "$TEST_NS" \
        --version '>=6.0.0, <7.0.0' \
        --set httpRoute.enabled=true \
        --set-json "httpRoute.parentRefs=[{\"name\":\"pi-gw\"}]" \
        --set "httpRoute.hostnames[0]=$hostname" \
        --wait --timeout 120s
    pass "podinfo installed"

    log "Creating GRPCRoute for podinfo gRPC..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: GRPCRoute
metadata:
  name: podinfo-grpc
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: pi-gw
  hostnames:
  - "$hostname"
  rules:
  - backendRefs:
    - name: podinfo
      port: 9999
EOF

    wait_for_https "https://$hostname/"

    log "Running podcli check http..."
    retry 5 3 podcli check http "https://$hostname/" \
        || fail "podcli check http failed"
    pass "podcli check http passed"

    log "Running podcli check grpc..."
    retry 5 3 podcli check grpc "$hostname:443" --service=podinfo --tls \
        || fail "podcli check grpc failed"
    pass "podcli check grpc passed"

    log "Running podcli check cert..."
    retry 5 3 podcli check cert "$hostname" \
        || fail "podcli check cert failed"
    pass "podcli check cert passed"

    log "Running podcli check tcp..."
    retry 5 3 podcli check tcp "$hostname:443" \
        || fail "podcli check tcp failed"
    pass "podcli check tcp passed"

    log "Running podcli check ws..."
    retry 5 3 podcli check ws "wss://$hostname/ws/echo" \
        || fail "podcli check ws failed"
    pass "podcli check ws passed"

    log "Cleaning up podinfo test..."
    kubectl delete grpcroute podinfo-grpc -n "$TEST_NS"
    helm uninstall podinfo -n "$TEST_NS"
    kubectl delete gateway pi-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway pi-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "pi-gw still exists"
}

# ─── Run ──────────────────────────────────────────────────────────────────────

run_tests \
    test_gateway_lifecycle \
    test_multi_routes \
    test_dns_default_all_zones \
    test_no_dns \
    test_disabled_reconciliation \
    test_dns_config_removal \
    test_multi_zone_dns \
    test_multi_gateway_overlapping_zones \
    test_cluster_recreation \
    test_load_balancing \
    test_traffic_splitting \
    test_session_persistence \
    test_vpa_autoscaling \
    test_suspend_gateway \
    test_resume_gateway \
    test_reconcile_gateway \
    test_rotate_gateway_token \
    test_podinfo
