#!/usr/bin/env bash
# Copyright 2026 Matheus Pimenta.
# SPDX-License-Identifier: AGPL-3.0
#
# End-to-end test script for cloudflare-gateway-controller.
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

set -euo pipefail

# Configuration.
IMAGE="${IMAGE:-cloudflare-gateway-controller:dev}"
CREDENTIALS_FILE="${CREDENTIALS_FILE:-./api.token}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-cfgw-e2e}"
CFGWCTL="${CFGWCTL:-./bin/cfgwctl}"
TEST_ZONE_NAME="${TEST_ZONE_NAME:-dev.cloudflare-gateway-controller.dev}"

# Derived values.
CHART_DIR="charts/cloudflare-gateway-controller"
RELEASE_NAME="cloudflare-gateway-controller"
CONTROLLER_NS="cfgw-system"
TEST_NS="cfgw-e2e-test"
TS=$(date +%s)
TEST_HOSTNAME="e2e-${TS: -6}.${TEST_ZONE_NAME}"
IMAGE_REPO="${IMAGE%:*}"
IMAGE_TAG="${IMAGE##*:}"

# Logging helpers.
log()  { echo "==> $*"; }
pass() { echo "PASS: $*"; }
fail() {
    echo "FAIL: $*"
    echo "--- Pods ---"
    kubectl get pods -A --no-headers 2>/dev/null || true
    echo "--- Controller logs ---"
    kubectl logs -n "$CONTROLLER_NS" -l app.kubernetes.io/name="$RELEASE_NAME" 2>/dev/null || true
    echo "--- Events ---"
    kubectl get events -A --sort-by='.lastTimestamp' 2>/dev/null || true
    echo "--- GatewayClass status ---"
    kubectl get gatewayclass -o yaml 2>/dev/null || true
    echo "--- Gateway status ---"
    kubectl get gateway -A -o yaml 2>/dev/null || true
    echo "--- HTTPRoute status ---"
    kubectl get httproute -A -o yaml 2>/dev/null || true
    exit 1
}

# Cleanup on exit.
cleanup() {
    log "Cleaning up..."
    kind delete cluster --name "$KIND_CLUSTER_NAME" 2>/dev/null || true
}
trap cleanup EXIT

# Validate prerequisites.
for cmd in kind kubectl helm jq "$CFGWCTL"; do
    command -v "$cmd" >/dev/null 2>&1 || fail "required command not found: $cmd"
done
[ -f "$CREDENTIALS_FILE" ] || fail "credentials file not found: $CREDENTIALS_FILE"

# cfgwctl shorthand.
cfgwctl() { "$CFGWCTL" --credentials-file "$CREDENTIALS_FILE" "$@"; }

# retry runs a command up to $1 times with $2 second delay between attempts.
retry() {
    local max_attempts=$1 delay=$2
    shift 2
    for i in $(seq 1 "$max_attempts"); do
        if "$@" 2>/dev/null; then return 0; fi
        sleep "$delay"
    done
    return 1
}

# ─── Phase 1: Cluster setup ────────────────────────────────────────────────────

log "Creating kind cluster '$KIND_CLUSTER_NAME'..."
kind delete cluster --name "$KIND_CLUSTER_NAME" 2>/dev/null || true
kind create cluster --name "$KIND_CLUSTER_NAME" --wait 60s

log "Loading controller image '$IMAGE' into kind..."
kind load docker-image "$IMAGE" --name "$KIND_CLUSTER_NAME"

log "Installing controller via Helm..."
kubectl create namespace "$CONTROLLER_NS"
helm install "$RELEASE_NAME" "$CHART_DIR" \
    --namespace "$CONTROLLER_NS" \
    --set image.repository="$IMAGE_REPO" \
    --set image.tag="$IMAGE_TAG" \
    --set image.pullPolicy=Never \
    --set 'podArgs[0]=--log-level=debug' \
    --wait --timeout 120s

