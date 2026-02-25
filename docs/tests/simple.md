# Simple Topology E2E Tests

**Script:** `hack/e2e-test.sh` | **Make target:** `make test-e2e` | **Kind config:** single node

The simple topology uses a single Cloudflare tunnel with optional DNS CNAME records.
No load balancer is involved. These tests run on Cloudflare's free plan.

## test_gateway_lifecycle

Full lifecycle of a Gateway with a single HTTPRoute: creation, verification, and
deletion of all Kubernetes and Cloudflare resources.

**Resources created:**
- `CloudflareGatewayParameters` with DNS zone config
- `Gateway` with HTTP listener
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
- `Gateway` with HTTP listener
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
11. Delete `route-b`.
12. Verify hostname B removed from tunnel config.
13. Verify CNAME B deleted.
14. Delete `Gateway`; verify tunnel deleted.
15. Clean up `CloudflareGatewayParameters` and Services.

## test_path_matching

HTTPRoute with path-based matching rules. Two different Services are routed
by path prefix (`/api` and `/web`), verifying that cloudflared receives
path-based ingress entries.

**Resources created:**
- `Gateway` with no DNS config (bare Secret as parametersRef)
- 2 Services and 1 HTTPRoute with 2 path-based rules

**Cloudflare resources:** 1 tunnel (no DNS CNAMEs — Gateway has no DNS config).

**Steps:**

1. Create `Gateway` without DNS config; wait for Programmed.
2. Create 2 Services (`api-svc`, `web-svc`).
3. Create `HTTPRoute` with 2 rules: `/api` to `api-svc`, `/web` to `web-svc`.
4. Verify tunnel config has an entry for the hostname with `/api` path.
5. Verify tunnel config has an entry for the hostname with `/web` path.
6. Delete `HTTPRoute` and `Gateway`.
7. Clean up Services.

## test_no_dns

HTTPRoute on a Gateway without DNS configuration. Verifies that the tunnel is
configured correctly but no DNS CNAME record is created.

**Resources created:**
- `Gateway` with no DNS config (bare Secret as parametersRef)
- `Service` and `HTTPRoute`

**Cloudflare resources:** 1 tunnel, explicitly no DNS CNAME.

**Steps:**

1. Create `Gateway` without DNS config; wait for Programmed.
2. Create `Service` and `HTTPRoute`.
3. Verify tunnel config has the hostname.
4. Verify no DNS CNAME record exists (checked multiple times with sleep).
5. Delete `HTTPRoute` and `Gateway`.
6. Clean up `Service`.

## test_deployment_patches

CloudflareGatewayParameters with JSON Patch operations applied to the cloudflared
Deployment. Verifies that the patch is applied correctly.

**Resources created:**
- `CloudflareGatewayParameters` with a patch that adds label `e2e-patch=applied`
- `Gateway` referencing the patched parameters

**Cloudflare resources:** 1 tunnel.

**Steps:**

1. Create `CloudflareGatewayParameters` with deployment patch (`op: add` label).
2. Create `Gateway`; wait for Programmed.
3. Verify cloudflared Deployment has label `e2e-patch=applied` on its pod template.
4. Delete `Gateway`; verify deleted.
5. Clean up `CloudflareGatewayParameters`.

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

Removes the DNS zone configuration from an existing `CloudflareGatewayParameters`.
Verifies that DNS CNAME records are deleted but tunnel ingress config is preserved.

**Resources created:**
- `CloudflareGatewayParameters` with DNS zone config
- `Gateway`, `Service`, and `HTTPRoute`

**Cloudflare resources:** 1 tunnel, 1 DNS CNAME (CNAME deleted after config removal).

**Steps:**

1. Create parameters with DNS zone, `Gateway`, `Service`, `HTTPRoute`;
   wait for Programmed.
2. Verify DNS CNAME exists.
3. Update `CloudflareGatewayParameters` to remove `dns.zone` config.
4. Verify DNS CNAME deleted.
5. Verify tunnel config still has the hostname (ingress unaffected by DNS removal).
6. Delete `HTTPRoute` and `Gateway`.
7. Clean up `CloudflareGatewayParameters` and `Service`.

## test_multiple_listeners_rejected

Attempts to create a Gateway with two listeners. Verifies that the controller
rejects it with `Accepted=False` and reason `ListenersNotValid`.

**Resources created:**
- `Gateway` with 2 listeners (http:80 and http:8080)

**Cloudflare resources:** none (Gateway rejected before reconciliation).

**Steps:**

1. Create `Gateway` with 2 listeners.
2. Verify `Accepted` condition is `False`.
3. Verify `Accepted` reason is `ListenersNotValid`.
4. Delete `Gateway`; verify deleted.

## test_cluster_recreation

Proves that deterministic Cloudflare resource naming enables cluster recreation
without leaking resources. A reborn cluster with the same `clusterName` adopts
existing Cloudflare resources (tunnel, DNS CNAME) instead of creating duplicates.

**Must run last** because it destroys and recreates the kind cluster.

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
