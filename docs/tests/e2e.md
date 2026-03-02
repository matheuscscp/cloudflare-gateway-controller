# E2E Tests

**Script:** `hack/e2e-test.sh` | **Make target:** `make test-e2e` | **Kind config:** single node

The controller uses a single Cloudflare tunnel with optional DNS CNAME records.
The sidecar reverse proxy is enabled by default in all tests, so cloudflared
forwards all traffic through the sidecar for hostname/path-based routing.

## test_gateway_lifecycle

Full lifecycle of a Gateway with a single HTTPRoute: creation, verification, and
deletion of all Kubernetes and Cloudflare resources.

**Resources created:**
- `CloudflareGatewayParameters` with DNS zone config
- `Gateway` with HTTPS listener
- `Service` and `HTTPRoute` with one hostname

**Cloudflare resources:** 1 tunnel, 1 DNS CNAME record.

**Steps:**

1. Create `CloudflareGatewayParameters` with DNS zone configuration.
2. Create `Gateway`; wait for Programmed condition.
3. Verify tunnel exists in Cloudflare.
4. Create `Service` and `HTTPRoute`.
5. Verify tunnel ingress config includes the HTTPRoute hostname.
6. Verify DNS CNAME record exists for the hostname.
7. Delete `HTTPRoute`.
8. Verify tunnel ingress config no longer has the hostname.
9. Verify DNS CNAME record deleted.
10. Delete `Gateway`.
11. Verify `Gateway` Kubernetes resource deleted.
12. Verify tunnel deleted from Cloudflare.
13. Clean up `CloudflareGatewayParameters` and `Service`.

## test_multi_routes

Multiple HTTPRoutes on the same Gateway with different hostnames and Services.
Tests partial route deletion — removing one route should clean up only its hostname
while preserving the other.

**Resources created:**
- `CloudflareGatewayParameters` with DNS zone config
- `Gateway` with HTTPS listener
- 2 Services and 2 HTTPRoutes with different hostnames

**Cloudflare resources:** 1 tunnel (shared), 2 DNS CNAME records.

**Steps:**

1. Create `CloudflareGatewayParameters` and `Gateway`; wait for Programmed.
2. Create 2 Services.
3. Create `route-a` with hostname A.
4. Verify tunnel config has hostname A.
5. Create `route-b` with hostname B.
6. Verify tunnel config has both hostnames.
7. Verify both DNS CNAMEs exist.
8. Delete `route-a`.
9. Verify hostname A removed from tunnel config; hostname B still present.
10. Verify CNAME A deleted; CNAME B still exists.
11. Delete `route-b` and `Gateway`; verify tunnel deleted.
12. Clean up `CloudflareGatewayParameters` and Services.

## test_dns_default_all_zones

Gateway with a bare Secret (no `CloudflareGatewayParameters`) verifies that DNS
management is enabled by default for all hostnames. Each hostname's zone is
resolved dynamically via the Cloudflare API.

**Resources created:**
- `Gateway` with bare Secret as parametersRef (no CGP)
- `Service` and `HTTPRoute` with one hostname

**Cloudflare resources:** 1 tunnel, 1 DNS CNAME record (created via all-zones mode).

**Steps:**

1. Create `Gateway` with bare Secret; wait for Programmed.
2. Verify `DNSManagement` condition is `True/Enabled` with message "All hostnames".
3. Create `Service` and `HTTPRoute`.
4. Verify DNS CNAME record created for the hostname (all-zones mode).
5. Verify HTTPRoute `DNSRecordsApplied` condition has "Applied hostnames" and no
   "Skipped" section.
6. Delete `HTTPRoute` and `Gateway`; verify Gateway deleted.
7. Clean up `Service`.

## test_no_dns

HTTPRoute on a Gateway with DNS explicitly disabled via an empty zones list.
Verifies that the tunnel is configured correctly but no DNS CNAME record is
created.

**Resources created:**
- `CloudflareGatewayParameters` with `dns: { zones: [] }` (DNS disabled)
- `Gateway` referencing the parameters
- `Service` and `HTTPRoute`

**Cloudflare resources:** 1 tunnel, explicitly no DNS CNAME.

**Steps:**

1. Create `CloudflareGatewayParameters` with empty zones list; create `Gateway`;
   wait for Programmed.
2. Create `Service` and `HTTPRoute`.
3. Verify tunnel config has the hostname.
4. Verify no DNS CNAME record exists (checked multiple times with sleep).
5. Delete `HTTPRoute` and `Gateway`.
6. Clean up `CloudflareGatewayParameters` and `Service`.

## test_disabled_reconciliation

Marks a Gateway with the `cloudflare-gateway-controller.io/reconcile=disabled`
annotation before deletion. Verifies that the controller skips finalization,
leaving Kubernetes and Cloudflare resources orphaned.