log "Waiting for controller deployment to be ready..."
kubectl rollout status deployment/"$RELEASE_NAME" -n "$CONTROLLER_NS" --timeout=120s

pass "Controller is running"

# ─── Phase 2: Test resources ───────────────────────────────────────────────────

log "Creating test namespace '$TEST_NS'..."
kubectl create namespace "$TEST_NS"

log "Creating Cloudflare credentials Secret..."
kubectl create secret generic cloudflare-creds \
    --from-env-file="$CREDENTIALS_FILE" \
    -n "$TEST_NS"

log "Creating GatewayClass 'cloudflare'..."
kubectl apply -f - <<'EOF'
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: cloudflare
spec:
  controllerName: cloudflare-gateway-controller.io/controller
EOF

log "Waiting for GatewayClass to be Ready..."
retry 60 3 kubectl wait gatewayclass/cloudflare --for=condition=Ready --timeout=5s \
    || fail "GatewayClass did not become Ready"
pass "GatewayClass is Ready"

# ─── Phase 3: Gateway lifecycle ────────────────────────────────────────────────

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
    zone:
      name: "$TEST_ZONE_NAME"
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

# Get tunnel name from Gateway UID.
GW_UID=$(kubectl get gateway test-gateway -n "$TEST_NS" -o jsonpath='{.metadata.uid}')
TUNNEL_NAME="gateway-${GW_UID}"

log "Verifying tunnel '$TUNNEL_NAME' exists..."
TUNNEL_ID=$(cfgwctl tunnel get-id --name "$TUNNEL_NAME" | jq -r '.tunnelId')
[ -n "$TUNNEL_ID" ] && [ "$TUNNEL_ID" != "null" ] || fail "tunnel not found"
pass "Tunnel exists: $TUNNEL_ID"

# ─── Phase 4: HTTPRoute lifecycle ──────────────────────────────────────────────

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

log "Creating HTTPRoute with hostname '$TEST_HOSTNAME'..."
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
  - "$TEST_HOSTNAME"
  rules:
  - backendRefs:
    - name: test-backend
      port: 80
EOF

log "Waiting for tunnel config to include '$TEST_HOSTNAME'..."
check_tunnel_has_hostname() {
    cfgwctl tunnel get-config --tunnel-id "$TUNNEL_ID" \
        | jq -e ".[] | select(.hostname == \"$TEST_HOSTNAME\")" >/dev/null
}
retry 60 3 check_tunnel_has_hostname || fail "tunnel config does not contain hostname"
pass "Tunnel config has hostname"

log "Finding zone ID for '$TEST_ZONE_NAME'..."
ZONE_ID=$(cfgwctl dns find-zone --hostname "$TEST_HOSTNAME" | jq -r '.zoneId')
[ -n "$ZONE_ID" ] && [ "$ZONE_ID" != "null" ] || fail "zone ID not found"

log "Verifying DNS CNAME for '$TEST_HOSTNAME'..."
TUNNEL_TARGET="${TUNNEL_ID}.cfargotunnel.com"
check_dns_cname() {
    cfgwctl dns list-cnames --zone-id "$ZONE_ID" --target "$TUNNEL_TARGET" \
        | jq -e ".hostnames[] | select(. == \"$TEST_HOSTNAME\")" >/dev/null
}
retry 60 3 check_dns_cname || fail "DNS CNAME not found"
pass "DNS CNAME exists"

# ─── Phase 5: HTTPRoute deletion ───────────────────────────────────────────────

log "Deleting HTTPRoute 'test-route'..."
kubectl delete httproute test-route -n "$TEST_NS"

log "Waiting for tunnel config to remove '$TEST_HOSTNAME'..."
check_tunnel_no_hostname() {
    ! cfgwctl tunnel get-config --tunnel-id "$TUNNEL_ID" \
        | jq -e ".[] | select(.hostname == \"$TEST_HOSTNAME\")" >/dev/null 2>&1
}
retry 60 3 check_tunnel_no_hostname || fail "tunnel config still contains hostname"
pass "Tunnel config updated after route deletion"

