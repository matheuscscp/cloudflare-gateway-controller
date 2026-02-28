# CloudflareGatewayStatus CRD

**CloudflareGatewayStatus** (short name: `cgs`) stores the observable Cloudflare
resource state for a Gateway managed by this controller. The controller
automatically creates one CGS per managed Gateway (same name and namespace) and
keeps it up to date during reconciliation.

This object is purely observational — the controller does not depend on it for
reconciliation or cleanup. It is useful for inspecting the current state of
Cloudflare resources managed for a Gateway.

The CGS only uses the `.status` subresource. There is no `.spec`.

## Example

The following example shows a CloudflareGatewayStatus for a Gateway with
a single tunnel and DNS configuration:

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
      message: Deployment is available
      lastTransitionTime: "2026-01-15T10:01:00Z"
    - type: DNSManagement
      status: "True"
      reason: Managed
      message: |-
        Allowed zones:
        - example.com
      lastTransitionTime: "2026-01-15T10:01:00Z"
    - type: Ready
      status: "True"
      reason: ReconciliationSucceeded
      message: Reconciliation succeeded
      lastTransitionTime: "2026-01-15T10:01:00Z"
  tunnel:
    name: gateway-abc123
    id: "f47ac10b-58cc-4372-a567-0e02b2c3d479"
    deploymentName: cloudflared-my-gateway
    secretName: cloudflared-token-my-gateway
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

The CGS mirrors the same conditions as the parent
[Gateway](Gateway.md#conditions):

- `Accepted`: Whether the Gateway passed validation.
- `Programmed`: Whether the cloudflared Deployment is available.
- `DNSManagement`: Whether DNS management is configured.
- `Ready`: Overall reconciliation state (custom kstatus condition).

See [Gateway conditions](Gateway.md#conditions) for the full status/reason
details.

### Tunnel

The `.status.tunnel` field records the Cloudflare tunnel managed for this
Gateway and its associated Kubernetes resources.

The tunnel entry has the following fields:

- `name`: Cloudflare tunnel name.
- `id`: Cloudflare tunnel UUID.
- `deploymentName`: Name of the cloudflared Deployment in the Gateway's
  namespace.
- `secretName`: Name of the tunnel token Secret in the Gateway's namespace.
