#!/usr/bin/env bash
# Copyright 2026 Matheus Pimenta.
# SPDX-License-Identifier: AGPL-3.0
#
# Shared library for e2e test scripts. Source this file from each e2e script.
#
# Required variables (must be set BEFORE sourcing):
#   TEST_ZONE_NAME       — Cloudflare DNS zone name for testing
#   KIND_CLUSTER_NAME    — kind cluster name
#   TEST_NS              — Kubernetes namespace for test resources
#
# Optional variables:
#   IMAGE                — controller image (default: cloudflare-gateway-controller:dev)
#   CREDENTIALS_FILE     — path to credentials file (default: ./api.token)
#   CFGWCTL              — path to cfgwctl binary (default: ./bin/cfgwctl)
#   KIND_CONFIG          — path to kind config file (default: none, single-node)
#   REUSE_CLUSTER        — if "1", reuse existing kind cluster
#   REUSE_CONTROLLER     — if "1", skip controller install if already running
#   RELOAD_CONTROLLER    — if "1", reload image and restart controller
#   CONTROLLER_LOG_FILE  — path to controller log file (default: test-<cluster>-controller.log)
#   TEST                 — if set, run only the named test function

# Configuration defaults.
IMAGE="${IMAGE:-cloudflare-gateway-controller:dev}"
CREDENTIALS_FILE="${CREDENTIALS_FILE:-./api.token}"
CFGWCTL="${CFGWCTL:-./bin/cfgwctl}"
CHART_DIR="charts/cloudflare-gateway-controller"
RELEASE_NAME="cloudflare-gateway-controller"
CONTROLLER_NS="cfgw-system"

# Derived values.
IMAGE_REPO="${IMAGE%:*}"
IMAGE_TAG="${IMAGE##*:}"

# Timing.
TS=$(date +%s)
PHASE_START=$TS

# Track whether we created the cluster (for cleanup).
_CREATED_CLUSTER=0
_LOG_STREAM_PID=""
CONTROLLER_LOG_FILE="${CONTROLLER_LOG_FILE:-test-${KIND_CLUSTER_NAME}-controller.log}"
: > "$CONTROLLER_LOG_FILE"

# ─── Logging ──────────────────────────────────────────────────────────────────

phase_delta() {
    local now elapsed
    now=$(date +%s)
    elapsed=$((now - PHASE_START))
    echo "(${elapsed}s)"
    PHASE_START=$now
}

log()  { echo "==> $*"; }
pass() { echo "PASS: $*"; }
fail() {
    echo "FAIL: $*"
    echo "--- Pods ---"
    kubectl get pods -A --no-headers 2>/dev/null || true
    echo "--- Controller logs (last 100 lines) ---"
    kubectl logs -n "$CONTROLLER_NS" -l app.kubernetes.io/name="$RELEASE_NAME" --tail=100 2>/dev/null || true
    echo "--- Events (last 50) ---"
    kubectl get events -A --sort-by='.lastTimestamp' 2>/dev/null | tail -50 || true
    echo "--- GatewayClass status ---"
    kubectl get gatewayclass -o yaml 2>/dev/null || true
    echo "--- Gateway status ---"
    kubectl get gateway -A -o yaml 2>/dev/null || true
    echo "--- HTTPRoute status ---"
    kubectl get httproute -A -o yaml 2>/dev/null || true
    exit 1
}

# ─── Helpers ──────────────────────────────────────────────────────────────────

# retry runs a command up to $1 times with $2 second delay between attempts.
retry() {
    local max_attempts=$1 delay=$2
    shift 2
    local output
    for i in $(seq 1 "$max_attempts"); do
        output=$("$@" 2>&1) && return 0
        printf "  retry %d/%d: %s\n" "$i" "$max_attempts" "$output"
        sleep "$delay"
    done
    return 1
}

# cfgwctl shorthand. Exported so bash -c subshells can use it.
cfgwctl() { "$CFGWCTL" test --credentials-file "$CREDENTIALS_FILE" "$@"; }
export -f cfgwctl
export CFGWCTL CREDENTIALS_FILE

# cf_resource_name computes a deterministic Cloudflare resource name from the
# given parts, matching the Go TunnelName() function.
cf_resource_name() {
    echo -n "cloudflare-gateway-controller.io/clusters/$1/namespaces/$2/gateways/$3"
}
export -f cf_resource_name