log "Verifying DNS CNAME removed for '$TEST_HOSTNAME'..."
check_dns_cname_removed() {
    ! cfgwctl dns list-cnames --zone-id "$ZONE_ID" --target "$TUNNEL_TARGET" \
        | jq -e ".hostnames[] | select(. == \"$TEST_HOSTNAME\")" >/dev/null 2>&1
}
retry 60 3 check_dns_cname_removed || fail "DNS CNAME still exists"
pass "DNS CNAME removed"

# ─── Phase 6: Gateway deletion ─────────────────────────────────────────────────

log "Deleting Gateway 'test-gateway'..."
kubectl delete gateway test-gateway -n "$TEST_NS"

log "Waiting for Gateway to be fully deleted..."
retry 60 3 bash -c "! kubectl get gateway test-gateway -n '$TEST_NS' 2>/dev/null" \
    || fail "Gateway still exists"
pass "Gateway deleted"

log "Verifying tunnel deleted..."
check_tunnel_deleted() {
    local id
    id=$(cfgwctl tunnel get-id --name "$TUNNEL_NAME" | jq -r '.tunnelId')
    [ -z "$id" ] || [ "$id" = "null" ]
}
retry 60 3 check_tunnel_deleted || fail "tunnel still exists"
pass "Tunnel deleted"

# ─── Phase 7: Multiple HTTPRoutes with multiple hostnames ─────────────────────

HOSTNAME_A="a-${TS: -6}.${TEST_ZONE_NAME}"
HOSTNAME_B="b-${TS: -6}.${TEST_ZONE_NAME}"

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
    zone:
      name: "$TEST_ZONE_NAME"
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

MR_GW_UID=$(kubectl get gateway multi-route-gw -n "$TEST_NS" -o jsonpath='{.metadata.uid}')
MR_TUNNEL_NAME="gateway-${MR_GW_UID}"
MR_TUNNEL_ID=$(cfgwctl tunnel get-id --name "$MR_TUNNEL_NAME" | jq -r '.tunnelId')
[ -n "$MR_TUNNEL_ID" ] && [ "$MR_TUNNEL_ID" != "null" ] || fail "multi-route tunnel not found"
MR_TUNNEL_TARGET="${MR_TUNNEL_ID}.cfargotunnel.com"

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
  - "$HOSTNAME_A"
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
  - "$HOSTNAME_B"
  rules:
  - backendRefs:
    - name: backend-b
      port: 80
EOF

log "Verifying tunnel config has both hostnames..."
check_mr_has_a() {
    cfgwctl tunnel get-config --tunnel-id "$MR_TUNNEL_ID" \
        | jq -e ".[] | select(.hostname == \"$HOSTNAME_A\")" >/dev/null
}
check_mr_has_b() {
    cfgwctl tunnel get-config --tunnel-id "$MR_TUNNEL_ID" \
        | jq -e ".[] | select(.hostname == \"$HOSTNAME_B\")" >/dev/null
}
retry 60 3 check_mr_has_a || fail "tunnel config missing hostname A"
retry 60 3 check_mr_has_b || fail "tunnel config missing hostname B"
pass "Tunnel config has both hostnames"

MR_ZONE_ID=$(cfgwctl dns find-zone --hostname "$HOSTNAME_A" | jq -r '.zoneId')

log "Verifying both DNS CNAMEs exist..."
check_mr_dns_a() {
    cfgwctl dns list-cnames --zone-id "$MR_ZONE_ID" --target "$MR_TUNNEL_TARGET" \
        | jq -e ".hostnames[] | select(. == \"$HOSTNAME_A\")" >/dev/null
}
check_mr_dns_b() {
    cfgwctl dns list-cnames --zone-id "$MR_ZONE_ID" --target "$MR_TUNNEL_TARGET" \
        | jq -e ".hostnames[] | select(. == \"$HOSTNAME_B\")" >/dev/null
}
retry 60 3 check_mr_dns_a || fail "DNS CNAME for hostname A not found"
retry 60 3 check_mr_dns_b || fail "DNS CNAME for hostname B not found"
pass "Both DNS CNAMEs exist"

