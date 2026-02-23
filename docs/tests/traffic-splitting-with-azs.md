# Traffic Splitting with AZs Topology E2E Tests

**Script:** `hack/e2e-test-ts-az.sh` | **Make target:** `make test-e2e-ts-az` | **Kind config:** multi-node (2 AZ workers)

The Traffic Splitting with AZs topology combines per-service tunnels with per-AZ
redundancy. Each Service gets a pool with one origin per AZ, resulting in
*S* x *A* tunnels. These tests require Cloudflare Load Balancing (paid plan).

The tests run sequentially and share state — each test builds on the resources
created by the previous one.

## test_tsaz_basic

Sets up the TS+AZ Gateway with 2 Services and 2 AZs, verifying the full
infrastructure: 4 tunnels, 4 Deployments with node affinity, 2 pools with
2 origins each, a shared monitor, and a load balancer.

**Resources created:**
- `CloudflareGatewayParameters` with DNS zone, 2 AZs (`az-a`, `az-b`),
  `TrafficSplitting` topology, `Random` steering, and health monitor
- `Gateway` with HTTP listener
- Services `svc-alpha` and `svc-beta`
- `HTTPRoute` with hostname A and 2 weighted `backendRefs` (alpha=70%, beta=30%)

**Cloudflare resources:** 4 tunnels (2 services x 2 AZs), 1 monitor, 2 pools
(2 origins each), 1 load balancer.

**Steps:**

1. Create `CloudflareGatewayParameters` and `Gateway`; wait for Programmed.
2. Create `svc-alpha` and `svc-beta`.
3. Create `HTTPRoute` with weighted `backendRefs`.
4. For each service x AZ combination (4 total):
   - Verify tunnel exists.
5. For each service x AZ combination:
   - Verify Deployment exists.
   - Verify Deployment node affinity matches the AZ.
6. Verify tunnel `svc-alpha-az-a` config routes to `svc-alpha` (service field check).
7. Verify tunnel `svc-beta-az-a` config routes to `svc-beta`.
8. Verify monitor exists.
9. Verify 2 pools exist.
10. Verify pool `svc-alpha` has 2 origins.
11. Verify pool `svc-beta` has 2 origins.
12. Verify pool `svc-alpha` origins reference both AZ tunnel endpoints.
13. Look up zone ID for subsequent tests.
14. Verify load balancer exists for hostname A.

## test_tsaz_multi_hostname

Creates a second HTTPRoute with a different hostname referencing only `svc-alpha`.
Verifies that no new tunnels or pools are created (svc-alpha already has tunnels
in both AZs), the `svc-alpha` tunnels are updated with both hostnames, and a
second load balancer is created.

**Resources created:**
- `HTTPRoute` with hostname B and `backendRef` to `svc-alpha` only

**Cloudflare resources:** no new tunnels or pools. 1 new load balancer.
Total: 4 tunnels, 2 pools, 2 load balancers.

**Steps:**

1. Create `HTTPRoute` with hostname B (references `svc-alpha` only).
2. Verify tunnel `svc-alpha-az-a` now has both hostnames (A and B).
3. Verify tunnel `svc-alpha-az-a` didn't lose hostname A.
4. Verify 2 pools still exist (no new pools).
5. Verify load balancer exists for hostname B.
6. Verify load balancer for hostname A still exists.

## test_tsaz_route_deletion

Deletes the first HTTPRoute (hostname A, which referenced `svc-alpha` + `svc-beta`).
Verifies that the unreferenced service (`svc-beta`) has all its tunnels (both AZs)
and Deployments deleted, while the still-referenced service (`svc-alpha`) tunnels
are updated to remove hostname A.

**Steps:**

1. Delete `HTTPRoute` for hostname A.
2. Verify tunnel `svc-beta-az-a` deleted.
3. Verify tunnel `svc-beta-az-b` deleted.
4. Verify Deployment `svc-beta-az-a` deleted.
5. Verify Deployment `svc-beta-az-b` deleted.
6. Verify tunnel `svc-alpha-az-a` still exists.
7. Verify tunnel `svc-alpha-az-b` still exists.
8. Verify tunnel `svc-alpha-az-a` no longer has hostname A; hostname B preserved.
9. Verify load balancer for hostname A deleted.
10. Verify load balancer for hostname B still exists.
11. Verify pool `svc-beta` deleted.
12. Delete `HTTPRoute` for hostname B.
13. Verify load balancer for hostname B deleted.

## test_tsaz_gateway_deletion

Deletes the Gateway and verifies complete cleanup of all Cloudflare and Kubernetes
resources.

**Steps:**

1. Delete `Gateway`.
2. Verify `Gateway` Kubernetes resource deleted.
3. Verify all load balancers deleted (count = 0).
4. Verify all pools deleted (count = 0).
5. Verify monitor deleted.
6. Verify tunnel `svc-alpha-az-a` deleted.
7. Verify tunnel `svc-alpha-az-b` deleted.
8. Verify Deployment `svc-alpha-az-a` deleted.
9. Verify Deployment `svc-alpha-az-b` deleted.
10. Clean up `CloudflareGatewayParameters` and Services.
