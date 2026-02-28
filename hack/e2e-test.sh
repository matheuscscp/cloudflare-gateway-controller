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
  - name: http
    protocol: HTTP
    port: 80
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

    log "Waiting for tunnel config to include '$test_hostname'..."
    check_tunnel_has_hostname() {
        cfgwctl tunnel get-config --tunnel-id "$tunnel_id" \
            | jq -e ".[] | select(.hostname == \"$test_hostname\")" >/dev/null
    }
    retry 60 3 check_tunnel_has_hostname || fail "tunnel config does not contain hostname"
    pass "Tunnel config has hostname"

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

    log "Waiting for tunnel config to remove '$test_hostname'..."
    check_tunnel_no_hostname() {
        ! cfgwctl tunnel get-config --tunnel-id "$tunnel_id" \
            | jq -e ".[] | select(.hostname == \"$test_hostname\")" >/dev/null 2>&1
    }
    retry 60 3 check_tunnel_no_hostname || fail "tunnel config still contains hostname"
    pass "Tunnel config updated after route deletion"

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
  - name: http
    protocol: HTTP
    port: 80
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

    log "Verifying tunnel config has both hostnames..."
    retry 60 3 bash -c "cfgwctl tunnel get-config --tunnel-id '$mr_tunnel_id' | jq -e '.[] | select(.hostname == \"$hostname_a\")' >/dev/null" \
        || fail "tunnel config missing hostname A"
    retry 60 3 bash -c "cfgwctl tunnel get-config --tunnel-id '$mr_tunnel_id' | jq -e '.[] | select(.hostname == \"$hostname_b\")' >/dev/null" \
        || fail "tunnel config missing hostname B"
    pass "Tunnel config has both hostnames"

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
    retry 60 3 bash -c "! cfgwctl tunnel get-config --tunnel-id '$mr_tunnel_id' | jq -e '.[] | select(.hostname == \"$hostname_a\")' >/dev/null 2>&1" \
        || fail "tunnel config still has hostname A"
    cfgwctl tunnel get-config --tunnel-id "$mr_tunnel_id" | jq -e ".[] | select(.hostname == \"$hostname_b\")" >/dev/null \
        || fail "tunnel config lost hostname B"
    pass "Tunnel config correctly updated after partial route deletion"

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

test_path_matching() {
    local path_hostname="paths-${TS: -6}.test"

    log "Creating Gateway 'path-gw' (no DNS config)..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: path-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: ""
      kind: Secret
      name: cloudflare-creds
  listeners:
  - name: http
    protocol: HTTP
    port: 80
EOF

    retry 60 2 kubectl wait gateway/path-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "path-gw did not become Programmed"
    pass "path-gw is Programmed"

    local path_tunnel_name path_tunnel_id
    path_tunnel_name=$(cf_resource_name "$KIND_CLUSTER_NAME" "$TEST_NS" "path-gw")
    path_tunnel_id=$(cfgwctl tunnel get-id --name "$path_tunnel_name" | jq -r '.tunnelId')
    [ -n "$path_tunnel_id" ] && [ "$path_tunnel_id" != "null" ] || fail "path tunnel not found"

    log "Creating Services and HTTPRoute with path matches..."
    for svc in api-svc web-svc; do
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
  name: path-route
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: path-gw
  hostnames:
  - "$path_hostname"
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /api
    backendRefs:
    - name: api-svc
      port: 80
  - matches:
    - path:
        type: PathPrefix
        value: /web
    backendRefs:
    - name: web-svc
      port: 80
EOF

    log "Verifying tunnel config has path-based entries..."
    retry 60 3 bash -c "cfgwctl tunnel get-config --tunnel-id '$path_tunnel_id' | jq -e '.[] | select(.hostname == \"$path_hostname\" and .path == \"/api\")' >/dev/null" \
        || fail "tunnel config missing /api path"
    retry 60 3 bash -c "cfgwctl tunnel get-config --tunnel-id '$path_tunnel_id' | jq -e '.[] | select(.hostname == \"$path_hostname\" and .path == \"/web\")' >/dev/null" \
        || fail "tunnel config missing /web path"
    pass "Tunnel config has path-based entries"

    log "Cleaning up path matching test..."
    kubectl delete httproute path-route -n "$TEST_NS"
    kubectl delete gateway path-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway path-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "path-gw still exists"
    kubectl delete service api-svc web-svc -n "$TEST_NS" --ignore-not-found
}