log "Deleting route-a only..."
kubectl delete httproute route-a -n "$TEST_NS"

log "Verifying hostname A removed but hostname B still present..."
check_mr_no_a() {
    ! cfgwctl tunnel get-config --tunnel-id "$MR_TUNNEL_ID" \
        | jq -e ".[] | select(.hostname == \"$HOSTNAME_A\")" >/dev/null 2>&1
}
retry 60 3 check_mr_no_a || fail "tunnel config still has hostname A"
check_mr_has_b || fail "tunnel config lost hostname B"
pass "Tunnel config correctly updated after partial route deletion"

log "Verifying DNS CNAME A removed but B still exists..."
check_mr_dns_a_removed() {
    ! cfgwctl dns list-cnames --zone-id "$MR_ZONE_ID" --target "$MR_TUNNEL_TARGET" \
        | jq -e ".hostnames[] | select(. == \"$HOSTNAME_A\")" >/dev/null 2>&1
}
retry 60 3 check_mr_dns_a_removed || fail "DNS CNAME for A still exists"
check_mr_dns_b || fail "DNS CNAME for B disappeared"
pass "DNS CNAMEs correctly updated"

log "Cleaning up multi-route test..."
kubectl delete httproute route-b -n "$TEST_NS"
kubectl delete gateway multi-route-gw -n "$TEST_NS"
retry 60 3 bash -c "! kubectl get gateway multi-route-gw -n '$TEST_NS' 2>/dev/null" \
    || fail "multi-route-gw still exists"
check_mr_tunnel_deleted() {
    local id
    id=$(cfgwctl tunnel get-id --name "$MR_TUNNEL_NAME" | jq -r '.tunnelId')
    [ -z "$id" ] || [ "$id" = "null" ]
}
retry 60 3 check_mr_tunnel_deleted || fail "multi-route tunnel still exists"
pass "Phase 7: Multiple HTTPRoutes passed"

# ─── Phase 8: HTTPRoute with path matching ────────────────────────────────────

PATH_HOSTNAME="paths-${TS: -6}.test"

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

PATH_GW_UID=$(kubectl get gateway path-gw -n "$TEST_NS" -o jsonpath='{.metadata.uid}')
PATH_TUNNEL_NAME="gateway-${PATH_GW_UID}"
PATH_TUNNEL_ID=$(cfgwctl tunnel get-id --name "$PATH_TUNNEL_NAME" | jq -r '.tunnelId')
[ -n "$PATH_TUNNEL_ID" ] && [ "$PATH_TUNNEL_ID" != "null" ] || fail "path tunnel not found"

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
  - "$PATH_HOSTNAME"
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
check_path_api() {
    cfgwctl tunnel get-config --tunnel-id "$PATH_TUNNEL_ID" \
        | jq -e ".[] | select(.hostname == \"$PATH_HOSTNAME\" and .path == \"/api\")" >/dev/null
}
check_path_web() {
    cfgwctl tunnel get-config --tunnel-id "$PATH_TUNNEL_ID" \
        | jq -e ".[] | select(.hostname == \"$PATH_HOSTNAME\" and .path == \"/web\")" >/dev/null
}
retry 60 3 check_path_api || fail "tunnel config missing /api path"
retry 60 3 check_path_web || fail "tunnel config missing /web path"
pass "Tunnel config has path-based entries"

log "Cleaning up path matching test..."
kubectl delete httproute path-route -n "$TEST_NS"
kubectl delete gateway path-gw -n "$TEST_NS"
retry 60 3 bash -c "! kubectl get gateway path-gw -n '$TEST_NS' 2>/dev/null" \
    || fail "path-gw still exists"
pass "Phase 8: Path matching passed"

# ─── Phase 9: Gateway without DNS config (no DNS) ────────────────────────────