**Resources created:**
- `CloudflareGatewayParameters` with DNS zone config
- `Gateway`, `Service`, and `HTTPRoute`

**Cloudflare resources:** 1 tunnel, 1 DNS CNAME (both orphaned after deletion).

**Steps:**

1. Create parameters, `Gateway`, `Service`, and `HTTPRoute`; wait for Programmed.
2. Verify DNS CNAME exists.
3. Annotate `Gateway` with `reconcile=disabled`.
4. Delete `HTTPRoute` and `Gateway`.
5. Verify `Gateway` Kubernetes resource deleted.
6. Verify cloudflared Deployment still exists (orphaned).
7. Verify tunnel still exists in Cloudflare (orphaned).
8. Verify DNS CNAME still exists (orphaned).
9. Manual cleanup: delete Deployment, Secret, DNS CNAME, tunnel connections,
   and tunnel via `cfgwctl`.
10. Clean up `CloudflareGatewayParameters` and `Service`.

## test_dns_config_removal

Disables DNS management by updating `CloudflareGatewayParameters` to use an empty
zones list (`dns: { zones: [] }`). Verifies that DNS CNAME records are deleted but
tunnel ingress config is preserved.

**Resources created:**
- `CloudflareGatewayParameters` with DNS zone config
- `Gateway`, `Service`, and `HTTPRoute`

**Cloudflare resources:** 1 tunnel, 1 DNS CNAME (CNAME deleted after config change).

**Steps:**

1. Create parameters with DNS zone, `Gateway`, `Service`, `HTTPRoute`;
   wait for Programmed.
2. Verify DNS CNAME exists.
3. Update `CloudflareGatewayParameters` to set `dns: { zones: [] }` (disable DNS).
4. Verify DNS CNAME deleted.
5. Verify tunnel config still has the hostname (ingress unaffected by DNS change).
6. Delete `HTTPRoute` and `Gateway`.
7. Clean up `CloudflareGatewayParameters` and `Service`.

## test_multi_zone_dns

Multi-zone DNS management with zone additions and removals. Requires
`TEST_ZONE_NAME_2` and `TEST_ZONE_NAME_3`; skips if unset.

**Resources created:**
- `CloudflareGatewayParameters` with 2 DNS zones (initially)
- `Gateway` with HTTPS listener
- `Service` and `HTTPRoute` with hostnames across 3 zones plus one unmanaged hostname

**Cloudflare resources:** 1 tunnel, DNS CNAMEs in multiple zones.

**Steps:**

1. Create `CloudflareGatewayParameters` with zones 1 and 2; create `Gateway`;
   wait for Programmed.
2. Create `Service` and `HTTPRoute` with hostnames in zones 1, 2, 3, and an
   unmanaged domain.
3. Verify CNAMEs created in zones 1 and 2; zone 3 hostname correctly absent.
4. Verify `DNSRecordsApplied` condition reports skipped hostnames (zone 3 and
   unmanaged).
5. Update config: remove zone 1, add zone 3.
6. Verify zone 1 CNAMEs deleted, zone 2 CNAME intact, zone 3 CNAME created.
7. Update config: add zone 1 back (all 3 zones).
8. Verify zone 1 CNAMEs re-created; all 3 zones have their CNAMEs.
9. Delete `HTTPRoute`, `Gateway`; clean up resources.

## test_multi_gateway_overlapping_zones

Multiple Gateways with overlapping DNS zone configurations sharing the same
Cloudflare account. Requires `TEST_ZONE_NAME_2` and `TEST_ZONE_NAME_3`; skips
if unset.

**Resources created:**
- 3 `CloudflareGatewayParameters` with overlapping zone configs:
  - gw-a: zones 1, 2, 3
  - gw-b: zones 1, 2
  - gw-c: zones 2, 3
- 3 Gateways, 1 Service, 3 HTTPRoutes with unique hostnames per gateway

**Cloudflare resources:** 3 tunnels, 7 DNS CNAMEs total (3 + 2 + 2).

**Steps:**

1. Create 3 `CloudflareGatewayParameters` and 3 Gateways; wait for all Programmed.
2. Create `Service` and 3 HTTPRoutes (gw-a: 3 hostnames, gw-b: 2, gw-c: 2).
3. Verify all 7 CNAMEs exist across the 3 tunnels.
4. Delete gw-b (zones 1, 2); verify gw-b CNAMEs removed.
5. Verify gw-a and gw-c CNAMEs unaffected by gw-b deletion.
6. Remove zone 3 from gw-a config; verify gw-a zone 3 CNAME removed.
7. Verify gw-a zones 1, 2 CNAMEs intact.
8. Verify gw-c CNAMEs completely unaffected by gw-a's zone change.
9. Clean up all resources.