test_dns_default_all_zones() {
    local dns_def_hostname="dd-${TS: -6}.${TEST_ZONE_NAME}"

    log "Creating Gateway 'dns-def-gw' without CloudflareGatewayParameters (bare Secret)..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: dns-def-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: ""
      kind: Secret
      name: cloudflare-creds
  listeners:
  - name: http
    protocol: HTTP
    port: 80
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
  - name: http
    protocol: HTTP
    port: 80
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

    log "Verifying tunnel config has hostname..."
    retry 60 3 bash -c "cfgwctl tunnel get-config --tunnel-id '$nd_tunnel_id' | jq -e '.[] | select(.hostname == \"$no_dns_hostname\")' >/dev/null" \
        || fail "no-dns tunnel config missing hostname"
    pass "Tunnel config has hostname"

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

test_deployment_patches() {
    log "Creating CloudflareGatewayParameters 'patched-params' with deployment patches..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: patched-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  tunnel:
    patches:
    - op: add
      path: /spec/template/metadata/labels/e2e-patch
      value: "applied"
EOF

    log "Creating Gateway 'patched-gw' with deployment patches..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: patched-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: patched-params
  listeners:
  - name: http
    protocol: HTTP
    port: 80
EOF

    retry 60 2 kubectl wait gateway/patched-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "patched-gw did not become Programmed"
    pass "patched-gw is Programmed"

    log "Verifying cloudflared Deployment has patched label..."
    local patch_label
    patch_label=$(kubectl get deployment cloudflared-patched-gw -n "$TEST_NS" \
        -o jsonpath='{.spec.template.metadata.labels.e2e-patch}')
    [ "$patch_label" = "applied" ] || fail "Deployment patch not applied: got '$patch_label'"
    pass "Deployment patch applied"

    log "Cleaning up deployment patches test..."
    kubectl delete gateway patched-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway patched-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "patched-gw still exists"
    kubectl delete cloudflaregatewayparameters patched-params -n "$TEST_NS" --ignore-not-found
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
  - name: http
    protocol: HTTP
    port: 80
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

    log "Verifying cloudflared Deployment still exists (orphaned)..."
    kubectl get deployment cloudflared-disabled-gw -n "$TEST_NS" >/dev/null 2>&1 \
        || fail "cloudflared Deployment was deleted (should be orphaned)"
    pass "Cloudflared Deployment is orphaned"

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
    kubectl delete deployment cloudflared-disabled-gw -n "$TEST_NS" --ignore-not-found
    retry 60 3 bash -c "! kubectl get deployment cloudflared-disabled-gw -n '$TEST_NS' 2>/dev/null"
    kubectl delete secret cloudflared-token-disabled-gw -n "$TEST_NS" --ignore-not-found
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
  - name: http
    protocol: HTTP
    port: 80
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

    log "Verifying tunnel config still has hostname (tunnel config unaffected)..."
    cfgwctl tunnel get-config --tunnel-id "$zr_tunnel_id" \
        | jq -e ".[] | select(.hostname == \"$zone_rm_hostname\")" >/dev/null \
        || fail "tunnel config lost hostname after DNS config removal"
    pass "Tunnel config still has hostname"

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
  - name: http
    protocol: HTTP
    port: 80
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
  - name: http
    protocol: HTTP
    port: 80
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

    log "Verifying tunnel config has hostname after adoption..."
    retry 60 3 bash -c "cfgwctl tunnel get-config --tunnel-id '$tunnel_id_after' | jq -e '.[] | select(.hostname == \"$test_hostname\")' >/dev/null" \
        || fail "tunnel config missing hostname after adoption"
    pass "Tunnel config has hostname after adoption"

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

test_multiple_listeners_rejected() {
    log "Creating Gateway 'multi-listen-gw' with two listeners..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: multi-listen-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: ""
      kind: Secret
      name: cloudflare-creds
  listeners:
  - name: http
    protocol: HTTP
    port: 80
  - name: https
    protocol: HTTP
    port: 8080
EOF

    log "Verifying Gateway is rejected with Accepted=False..."
    retry 60 3 bash -c "
        accepted_status=\$(kubectl get gateway multi-listen-gw -n '$TEST_NS' -o jsonpath='{.status.conditions[?(@.type==\"Accepted\")].status}')
        accepted_reason=\$(kubectl get gateway multi-listen-gw -n '$TEST_NS' -o jsonpath='{.status.conditions[?(@.type==\"Accepted\")].reason}')
        [ \"\$accepted_status\" = 'False' ] && [ \"\$accepted_reason\" = 'ListenersNotValid' ]
    " || fail "multi-listen-gw should be rejected with Accepted=False/ListenersNotValid"
    pass "Gateway with multiple listeners correctly rejected"

    log "Cleaning up multiple listeners test..."
    kubectl delete gateway multi-listen-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway multi-listen-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "multi-listen-gw still exists"
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
  - name: http
    protocol: HTTP
    port: 80
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
  - name: http
    protocol: HTTP
    port: 80
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
  infrastructure:
    parametersRef:
      group: ""
      kind: Secret
      name: cloudflare-creds
  listeners:
  - name: http
    protocol: HTTP
    port: 80
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

    log "Waiting for HTTPS endpoint to be reachable at '$hostname'..."
    for i in $(seq 1 60); do
        if curl -sf "https://$hostname/" >/dev/null 2>&1; then break; fi
        if [ "$i" = "60" ]; then
            echo "Last curl attempt output:"
            curl -sv "https://$hostname/" 2>&1 || true
            fail "HTTPS endpoint not reachable at $hostname"
        fi
        sleep 5
    done
    pass "HTTPS endpoint reachable"

    log "Running load test..."
    "$CFGWCTL" test load \
        --url "https://$hostname/" \
        --requests 1000 \
        --concurrency 10 \
        --namespace "$TEST_NS" \
        --label-selector app=lb-test \
        --max-cv 0.5 \
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

test_sidecar_disabled() {
    local hostname="sd-${TS: -6}.${TEST_TRAFFIC_ZONE_NAME}"

    log "Deploying 1-replica test server for sidecar-disabled test..."
    kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sd-test
  namespace: $TEST_NS
spec:
  replicas: 1
  selector:
    matchLabels:
      app: sd-test
  template:
    metadata:
      labels:
        app: sd-test
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
  name: sd-backend
  namespace: $TEST_NS
spec:
  selector:
    app: sd-test
  ports:
  - port: 80
    targetPort: 8080
    protocol: TCP
EOF

    log "Waiting for sd-test rollout..."
    kubectl rollout status deployment/sd-test -n "$TEST_NS" --timeout=120s \
        || fail "sd-test deployment did not become ready"
    pass "sd-test deployment ready"

    log "Creating CloudflareGatewayParameters 'sd-params' with sidecar disabled..."
    kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: sd-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
  tunnel:
    sidecar:
      enabled: false
EOF

    log "Creating Gateway 'sd-gw' referencing sd-params..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: sd-gw
  namespace: $TEST_NS
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: sd-params
  listeners:
  - name: http
    protocol: HTTP
    port: 80
EOF

    retry 60 5 kubectl wait gateway/sd-gw -n "$TEST_NS" \
        --for=condition=Programmed --timeout=5s \
        || fail "sd-gw did not become Programmed"
    pass "sd-gw is Programmed"

    log "Verifying Sidecar condition is False/Disabled..."
    check_sidecar_disabled() {
        local reason
        reason=$(kubectl get gateway sd-gw -n "$TEST_NS" \
            -o jsonpath='{.status.conditions[?(@.type=="Sidecar")].reason}')
        [ "$reason" = "Disabled" ]
    }
    retry 30 2 check_sidecar_disabled || fail "Sidecar condition not Disabled"
    pass "Sidecar condition is False/Disabled"

    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: sd-route
  namespace: $TEST_NS
spec:
  parentRefs:
  - name: sd-gw
  hostnames:
  - "$hostname"
  rules:
  - backendRefs:
    - name: sd-backend
      port: 80
EOF

    log "Waiting for HTTPS endpoint to be reachable at '$hostname'..."
    for i in $(seq 1 60); do
        if curl -sf "https://$hostname/" >/dev/null 2>&1; then break; fi
        if [ "$i" = "60" ]; then
            echo "Last curl attempt output:"
            curl -sv "https://$hostname/" 2>&1 || true
            fail "HTTPS endpoint not reachable at $hostname"
        fi
        sleep 5
    done
    pass "HTTPS endpoint reachable"

    log "Running traffic check (sidecar disabled)..."
    "$CFGWCTL" test load \
        --url "https://$hostname/" \
        --requests 10 \
        --concurrency 1 \
        || fail "traffic check failed"
    pass "Traffic flows end-to-end with sidecar disabled"

    log "Cleaning up sidecar-disabled test..."
    kubectl delete httproute sd-route -n "$TEST_NS"
    kubectl delete gateway sd-gw -n "$TEST_NS"
    retry 60 3 bash -c "! kubectl get gateway sd-gw -n '$TEST_NS' 2>/dev/null" \
        || fail "sd-gw still exists"
    kubectl delete cloudflaregatewayparameters sd-params -n "$TEST_NS" --ignore-not-found
    kubectl delete deployment sd-test -n "$TEST_NS" --ignore-not-found
    kubectl delete service sd-backend -n "$TEST_NS" --ignore-not-found
}

# ─── Run ──────────────────────────────────────────────────────────────────────

run_tests \
    test_gateway_lifecycle \
    test_multi_routes \
    test_path_matching \
    test_dns_default_all_zones \
    test_no_dns \
    test_deployment_patches \
    test_disabled_reconciliation \
    test_dns_config_removal \
    test_multiple_listeners_rejected \
    test_multi_zone_dns \
    test_multi_gateway_overlapping_zones \
    test_cluster_recreation \
    test_load_balancing \
    test_sidecar_disabled