NO_DNS_HOSTNAME="nd-${TS: -6}.${TEST_ZONE_NAME}"

log "Creating Gateway 'no-dns-gw' without DNS config..."
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
      group: ""
      kind: Secret
      name: cloudflare-creds
  listeners:
  - name: http
    protocol: HTTP
    port: 80
EOF

retry 60 2 kubectl wait gateway/no-dns-gw -n "$TEST_NS" \
    --for=condition=Programmed --timeout=5s \
    || fail "no-dns-gw did not become Programmed"
pass "no-dns-gw is Programmed"

NO_DNS_GW_UID=$(kubectl get gateway no-dns-gw -n "$TEST_NS" -o jsonpath='{.metadata.uid}')
NO_DNS_TUNNEL_NAME="gateway-${NO_DNS_GW_UID}"
NO_DNS_TUNNEL_ID=$(cfgwctl tunnel get-id --name "$NO_DNS_TUNNEL_NAME" | jq -r '.tunnelId')
[ -n "$NO_DNS_TUNNEL_ID" ] && [ "$NO_DNS_TUNNEL_ID" != "null" ] || fail "no-dns tunnel not found"

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
  - "$NO_DNS_HOSTNAME"
  rules:
  - backendRefs:
    - name: no-dns-backend
      port: 80
EOF

log "Verifying tunnel config has hostname..."
check_no_dns_has_hostname() {
    cfgwctl tunnel get-config --tunnel-id "$NO_DNS_TUNNEL_ID" \
        | jq -e ".[] | select(.hostname == \"$NO_DNS_HOSTNAME\")" >/dev/null
}
retry 60 3 check_no_dns_has_hostname || fail "no-dns tunnel config missing hostname"
pass "Tunnel config has hostname"

log "Verifying NO DNS CNAME was created..."
NO_DNS_ZONE_ID=$(cfgwctl dns find-zone --hostname "$NO_DNS_HOSTNAME" | jq -r '.zoneId')
NO_DNS_TUNNEL_TARGET="${NO_DNS_TUNNEL_ID}.cfargotunnel.com"
check_no_dns_cname_absent() {
    ! cfgwctl dns list-cnames --zone-id "$NO_DNS_ZONE_ID" --target "$NO_DNS_TUNNEL_TARGET" \
        | jq -e ".hostnames[] | select(. == \"$NO_DNS_HOSTNAME\")" >/dev/null 2>&1
}
# Use Consistently-style check: verify absence now and after a brief wait.
check_no_dns_cname_absent || fail "DNS CNAME exists when it should not"
sleep 5
check_no_dns_cname_absent || fail "DNS CNAME appeared when it should not"
pass "No DNS CNAME created (as expected)"

log "Cleaning up no-DNS test..."
kubectl delete httproute no-dns-route -n "$TEST_NS"
kubectl delete gateway no-dns-gw -n "$TEST_NS"
retry 60 3 bash -c "! kubectl get gateway no-dns-gw -n '$TEST_NS' 2>/dev/null" \
    || fail "no-dns-gw still exists"
pass "Phase 9: No DNS passed"

# ─── Phase 10: Deployment patches via CloudflareGatewayParameters ─────────────

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
  tunnels:
    cloudflared:
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
PATCH_LABEL=$(kubectl get deployment cloudflared-patched-gw -n "$TEST_NS" \
    -o jsonpath='{.spec.template.metadata.labels.e2e-patch}')
[ "$PATCH_LABEL" = "applied" ] || fail "Deployment patch not applied: got '$PATCH_LABEL'"
pass "Deployment patch applied"

log "Cleaning up deployment patches test..."
kubectl delete gateway patched-gw -n "$TEST_NS"
retry 60 3 bash -c "! kubectl get gateway patched-gw -n '$TEST_NS' 2>/dev/null" \
    || fail "patched-gw still exists"
pass "Phase 10: Deployment patches passed"

# ─── Phase 11: Deletion while reconciliation is disabled ──────────────────────

