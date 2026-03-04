# Gateway

The controller reconciles **Gateway** resources whose GatewayClass has
`.spec.controllerName: cloudflare-gateway-controller.io/controller`.

This document covers the annotations recognized by this controller and the
conditions it sets. For the full Gateway spec, see the
[Gateway API documentation](https://gateway-api.sigs.k8s.io/api-types/gateway/).

## Example

The following example shows a Gateway that references a
[CloudflareGatewayParameters](CloudflareGatewayParameters.md) for its
Cloudflare configuration:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: my-gateway
  namespace: default
  annotations:
    cloudflare-gateway-controller.io/reconcile: "enabled"
    cloudflare-gateway-controller.io/reconcileEvery: "10m"
spec:
  gatewayClassName: cloudflare
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: my-params
  listeners:
    - name: https
      protocol: HTTPS
      port: 443
```

## Writing a Gateway spec

The Gateway must have exactly one listener with `protocol: HTTPS` and
`port: 443`. The `tls` field and hostnames on listeners are not supported.
Cloudflare handles TLS termination; hostnames are configured on
[HTTPRoute](HTTPRoute.md) resources instead.

The `.spec.addresses` field is not supported and must not be set.

### Parameters reference

The `.spec.infrastructure.parametersRef` field references a
[CloudflareGatewayParameters](CloudflareGatewayParameters.md) that provides
Cloudflare-specific configuration (credentials, DNS, tunnel):

```yaml
spec:
  infrastructure:
    parametersRef:
      group: cloudflare-gateway-controller.io
      kind: CloudflareGatewayParameters
      name: my-params
```

### Credentials resolution

The controller resolves Cloudflare API credentials in the following order:

1. If `.spec.infrastructure.parametersRef` references a
   `CloudflareGatewayParameters` that has `.spec.secretRef`, credentials are
   read from that Secret.
2. Otherwise, credentials are read from the [GatewayClass](GatewayClass.md)
   `parametersRef` Secret.

If none of the above is set, the Gateway is rejected with
`Accepted=False/InvalidParameters`.

### Reconciliation control

The `cloudflare-gateway-controller.io/reconcile` annotation controls whether
reconciliation is enabled for the Gateway.

The default value is `enabled`. Set to `disabled` to pause reconciliation:

```yaml
metadata:
  annotations:
    cloudflare-gateway-controller.io/reconcile: "disabled"
```

When reconciliation is disabled and the Gateway is deleted, the controller
removes owner references from managed Kubernetes resources (Deployments,
Secrets, ConfigMaps, ServiceAccounts, Roles, RoleBindings) instead of
deleting Cloudflare resources, leaving tunnels and DNS records intact.

### Reconciliation interval

The `cloudflare-gateway-controller.io/reconcileEvery` annotation overrides the
default periodic reconciliation interval used for drift correction. The value
must be a valid Go duration string.

```yaml
metadata:
  annotations:
    cloudflare-gateway-controller.io/reconcileEvery: "5m"
```

The default interval is `10m`. If the value cannot be parsed, the Gateway is
rejected with `Accepted=False/InvalidParameters`.

### Cloudflare resource naming

The controller generates deterministic names for Cloudflare and Kubernetes
resources based on the Gateway identity. This enables cluster recreation
without leaking Cloudflare resources — a reborn cluster with the same
`clusterName` adopts existing resources instead of creating duplicates.

| Resource                | Name                                                        |
|-------------------------|-------------------------------------------------------------|
| Cloudflare Tunnel       | `cloudflare-gateway-controller.io/clusters/<clusterName>/namespaces/<namespace>/gateways/<gatewayName>` |
| cloudflared Deployment  | `gateway-<gatewayName>-<replicaName>` (default: `primary`)  |
| VPA (when autoscaling)  | `gateway-<gatewayName>-<replicaName>` (default: `primary`)  |
| Tunnel token Secret     | `gateway-<gatewayName>`                                     |
| Routes ConfigMap        | `gateway-<gatewayName>`                                     |
| Tunnel ServiceAccount   | `gateway-<gatewayName>`                                     |
| Tunnel Role             | `gateway-<gatewayName>`                                     |
| Tunnel RoleBinding      | `gateway-<gatewayName>`                                     |

`clusterName` comes from the Helm value `config.clusterName` (required).

Source: `TunnelName()`, `GatewayResourceName()`, `GatewayReplicaName()` in `api/v1/meta_types.go`.

## Validations

The controller validates the Gateway spec and annotations on every
reconciliation. If any validation fails, the Gateway is rejected with
`Accepted=False`.

### Annotations

- The `reconcileEvery` annotation value must be a valid Go duration string.
  Invalid values are rejected with `Accepted=False/InvalidParameters`.

### Listeners

- The Gateway must have exactly one listener. Multiple listeners are rejected
  with `Accepted=False/ListenersNotValid`.
- The listener `protocol` must be `HTTPS` and `port` must be `443`. Other
  values are rejected with `Accepted=False/ListenersNotValid`.
- Cloudflare handles TLS termination, so the `tls` field must not be set.
  Rejected with `Accepted=False/ListenersNotValid`.
- The listener `hostname` field must not be set. Hostnames are configured on
  [HTTPRoutes](HTTPRoute.md) instead. Rejected with
  `Accepted=False/ListenersNotValid`.
- If `allowedRoutes.kinds` is set, it must only contain `HTTPRoute`. Other
  kinds are rejected with `Accepted=False/ListenersNotValid`.

### Addresses

The `.spec.addresses` field must not be set. Rejected with
`Accepted=False/UnsupportedAddress`.

### Parameters reference

- `.spec.infrastructure.parametersRef` must reference a
  `CloudflareGatewayParameters`. Other kinds are rejected with
  `Accepted=False/InvalidParameters`.
- When referencing a `CloudflareGatewayParameters`, the resource must exist.
  Rejected with `Accepted=False/InvalidParameters`.

### Credentials

The controller must be able to resolve Cloudflare API credentials through
the [fallback chain](#credentials-resolution). If none of the sources
provide credentials, the Gateway is rejected with
`Accepted=False/InvalidParameters`.

When falling back to the GatewayClass `parametersRef`:

- The GatewayClass must have a `parametersRef`. Rejected with
  `Accepted=False/InvalidParameters`.
- The `parametersRef` must reference a `core/v1` Secret with a namespace.
  Rejected with `Accepted=False/InvalidParameters`.
- Cross-namespace Secret references must be allowed by a `ReferenceGrant`.
  Rejected with `Accepted=False/InvalidParameters`.

## Gateway Status

### Addresses

The controller populates `.status.addresses` with one entry for the managed
tunnel, using `type: Hostname` and the tunnel's CNAME target
(`<tunnelID>.cfargotunnel.com`).

### Conditions

A Gateway enters various states during its lifecycle, reflected as Kubernetes
Conditions. It can be [accepted](#accepted-gateway),
[programmed](#programmed-gateway),
[DNS-managed](#dns-management),
or [ready](#ready-gateway).

#### Accepted Gateway

Standard Gateway API condition. The controller marks a Gateway as _accepted_
when it passes validation.

When the Gateway is accepted, the controller sets a Condition with the following
attributes in the Gateway's `.status.conditions`:

- `type: Accepted`
- `status: "True"`
- `reason: Accepted`

When the Gateway is not accepted:

- `type: Accepted`
- `status: "False"`
- `reason: ListenersNotValid | UnsupportedAddress | InvalidParameters`

Reasons for rejection:

- `ListenersNotValid`: Listener validation failed (e.g. wrong listener count,
  unsupported protocol, TLS configured, hostname set on listener).
- `UnsupportedAddress`: `.spec.addresses` is set (not supported).
- `InvalidParameters`: `CloudflareGatewayParameters` could not be read or
  validated, or credentials are invalid.

#### Programmed Gateway

Standard Gateway API condition. The controller reports whether the tunnel
Deployment is available.

When the Deployment is ready:

- `type: Programmed`
- `status: "True"`
- `reason: Programmed`

When the Deployment is not yet ready:

- `type: Programmed`
- `status: "False"`
- `reason: Pending`

#### DNS Management

Custom condition, not part of the Gateway API spec. Reports whether DNS CNAME
record management is enabled for this Gateway.

DNS is enabled by default for all hostnames. It can be restricted to specific
zones via `.spec.dns.zones` in the
[CloudflareGatewayParameters](CloudflareGatewayParameters.md), or disabled
entirely by setting an empty zones list (`dns.zones: []`).

When DNS management is enabled for all hostnames (no zone restriction):

- `type: DNSManagement`
- `status: "True"`
- `reason: Enabled`
- `message: All hostnames`

When DNS management is enabled for specific zones:

- `type: DNSManagement`
- `status: "True"`
- `reason: Enabled`
- `message`: lists each allowed zone on its own line

When DNS management is disabled:

- `type: DNSManagement`
- `status: "False"`
- `reason: Disabled`

#### Ready Gateway

Custom condition, not part of the Gateway API spec.
[kstatus](https://github.com/kubernetes-sigs/cli-utils/blob/master/pkg/kstatus/README.md)-compatible
condition that summarizes the overall reconciliation state.

When all Cloudflare resources are reconciled and the Deployment is available:

- `type: Ready`
- `status: "True"`
- `reason: ReconciliationSucceeded`

When a terminal failure occurs (e.g. the Deployment exceeded its progress
deadline, unrecoverable API error):

- `type: Ready`
- `status: "False"`
- `reason: ReconciliationFailed`

When waiting for the Deployment to become ready (not an error):

- `type: Ready`
- `status: "Unknown"`
- `reason: Progressing`

When transient errors occurred during reconciliation and the controller will
retry:

- `type: Ready`
- `status: "Unknown"`
- `reason: ProgressingWithRetry`