## test_cluster_recreation

Proves that deterministic Cloudflare resource naming enables cluster recreation
without leaking resources. A reborn cluster with the same `clusterName` adopts
existing Cloudflare resources (tunnel, DNS CNAME) instead of creating duplicates.

**Resources created:**
- `CloudflareGatewayParameters` with DNS zone config
- `Gateway`, `Service`, and `HTTPRoute`

**Cloudflare resources:** 1 tunnel, 1 DNS CNAME (adopted after recreation).

**Steps:**

1. Create `CloudflareGatewayParameters`, `Gateway`, `Service`, and `HTTPRoute`;
   wait for Programmed.
2. Record the Cloudflare tunnel ID.
3. Verify DNS CNAME exists.
4. Delete the kind cluster.
5. Recreate the kind cluster with the same name.
6. Reinstall controller, recreate namespace and GatewayClass.
7. Recreate the same `Gateway`, `Service`, and `HTTPRoute`.
8. Wait for Programmed.
9. Verify tunnel ID is **unchanged** (same as before — adopted, not recreated).
10. Verify tunnel config includes the hostname.
11. Verify DNS CNAME still exists.
12. Delete `HTTPRoute` and `Gateway`.
13. Verify tunnel deleted by finalizer.
14. Verify DNS CNAME deleted.
15. Clean up `CloudflareGatewayParameters` and `Service`.

## test_load_balancing

Sends real HTTP traffic through the Cloudflare tunnel and verifies that the
sidecar proxy distributes requests evenly across backend pods via kube-proxy.
Uses `cfgwctl test serve` as the backend and `cfgwctl test load` as the
load generator.

**Resources created:**
- 10-replica `Deployment` running `cfgwctl test serve` (selector `app=lb-test`)
- `Service` `lb-backend` (port 80 → targetPort 8080)
- `Gateway` with bare Secret (no CGP — sidecar enabled by default)
- `HTTPRoute` with one hostname

**Cloudflare resources:** 1 tunnel, 1 DNS CNAME record.

**Pass criteria:**
- All 1000 requests return 2xx (zero 5xx).
- Every pod receives at least 1 request.
- Coefficient of variation (CV = stddev / mean) of per-pod counts ≤ 0.5.
- Every response's `host` field matches the public hostname (verifies correct
  Host header forwarding through cloudflared and the sidecar).

**Steps:**

1. Deploy 10-replica test server and `Service`.
2. Wait for rollout.
3. Create `Gateway` (bare Secret); wait for Programmed.
4. Create `HTTPRoute`.
5. Wait for HTTPS endpoint reachable.
6. Run `cfgwctl test load` with 1000 requests, concurrency 10, pod
   distribution check (`--namespace`, `--label-selector`, `--max-cv 0.5`),
   and host header verification (`--hostname`).
7. Delete `HTTPRoute`, `Gateway` (wait for deletion), `Deployment`, `Service`.

## test_sidecar_disabled

Verifies end-to-end HTTP traffic through the tunnel when the sidecar reverse
proxy is explicitly disabled. Cloudflared connects directly to backend Services.

When the sidecar is disabled, traffic splitting (weighted backendRefs) is not
available, and cloudflared's persistent connections prevent effective kube-proxy
load balancing across pods. Multiple backendRefs per rule are rejected.

**Resources created:**
- 1-replica `Deployment` running `cfgwctl test serve` (selector `app=sd-test`)
- `Service` `sd-backend` (port 80 → targetPort 8080)
- `CloudflareGatewayParameters` with `tunnel.sidecar.enabled: false`
- `Gateway` referencing the parameters
- `HTTPRoute` with one hostname

**Cloudflare resources:** 1 tunnel, 1 DNS CNAME record.

**Pass criteria:**
- Sidecar condition is `status=False, reason=Disabled`.
- 10 requests return 2xx (zero 5xx).
- Every response's `host` field matches the public hostname.

**Steps:**

1. Deploy 1-replica test server and `Service`.
2. Create `CloudflareGatewayParameters` with sidecar disabled.
3. Create `Gateway`; wait for Programmed.
4. Verify Sidecar condition is `False/Disabled`.
5. Create `HTTPRoute`.
6. Wait for HTTPS endpoint reachable.
7. Run `cfgwctl test load` with 10 requests, concurrency 1, and host header
   verification (`--hostname`).
8. Delete `HTTPRoute`, `Gateway` (wait for deletion), CGP, `Deployment`, `Service`.

## test_traffic_splitting

Sends real HTTP traffic through the Cloudflare tunnel and verifies that the
sidecar proxy distributes requests across two backend Services according to
their weights (80/20). Uses `cfgwctl test serve` as the backend and
`cfgwctl test load` as the load generator.