DISABLED_HOSTNAME="dis-${TS: -6}.${TEST_ZONE_NAME}"

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
    zone:
      name: "$TEST_ZONE_NAME"
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

DIS_GW_UID=$(kubectl get gateway disabled-gw -n "$TEST_NS" -o jsonpath='{.metadata.uid}')
DIS_TUNNEL_NAME="gateway-${DIS_GW_UID}"
DIS_TUNNEL_ID=$(cfgwctl tunnel get-id --name "$DIS_TUNNEL_NAME" | jq -r '.tunnelId')
[ -n "$DIS_TUNNEL_ID" ] && [ "$DIS_TUNNEL_ID" != "null" ] || fail "disabled tunnel not found"
DIS_TUNNEL_TARGET="${DIS_TUNNEL_ID}.cfargotunnel.com"

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
  - "$DISABLED_HOSTNAME"
  rules:
  - backendRefs:
    - name: disabled-backend
      port: 80
EOF

log "Waiting for DNS CNAME for disabled-gw..."
DIS_ZONE_ID=$(cfgwctl dns find-zone --hostname "$DISABLED_HOSTNAME" | jq -r '.zoneId')
check_dis_dns() {
    cfgwctl dns list-cnames --zone-id "$DIS_ZONE_ID" --target "$DIS_TUNNEL_TARGET" \
        | jq -e ".hostnames[] | select(. == \"$DISABLED_HOSTNAME\")" >/dev/null
}
retry 60 3 check_dis_dns || fail "DNS CNAME for disabled-gw not found"
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
DIS_TUNNEL_CHECK=$(cfgwctl tunnel get-id --name "$DIS_TUNNEL_NAME" | jq -r '.tunnelId')
[ "$DIS_TUNNEL_CHECK" = "$DIS_TUNNEL_ID" ] || fail "tunnel was deleted (should still exist)"
pass "Tunnel still exists"

log "Verifying DNS CNAME still exists..."
check_dis_dns || fail "DNS CNAME was deleted (should still exist)"
pass "DNS CNAME still exists"

log "Manual cleanup of orphaned resources..."
kubectl delete deployment cloudflared-disabled-gw -n "$TEST_NS" --ignore-not-found
kubectl delete secret cloudflared-token-disabled-gw -n "$TEST_NS" --ignore-not-found
cfgwctl dns delete-cname --zone-id "$DIS_ZONE_ID" --hostname "$DISABLED_HOSTNAME"
cfgwctl tunnel cleanup-connections --tunnel-id "$DIS_TUNNEL_ID"
cfgwctl tunnel delete --tunnel-id "$DIS_TUNNEL_ID"
pass "Phase 11: Deletion while disabled passed"

# ─── Phase 12: DNS config removal cleans up DNS ──────────────────────────────

ZONE_RM_HOSTNAME="zrm-${TS: -6}.${TEST_ZONE_NAME}"

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
    zone:
      name: "$TEST_ZONE_NAME"
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

ZR_GW_UID=$(kubectl get gateway zone-rm-gw -n "$TEST_NS" -o jsonpath='{.metadata.uid}')
ZR_TUNNEL_NAME="gateway-${ZR_GW_UID}"
ZR_TUNNEL_ID=$(cfgwctl tunnel get-id --name "$ZR_TUNNEL_NAME" | jq -r '.tunnelId')
[ -n "$ZR_TUNNEL_ID" ] && [ "$ZR_TUNNEL_ID" != "null" ] || fail "zone-rm tunnel not found"
ZR_TUNNEL_TARGET="${ZR_TUNNEL_ID}.cfargotunnel.com"

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
  - "$ZONE_RM_HOSTNAME"
  rules:
  - backendRefs:
    - name: zone-rm-backend
      port: 80
EOF

