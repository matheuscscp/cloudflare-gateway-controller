# CloudflareGatewayStatus CRD

**CloudflareGatewayStatus** (short name: `cgs`) stores the observable Cloudflare
resource state for a Gateway managed by this controller. The controller
automatically creates one CGS per managed Gateway (same name and namespace) and
keeps it up to date during reconciliation.

This object is used internally by the controller for safe cleanup when the
`CloudflareGatewayParameters` reference changes or is deleted. It is also useful
for inspecting the current state of Cloudflare resources.

The CGS only uses the `.status` subresource. There is no `.spec`.

## Example

The following example shows a CloudflareGatewayStatus for a Gateway with
two availability zones and a load balancer:

```yaml
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayStatus
metadata:
  name: my-gateway
  namespace: default
status:
  conditions:
    - type: Accepted
      status: "True"
      reason: Accepted
      message: Gateway is accepted
      lastTransitionTime: "2026-01-15T10:00:00Z"
    - type: Programmed
      status: "True"
      reason: Programmed
      message: All Deployments are available
      lastTransitionTime: "2026-01-15T10:01:00Z"
    - type: Ready
      status: "True"
      reason: ReconciliationSucceeded
      message: Reconciliation succeeded
      lastTransitionTime: "2026-01-15T10:01:00Z"
  tunnels:
    - name: gateway-abc123-az-a
      id: "f47ac10b-58cc-4372-a567-0e02b2c3d479"
      deploymentName: cloudflared-my-gateway-az-a
      secretName: cloudflared-token-my-gateway-az-a
      azName: az-a
    - name: gateway-abc123-az-b
      id: "7c9e6679-7425-40de-944b-e07fc1f90ae7"
      deploymentName: cloudflared-my-gateway-az-b
      secretName: cloudflared-token-my-gateway-az-b
      azName: az-b
  loadBalancer:
    monitorId: "550e8400-e29b-41d4-a716-446655440000"
    monitorName: gateway-abc123
    pools:
      - name: gateway-abc123-az-a
        id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
      - name: gateway-abc123-az-b
        id: "6ba7b811-9dad-11d1-80b4-00c04fd430c8"
    hostnames:
      - "app.example.com"
  dns:
    zoneName: "example.com"
    zoneID: "023e105f4ecef8ad9ca31a8372d0c353"
```

**1.** List all CloudflareGatewayStatus objects:

```shell
kubectl get cloudflaregatewaystatuses
```

**2.** Inspect a specific CGS:

```shell
kubectl -n default get cgs my-gateway -o yaml
```

## Reading a CloudflareGatewayStatus

As with all other Kubernetes config, a CloudflareGatewayStatus is identified by
`apiVersion`, `kind`, and `metadata` fields. All meaningful data is in the
`.status` subresource.

### Conditions

The CGS mirrors the same three conditions as the parent
[Gateway](Gateway.md#conditions):

- `Accepted`: Whether the Gateway passed validation.
- `Programmed`: Whether all cloudflared Deployments are available.
- `Ready`: Overall reconciliation state (custom kstatus condition).

See [Gateway conditions](Gateway.md#conditions) for the full status/reason
details.

### Tunnels

The `.status.tunnels` field lists all Cloudflare tunnels managed for this
Gateway. Each entry records the state of a single tunnel and its associated
Kubernetes resources.

Each tunnel entry has the following fields:

- `name`: Cloudflare tunnel name.
- `id`: Cloudflare tunnel UUID.
- `deploymentName`: Name of the cloudflared Deployment in the Gateway's
  namespace.
- `secretName`: Name of the tunnel token Secret in the Gateway's namespace.
- `azName` (optional): Availability zone name. Empty when not using a per-AZ
  topology.
- `serviceName` (optional): Backend service name. Empty when not using a
  per-service topology.

### Load balancer

The `.status.loadBalancer` field records the state of Cloudflare Load Balancer
resources. This field is absent when no LB topology is configured.

The load balancer entry has the following fields:

- `monitorId`: Cloudflare health monitor UUID.
- `monitorName`: Cloudflare health monitor name.
- `pools`: List of Cloudflare LB pools. Each pool has `name` and `id` fields.
- `hostnames`: List of load-balanced hostnames.

### DNS

The `.status.dns` field records the DNS zone used for CNAME and LB management.
This field is absent when DNS is not configured.

The DNS entry has the following fields:

- `zoneName`: DNS zone name (e.g. `example.com`).
- `zoneID`: Cloudflare zone UUID.
