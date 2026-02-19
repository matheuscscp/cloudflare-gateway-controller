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
TEST_HOSTNAME="e2e-test-$(date +%s).${TEST_ZONE_NAME}"
IMAGE_REPO="${IMAGE%:*}"
IMAGE_TAG="${IMAGE##*:}"

# Logging helpers.
log()  { echo "==> $*"; }
pass() { echo "PASS: $*"; }
fail() {
    echo "FAIL: $*"
    echo "--- Pods ---"
    kubectl get pods -A --no-headers 2>/dev/null || true
    echo "--- Controller logs (last 30) ---"
    kubectl logs -n "$CONTROLLER_NS" -l app.kubernetes.io/name="$RELEASE_NAME" --tail=30 2>/dev/null || true
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
  controllerName: github.com/matheuscscp/cloudflare-gateway-controller
EOF

log "Waiting for GatewayClass to be Ready..."
retry 30 2 kubectl wait gatewayclass/cloudflare --for=condition=Ready --timeout=5s \
    || fail "GatewayClass did not become Ready"
pass "GatewayClass is Ready"

# ─── Phase 3: Gateway lifecycle ────────────────────────────────────────────────

log "Creating Gateway 'test-gateway' with zone annotation..."
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: test-gateway
  namespace: $TEST_NS
  annotations:
    gateway.cloudflare-gateway-controller.matheuscscp.github.com/zoneName: "$TEST_ZONE_NAME"
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
retry 30 2 check_tunnel_has_hostname || fail "tunnel config does not contain hostname"
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
retry 30 2 check_dns_cname || fail "DNS CNAME not found"
pass "DNS CNAME exists"

# ─── Phase 5: HTTPRoute deletion ───────────────────────────────────────────────

log "Deleting HTTPRoute 'test-route'..."
kubectl delete httproute test-route -n "$TEST_NS"

log "Waiting for tunnel config to remove '$TEST_HOSTNAME'..."
check_tunnel_no_hostname() {
    ! cfgwctl tunnel get-config --tunnel-id "$TUNNEL_ID" \
        | jq -e ".[] | select(.hostname == \"$TEST_HOSTNAME\")" >/dev/null 2>&1
}
retry 30 2 check_tunnel_no_hostname || fail "tunnel config still contains hostname"
pass "Tunnel config updated after route deletion"

log "Verifying DNS CNAME removed for '$TEST_HOSTNAME'..."
check_dns_cname_removed() {
    ! cfgwctl dns list-cnames --zone-id "$ZONE_ID" --target "$TUNNEL_TARGET" \
        | jq -e ".hostnames[] | select(. == \"$TEST_HOSTNAME\")" >/dev/null 2>&1
}
retry 30 2 check_dns_cname_removed || fail "DNS CNAME still exists"
pass "DNS CNAME removed"

# ─── Phase 6: Gateway deletion ─────────────────────────────────────────────────

log "Deleting Gateway 'test-gateway'..."
kubectl delete gateway test-gateway -n "$TEST_NS"

log "Waiting for Gateway to be fully deleted..."
retry 30 2 bash -c "! kubectl get gateway test-gateway -n '$TEST_NS' 2>/dev/null" \
    || fail "Gateway still exists"
pass "Gateway deleted"

log "Verifying tunnel deleted..."
check_tunnel_deleted() {
    local id
    id=$(cfgwctl tunnel get-id --name "$TUNNEL_NAME" | jq -r '.tunnelId')
    [ -z "$id" ] || [ "$id" = "null" ]
}
retry 30 2 check_tunnel_deleted || fail "tunnel still exists"
pass "Tunnel deleted"

# ─── Done ──────────────────────────────────────────────────────────────────────

echo ""
echo "========================================"
echo "  All E2E tests passed!"
echo "========================================"