log "Waiting for DNS CNAME..."
ZR_ZONE_ID=$(cfgwctl dns find-zone --hostname "$ZONE_RM_HOSTNAME" | jq -r '.zoneId')
check_zr_dns() {
    cfgwctl dns list-cnames --zone-id "$ZR_ZONE_ID" --target "$ZR_TUNNEL_TARGET" \
        | jq -e ".hostnames[] | select(. == \"$ZONE_RM_HOSTNAME\")" >/dev/null
}
retry 60 3 check_zr_dns || fail "DNS CNAME for zone-rm not found"
pass "DNS CNAME exists"

log "Removing DNS config from CloudflareGatewayParameters..."
kubectl apply -f - <<EOF
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: zone-rm-params
  namespace: $TEST_NS
spec:
  secretRef:
    name: cloudflare-creds
EOF

log "Verifying DNS CNAME removed..."
check_zr_dns_removed() {
    ! cfgwctl dns list-cnames --zone-id "$ZR_ZONE_ID" --target "$ZR_TUNNEL_TARGET" \
        | jq -e ".hostnames[] | select(. == \"$ZONE_RM_HOSTNAME\")" >/dev/null 2>&1
}
retry 60 3 check_zr_dns_removed || fail "DNS CNAME still exists after zone removal"
pass "DNS CNAME removed after DNS config removal"

log "Verifying tunnel config still has hostname (tunnel config unaffected)..."
check_zr_has_hostname() {
    cfgwctl tunnel get-config --tunnel-id "$ZR_TUNNEL_ID" \
        | jq -e ".[] | select(.hostname == \"$ZONE_RM_HOSTNAME\")" >/dev/null
}
check_zr_has_hostname || fail "tunnel config lost hostname after DNS config removal"
pass "Tunnel config still has hostname"

log "Cleaning up DNS config removal test..."
kubectl delete httproute zone-rm-route -n "$TEST_NS"
kubectl delete gateway zone-rm-gw -n "$TEST_NS"
retry 60 3 bash -c "! kubectl get gateway zone-rm-gw -n '$TEST_NS' 2>/dev/null" \
    || fail "zone-rm-gw still exists"
pass "Phase 12: DNS config removal passed"

# ─── Phase 13: Multiple listeners ────────────────────────────────────────────

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
    protocol: HTTPS
    port: 443
EOF

retry 60 2 kubectl wait gateway/multi-listen-gw -n "$TEST_NS" \
    --for=condition=Programmed --timeout=5s \
    || fail "multi-listen-gw did not become Programmed"
pass "multi-listen-gw is Programmed"

log "Verifying both listeners are Accepted..."
LISTENER_COUNT=$(kubectl get gateway multi-listen-gw -n "$TEST_NS" \
    -o jsonpath='{.status.listeners}' | jq 'length')
[ "$LISTENER_COUNT" = "2" ] || fail "expected 2 listener statuses, got $LISTENER_COUNT"

HTTP_ACCEPTED=$(kubectl get gateway multi-listen-gw -n "$TEST_NS" \
    -o jsonpath='{.status.listeners[?(@.name=="http")].conditions[?(@.type=="Accepted")].status}')
HTTPS_ACCEPTED=$(kubectl get gateway multi-listen-gw -n "$TEST_NS" \
    -o jsonpath='{.status.listeners[?(@.name=="https")].conditions[?(@.type=="Accepted")].status}')
[ "$HTTP_ACCEPTED" = "True" ] || fail "http listener not Accepted: $HTTP_ACCEPTED"
[ "$HTTPS_ACCEPTED" = "True" ] || fail "https listener not Accepted: $HTTPS_ACCEPTED"
pass "Both listeners Accepted"

log "Cleaning up multiple listeners test..."
kubectl delete gateway multi-listen-gw -n "$TEST_NS"
retry 60 3 bash -c "! kubectl get gateway multi-listen-gw -n '$TEST_NS' 2>/dev/null" \
    || fail "multi-listen-gw still exists"
pass "Phase 13: Multiple listeners passed"

# ─── Done ──────────────────────────────────────────────────────────────────────

echo ""
echo "========================================"
echo "  All e2e tests passed!"
echo "========================================"
