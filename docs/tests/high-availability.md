# High Availability Topology E2E Tests

**Script:** `hack/e2e-test-ha.sh` | **Make target:** `make test-e2e-ha` | **Kind config:** multi-node (2 AZ workers)

The High Availability topology deploys one tunnel per availability zone. All tunnels
carry a full copy of every ingress rule, and a Cloudflare Load Balancer steers traffic
between zones. These tests require Cloudflare Load Balancing (paid plan).

The tests run sequentially and share state — each test builds on the resources
created by the previous one.

## test_ha_basic

Sets up the HA Gateway with 2 availability zones and verifies the infrastructure:
tunnels, Deployments with node affinity, monitor, and pools.

**Resources created:**
- `CloudflareGatewayParameters` with DNS zone, 2 AZs (`az-a`, `az-b`),
  `HighAvailability` topology, `Geographic` steering, and health monitor
- `Gateway` with HTTP listener

**Cloudflare resources:** 2 tunnels, 1 monitor, 2 pools (1 origin each).

**Steps:**

1. Create `CloudflareGatewayParameters` and `Gateway`; wait for Programmed.
2. Verify tunnel `az-a` exists.
3. Verify tunnel `az-b` exists.
4. Verify Deployment `az-a` has node affinity to zone `az-a`.
5. Verify Deployment `az-b` has node affinity to zone `az-b`.
6. Verify monitor exists.
7. Verify 2 pools exist.
8. Verify pool `az-a` origin points to tunnel `az-a` endpoint
   (`<tunnelID>.cfargotunnel.com`).
9. Verify pool `az-b` origin points to tunnel `az-b` endpoint.
10. Look up zone ID for subsequent tests.

## test_ha_httproute

Creates the first HTTPRoute and verifies that both AZ tunnels receive the hostname
in their ingress config (full redundancy) and that a load balancer is created.

**Resources created:**
- `Service` and `HTTPRoute` with hostname A

**Cloudflare resources:** both tunnel configs updated, 1 load balancer created.

**Steps:**

1. Create `Service` and `HTTPRoute` with hostname A.
2. Verify tunnel `az-a` config has hostname A.
3. Verify tunnel `az-b` config has hostname A.
4. Verify load balancer exists for hostname A.

## test_ha_multi_hostname

Creates a second HTTPRoute with a different hostname. Verifies that both AZ tunnels
receive both hostnames and that a second load balancer is created.

**Resources created:**
- `Service` and `HTTPRoute` with hostname B

**Cloudflare resources:** both tunnel configs updated with hostname B, 2 load
balancers total.

**Steps:**

1. Create `Service` and `HTTPRoute` with hostname B.
2. Verify tunnel `az-a` config has hostname B.
3. Verify tunnel `az-b` config has hostname B.
4. Verify load balancer exists for hostname B.
5. Verify load balancer for hostname A still exists.

## test_ha_route_deletion

Deletes the first HTTPRoute. Verifies that hostname A is removed from both tunnel
configs and its load balancer is deleted, while hostname B and its load balancer
are preserved.

**Steps:**

1. Delete `HTTPRoute` for hostname A.
2. Verify tunnel `az-a` no longer has hostname A.
3. Verify tunnel `az-b` no longer has hostname A.
4. Verify load balancer for hostname A deleted.
5. Verify load balancer for hostname B still exists.
6. Delete `HTTPRoute` for hostname B.
7. Verify load balancer for hostname B deleted.

## test_ha_gateway_deletion

Deletes the Gateway and verifies complete cleanup of all Cloudflare and Kubernetes
resources.

**Steps:**

1. Delete `Gateway`.
2. Verify `Gateway` Kubernetes resource deleted.
3. Verify all load balancers deleted (count = 0).
4. Verify all pools deleted (count = 0).
5. Verify monitor deleted.
6. Verify tunnel `az-a` deleted.
7. Verify tunnel `az-b` deleted.
8. Verify Deployment `az-a` deleted.
9. Verify Deployment `az-b` deleted.
10. Clean up `CloudflareGatewayParameters` and Services.

## test_simple_gw_lb_coexistence

Creates an HA gateway with a load balancer alongside a simple topology gateway in the
same DNS zone. Verifies that the simple gateway's full lifecycle (creation, route
deletion, gateway deletion/finalization) does not interfere with the HA gateway's
load balancer DNS.

**Resources created:**
- HA: `CloudflareGatewayParameters` (HA topology, 2 AZs), `Gateway`, `Service`,
  `HTTPRoute` with HA hostname
- Simple: `CloudflareGatewayParameters` (no LB), `Gateway`, `Service`, `HTTPRoute`
  with simple hostname

**Cloudflare resources:** HA tunnels, monitor, pools, load balancer; simple tunnel,
DNS CNAME.

**Steps:**

1. Create HA `CloudflareGatewayParameters`, `Gateway`, `Service`, and `HTTPRoute`;
   wait for Programmed.
2. Verify HA load balancer exists for HA hostname.
3. Create simple `CloudflareGatewayParameters`, `Gateway`, `Service`, and `HTTPRoute`
   in the same zone; wait for Programmed.
4. Verify simple DNS CNAME exists.
5. Verify HA LB hostname still exists after simple gateway creation.
6. Delete simple `HTTPRoute`.
7. Verify simple DNS CNAME removed.
8. Verify HA LB hostname still exists after simple route deletion.
9. Delete simple `Gateway` (triggers finalization).
10. Verify simple `Gateway` fully deleted.
11. Verify simple tunnel deleted.
12. Verify HA LB hostname still exists after simple gateway finalization.
13. Clean up HA gateway and all resources.