# sidecar_config_has_hostname checks whether the sidecar ConfigMap for a given
# gateway contains a route entry for the specified hostname (and optional pathPrefix).
sidecar_config_has_hostname() {
    local gw_name="$1" hostname="$2" path_prefix="${3:-}"
    local filter=".routes[] | select(.hostname == \"$hostname\")"
    if [ -n "$path_prefix" ]; then
        filter=".routes[] | select(.hostname == \"$hostname\" and .pathPrefix == \"$path_prefix\")"
    fi
    kubectl get configmap "gateway-${gw_name}" -n "$TEST_NS" \
        -o jsonpath='{.data.config\.yaml}' | yq -e "$filter" >/dev/null
}
export -f sidecar_config_has_hostname
export TEST_NS

# wait_for_https polls an HTTPS URL until it returns 2xx, printing the full
# curl error (DNS failures, connection refused, SSL errors, HTTP status) on
# every attempt so you can debug in real time instead of waiting for a timeout.
wait_for_https() {
    local hostname="${1#https://}"
    hostname="${hostname%%/*}"
    log "Waiting for HTTPS endpoint to be reachable at '$hostname'..."
    for i in $(seq 1 60); do
        local output
        output=$(curl -sS --connect-timeout 5 --max-time 10 -o /dev/null -w '\nHTTP %{http_code}' "https://$hostname/" 2>&1 || true)
        printf "  attempt %d/60: %s\n" "$i" "$output"
        if [[ "$output" == *"HTTP 2"* ]]; then
            pass "HTTPS endpoint reachable"
            return 0
        fi
        sleep 5
    done
    fail "HTTPS endpoint not reachable at $hostname after 60 attempts"
}

# ─── Prerequisites ────────────────────────────────────────────────────────────

validate_prerequisites() {
    for cmd in kind kubectl helm jq "$CFGWCTL"; do
        command -v "$cmd" >/dev/null 2>&1 || fail "required command not found: $cmd"
    done
    [ -f "$CREDENTIALS_FILE" ] || fail "credentials file not found: $CREDENTIALS_FILE"
    [ -n "$TEST_ZONE_NAME" ] || fail "TEST_ZONE_NAME is required"
}

# ─── Cluster lifecycle ────────────────────────────────────────────────────────

# setup_cluster creates a kind cluster or reuses an existing one.
# If REUSE_CLUSTER=1 and the cluster exists, skip creation.
# Otherwise, create and register cleanup trap.
setup_cluster() {
    if [ "${REUSE_CLUSTER:-}" = "1" ] && kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER_NAME"; then
        log "Reusing existing kind cluster '$KIND_CLUSTER_NAME'"
        # Ensure kubeconfig is set for existing cluster.
        kubectl cluster-info --context "kind-${KIND_CLUSTER_NAME}" >/dev/null 2>&1 \
            || fail "Cluster '$KIND_CLUSTER_NAME' exists but is not accessible"
        return
    fi

    log "Creating kind cluster '$KIND_CLUSTER_NAME'..."
    kind delete cluster --name "$KIND_CLUSTER_NAME" 2>/dev/null || true

    local kind_args=(create cluster --name "$KIND_CLUSTER_NAME" --wait 60s)
    if [ -n "${KIND_CONFIG:-}" ]; then
        kind_args+=(--config "$KIND_CONFIG")
    fi
    kind "${kind_args[@]}"
    _CREATED_CLUSTER=1
}

# cleanup deletes the kind cluster if we created it and stops the log stream.
cleanup() {
    if [ -n "$_LOG_STREAM_PID" ]; then
        kill "$_LOG_STREAM_PID" 2>/dev/null || true
        wait "$_LOG_STREAM_PID" 2>/dev/null || true
    fi
    if [ "$_CREATED_CLUSTER" = "1" ]; then
        log "Cleaning up kind cluster '$KIND_CLUSTER_NAME'..."
        kind delete cluster --name "$KIND_CLUSTER_NAME" 2>/dev/null || true
    fi
}

# register_cleanup sets up the EXIT trap. Call this after setup_cluster.
register_cleanup() {
    trap cleanup EXIT
}

# start_controller_log_stream streams controller logs to a file in the background.
# Call this after install_controller.
start_controller_log_stream() {
    log "Streaming controller logs to '$CONTROLLER_LOG_FILE'..."
    kubectl logs -n "$CONTROLLER_NS" -l app.kubernetes.io/name="$RELEASE_NAME" \
        -f --tail=-1 >>"$CONTROLLER_LOG_FILE" 2>&1 &
    _LOG_STREAM_PID=$!
}