**Resources created:**
- 1-replica `Deployment` `ts-svc-a` running `cfgwctl test serve` (selector `app=ts-svc-a`)
- 1-replica `Deployment` `ts-svc-b` running `cfgwctl test serve` (selector `app=ts-svc-b`)
- `Service` `ts-svc-a` and `ts-svc-b` (port 80 → targetPort 8080)
- `Gateway` with bare Secret (no CGP — sidecar enabled by default)
- `HTTPRoute` with one rule, 2 backendRefs: `ts-svc-a` weight 80, `ts-svc-b` weight 20

**Cloudflare resources:** 1 tunnel, 1 DNS CNAME record.

**Pass criteria:**
- All 200 requests return 2xx (zero 5xx).
- Both backends received at least 1 request.
- Each backend's actual share is within ±15% of its expected share (80%/20%).

**Steps:**

1. Deploy 2 test servers (1 replica each) and 2 Services.
2. Wait for rollouts.
3. Create `Gateway` (bare Secret); wait for Programmed.
4. Create `HTTPRoute` with weighted backendRefs (80/20).
5. Wait for HTTPS endpoint reachable.
6. Run `cfgwctl test load` with 200 requests, concurrency 5, and weighted
   backend distribution check (`--backend app=ts-svc-a:80 --backend app=ts-svc-b:20
   --tolerance 0.15`).
7. Delete `HTTPRoute`, `Gateway` (wait for deletion), Deployments, Services.

## test_session_persistence

Sends real HTTP traffic through the Cloudflare tunnel and verifies that the
sidecar proxy correctly implements cookie-based session persistence. With two
equally-weighted backends, all requests after the initial one (which sets the
cookie) must go to the same backend pod. Also verifies that the public hostname
is correctly forwarded through cloudflared and the sidecar to the backend.

**Resources created:**
- 1-replica `Deployment` `sp-svc-a` running `cfgwctl test serve` (selector `app=sp-svc-a`)
- 1-replica `Deployment` `sp-svc-b` running `cfgwctl test serve` (selector `app=sp-svc-b`)
- `Service` `sp-svc-a` and `sp-svc-b` (port 80 → targetPort 8080)
- `Gateway` with bare Secret (no CGP — sidecar enabled by default)
- `HTTPRoute` with one rule, 2 backendRefs (weight 50/50) and `sessionPersistence: { type: Cookie }`

**Cloudflare resources:** 1 tunnel, 1 DNS CNAME record.

**Pass criteria:**
- Initial request returns 2xx and sets a `cgw-session` cookie.
- All 50 follow-up requests (with cookie) return 2xx and the same `pod` value.
- The `host` field in every response matches the public hostname.

**Steps:**

1. Deploy 2 test servers (1 replica each) and 2 Services.
2. Wait for rollouts.
3. Create `Gateway` (bare Secret); wait for Programmed.
4. Create `HTTPRoute` with session persistence and weighted backendRefs (50/50).
5. Wait for HTTPS endpoint reachable.
6. Run `cfgwctl test session` with 50 requests, verifying cookie affinity and
   host header forwarding (`--hostname`).
7. Delete `HTTPRoute`, `Gateway` (wait for deletion), Deployments, Services.

## test_vpa_autoscaling

Vertical Pod Autoscaler (VPA) lifecycle: creation with per-container autoscaling
policies and cleanup when autoscaling is disabled. Installs the VPA CRD from
upstream before testing.

**Resources created:**
- VPA CRD (cluster-scoped, installed from upstream)
- `CloudflareGatewayParameters` with autoscaling enabled for cloudflared
  (minAllowed, maxAllowed, controlledResources, controlledValues) and sidecar
  (controlledValues)
- `Gateway` referencing the parameters

**Cloudflare resources:** 1 tunnel.

**Steps:**

1. Install VPA CRD from upstream; verify CRD is available.
2. Create `CloudflareGatewayParameters` with autoscaling enabled for both
   cloudflared and sidecar containers.
3. Create `Gateway`; wait for Programmed.
4. Verify VPA `gateway-vpa-gw-primary` exists.
5. Verify VPA `updateMode` is `InPlaceOrRecreate`.
6. Verify VPA `targetRef` points to the Deployment.
7. Verify container policies: wildcard `*` set to `Off`, `cloudflared` set to
   `Auto`, `sidecar` set to `Auto`.
8. Verify cloudflared `minAllowed`/`maxAllowed` values.
9. Verify cloudflared `controlledValues` is `RequestsAndLimits`.
10. Verify sidecar `controlledValues` is `RequestsOnly`.
11. Disable autoscaling by updating CGP (remove tunnel config).
12. Verify VPA is deleted (cleanup).
13. Delete `Gateway`; verify deleted.
14. Clean up `CloudflareGatewayParameters`.
