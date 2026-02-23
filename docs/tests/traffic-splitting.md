# Traffic Splitting Topology E2E Tests

**Script:** `hack/e2e-test-ts.sh` | **Make target:** `make test-e2e-ts` | **Kind config:** single node

The Traffic Splitting topology creates a separate tunnel for each Service referenced
in HTTPRoute `backendRef` entries. A Cloudflare Load Balancer distributes traffic
between per-service pools using the weights from the HTTPRoute. These tests require
Cloudflare Load Balancing (paid plan).

The tests run sequentially and share state — each test builds on the resources
created by the previous one.

## test_ts_basic

Sets up the TS Gateway with 2 weighted Services and verifies the infrastructure:
per-service tunnels, per-service Deployments, per-service pools with correct
origins, a shared monitor, and a load balancer.

**Resources created:**
- `CloudflareGatewayParameters` with DNS zone, `TrafficSplitting` topology,
  `Random` steering, and health monitor
- `Gateway` with HTTP listener
- Services `svc-alpha` and `svc-beta`
- `HTTPRoute` with hostname A and 2 weighted `backendRefs` (alpha=70%, beta=30%)

**Cloudflare resources:** 2 tunnels (one per Service), 1 monitor, 2 pools
(1 origin each), 1 load balancer.

**Steps:**

1. Create `CloudflareGatewayParameters` and `Gateway`; wait for Programmed.
2. Create `svc-alpha` and `svc-beta`.
3. Create `HTTPRoute` with weighted `backendRefs`.
4. Verify tunnel `svc-alpha` exists.
5. Verify tunnel `svc-beta` exists.
6. Verify Deployment `svc-alpha` exists.
7. Verify Deployment `svc-beta` exists.
8. Verify tunnel `svc-alpha` config routes to `svc-alpha` (service field check).
9. Verify tunnel `svc-beta` config routes to `svc-beta`.
10. Verify monitor exists.
11. Verify 2 pools exist.
12. Verify pool `svc-alpha` origin points to tunnel `svc-alpha` endpoint.
13. Verify pool `svc-beta` origin points to tunnel `svc-beta` endpoint.
14. Look up zone ID for subsequent tests.
15. Verify load balancer exists for hostname A.

## test_ts_multi_hostname

Creates a second HTTPRoute with a different hostname and a different set of Services
(`svc-alpha` + new `svc-gamma`). Verifies that a new tunnel and pool are created for
`svc-gamma`, the existing `svc-alpha` tunnel is updated with both hostnames, and a
second load balancer is created.

**Resources created:**
- Service `svc-gamma`
- `HTTPRoute` with hostname B and `backendRefs` (alpha=60%, gamma=40%)

**Cloudflare resources:** 1 new tunnel (`svc-gamma`), 1 new pool (`svc-gamma`),
1 new load balancer. Total: 3 tunnels, 3 pools, 2 load balancers.

**Steps:**

1. Create `svc-gamma`.
2. Create `HTTPRoute` with hostname B (references `svc-alpha` and `svc-gamma`).
3. Verify tunnel `svc-gamma` created.
4. Verify Deployment `svc-gamma` exists.
5. Verify pool `svc-gamma` origin points to tunnel `svc-gamma` endpoint.
6. Verify 3 pools total.
7. Verify tunnel `svc-alpha` now has both hostnames (A and B).
8. Verify load balancer exists for hostname B.
9. Verify load balancer for hostname A still exists.

## test_ts_route_deletion

Deletes the first HTTPRoute (hostname A, which referenced `svc-alpha` + `svc-beta`).
Verifies that the unreferenced service (`svc-beta`) has its tunnel, Deployment, and
pool deleted, while the still-referenced service (`svc-alpha`) is updated to remove
hostname A.

**Steps:**

1. Delete `HTTPRoute` for hostname A.
2. Verify tunnel `svc-beta` deleted (no longer referenced by any route).
3. Verify Deployment `svc-beta` deleted.
4. Verify tunnel `svc-alpha` still exists.
5. Verify tunnel `svc-alpha` no longer has hostname A; hostname B preserved.
6. Verify tunnel `svc-gamma` still exists.
7. Verify pool `svc-beta` deleted.
8. Verify load balancer for hostname A deleted.
9. Verify load balancer for hostname B still exists.
10. Delete `HTTPRoute` for hostname B.
11. Verify load balancer for hostname B deleted.

## test_ts_gateway_deletion

Deletes the Gateway and verifies complete cleanup of all Cloudflare and Kubernetes
resources.

**Steps:**

1. Delete `Gateway`.
2. Verify `Gateway` Kubernetes resource deleted.
3. Verify all load balancers deleted (count = 0).
4. Verify all pools deleted (count = 0).
5. Verify monitor deleted.
6. Verify tunnel `svc-alpha` deleted.
7. Verify tunnel `svc-gamma` deleted.
8. Verify Deployment `svc-alpha` deleted.
9. Verify Deployment `svc-gamma` deleted.
10. Clean up `CloudflareGatewayParameters` and Services.