# ─── Controller lifecycle ─────────────────────────────────────────────────────

# install_controller loads the image and installs/upgrades the controller.
# If REUSE_CONTROLLER=1 and the controller is Running, skip.
# If RELOAD_CONTROLLER=1, reload image and restart.
# Any extra arguments are forwarded to helm upgrade --install.
install_controller() {
    if [ "${REUSE_CONTROLLER:-}" = "1" ]; then
        local status
        status=$(kubectl get pods -n "$CONTROLLER_NS" -l app.kubernetes.io/name="$RELEASE_NAME" \
            -o jsonpath='{.items[0].status.phase}' 2>/dev/null || echo "")
        if [ "$status" = "Running" ]; then
            log "Reusing existing controller (Running)"
            if [ "${RELOAD_CONTROLLER:-}" = "1" ]; then
                log "Reloading controller image..."
                kind load docker-image "$IMAGE" --name "$KIND_CLUSTER_NAME"
                kubectl rollout restart deployment/"$RELEASE_NAME" -n "$CONTROLLER_NS"
                kubectl rollout status deployment/"$RELEASE_NAME" -n "$CONTROLLER_NS" --timeout=120s
                log "Controller reloaded"
            fi
            return
        fi
    fi

    log "Loading controller image '$IMAGE' into kind..."
    kind load docker-image "$IMAGE" --name "$KIND_CLUSTER_NAME"

    log "Installing controller via Helm..."
    kubectl create namespace "$CONTROLLER_NS" 2>/dev/null || true
    helm upgrade --install "$RELEASE_NAME" "$CHART_DIR" \
        --namespace "$CONTROLLER_NS" \
        --set image.repository="$IMAGE_REPO" \
        --set image.tag="$IMAGE_TAG" \
        --set image.pullPolicy=Never \
        --set config.clusterName="$KIND_CLUSTER_NAME" \
        --set 'podArgs[0]=--log-level=debug' \
        "$@" \
        --wait --timeout 120s

    log "Waiting for controller deployment to be ready..."
    kubectl rollout status deployment/"$RELEASE_NAME" -n "$CONTROLLER_NS" --timeout=120s
}

# ─── Test resources ───────────────────────────────────────────────────────────

# create_test_namespace creates the test namespace and credentials Secret
# if they don't already exist.
create_test_namespace() {
    kubectl create namespace "$TEST_NS" 2>/dev/null || true
    if ! kubectl get secret cloudflare-creds -n "$TEST_NS" >/dev/null 2>&1; then
        log "Creating Cloudflare credentials Secret..."
        kubectl create secret generic cloudflare-creds \
            --from-env-file="$CREDENTIALS_FILE" \
            -n "$TEST_NS"
    fi
}

# ensure_gatewayclass creates the GatewayClass and waits for it to be Ready.
ensure_gatewayclass() {
    log "Ensuring GatewayClass 'cloudflare'..."
    kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: cloudflare
spec:
  controllerName: cloudflare-gateway-controller.io/controller
  parametersRef:
    group: ""
    kind: Secret
    name: cloudflare-creds
    namespace: $TEST_NS
EOF
    retry 60 3 kubectl wait gatewayclass/cloudflare --for=condition=Ready --timeout=5s \
        || fail "GatewayClass did not become Ready"
    pass "GatewayClass is Ready"
}

# ─── Test runner ──────────────────────────────────────────────────────────────

# run_tests runs all test functions or a single one if TEST is set.
# Arguments: list of test function names in order.
run_tests() {
    local tests=("$@")

    if [ -n "${TEST:-}" ]; then
        # Run a single named test.
        local found=0
        for t in "${tests[@]}"; do
            if [ "$t" = "$TEST" ]; then
                found=1
                log "Running single test: $TEST"
                "$TEST"
                pass "$TEST $(phase_delta)"
                break
            fi
        done
        if [ "$found" = "0" ]; then
            echo "Unknown test: $TEST"
            echo "Available tests: ${tests[*]}"
            exit 1
        fi
    else
        # Run all tests in order.
        for t in "${tests[@]}"; do
            log "Running test: $t"
            "$t"
            pass "$t $(phase_delta)"
        done
    fi

    local total_elapsed=$(( $(date +%s) - TS ))
    echo ""
    echo "========================================"
    echo "  All tests passed! (${total_elapsed}s total)"
    echo "========================================"
}
