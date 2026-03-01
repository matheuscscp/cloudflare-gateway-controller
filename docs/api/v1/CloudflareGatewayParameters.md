# CloudflareGatewayParameters CRD

**CloudflareGatewayParameters** (short name: `cgp`) provides typed configuration
for Gateways managed by this controller. It is referenced via
`.spec.infrastructure.parametersRef` from a [Gateway](Gateway.md).

## Example

The following example shows a CloudflareGatewayParameters that configures
DNS record management, tunnel patches, and sidecar settings:

```yaml
apiVersion: cloudflare-gateway-controller.io/v1
kind: CloudflareGatewayParameters
metadata:
  name: my-params
  namespace: default
spec:
  secretRef:
    name: cloudflare-creds
  dns:
    zones:
      - name: "example.com"
  tunnel:
    patches:
      - op: replace
        path: /spec/template/spec/nodeSelector
        value:
          kubernetes.io/os: linux
    sidecar:
      enabled: true
```

The Secret referenced by `.spec.secretRef.name` must contain the Cloudflare API
credentials:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: cloudflare-creds
  namespace: default
type: Opaque
stringData:
  CLOUDFLARE_API_TOKEN: "your-api-token-here"
  CLOUDFLARE_ACCOUNT_ID: "your-account-id-here"
```

## Writing a CloudflareGatewayParameters spec

As with all other Kubernetes config, a CloudflareGatewayParameters needs
`apiVersion`, `kind`, and `metadata` fields. The name of a
CloudflareGatewayParameters object must be a valid
[DNS subdomain name](https://kubernetes.io/docs/concepts/overview/working-with-objects/names#dns-subdomain-names).

A CloudflareGatewayParameters also needs a
[`.spec` section](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#spec-and-status).

### Credentials configuration

The `.spec.secretRef` field is optional and references a Secret in the same
namespace containing Cloudflare API credentials.

```yaml
spec:
  secretRef:
    name: cloudflare-creds
```

The `.spec.secretRef.name` field is required when `.spec.secretRef` is set.
The Secret must contain `CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID` keys.

When `.spec.secretRef` is not set, the controller resolves credentials through
the fallback chain described in the [Gateway](Gateway.md#credentials-resolution)
documentation.

### DNS configuration

The `.spec.dns` field configures DNS CNAME record management. By default (when
the `dns` field is omitted), DNS management is enabled for **all** hostnames in
attached HTTPRoutes — each hostname's zone is resolved dynamically via the
Cloudflare API.

There are three modes:

**All hostnames (default)** — omit the `dns` field entirely:

```yaml
spec: {}
```

**Specific zones** — list the zones to manage:

```yaml
spec:
  dns:
    zones:
      - name: "example.com"
      - name: "other.com"
```

Only hostnames that are single-level subdomains of a configured zone get CNAME
records (e.g. `app.example.com` matches zone `example.com`, but
`deep.sub.example.com` does not).

**Disabled** — set an empty zones list:

```yaml
spec:
  dns:
    zones: []
```

When DNS is enabled, the controller creates CNAME records for matching
hostnames in the attached [HTTPRoutes](HTTPRoute.md).

### Tunnel configuration

The `.spec.tunnel` field is optional and configures Cloudflare tunnel settings.

#### Patches

The `.spec.tunnel.patches` field is optional and specifies
[RFC 6902](https://datatracker.ietf.org/doc/html/rfc6902) JSON Patch operations
applied to the cloudflared Deployment.

Patches run **after** the controller builds the base Deployment (which includes
the sidecar container when enabled) but **before** replica placement fields
(`affinity`, `zone`, `nodeSelector`) are applied on top. This means:

- Patches can target any field of the base Deployment, including sidecar
  container fields (resources, probes, etc.).
- Replica placement always takes priority over user patches — setting affinity
  or nodeSelector via patches will be overwritten by the replica config.
- Removing the sidecar container via patches when the sidecar is enabled is a
  **terminal error**. Use `.spec.tunnel.sidecar.enabled: false` to disable the
  sidecar instead.
- Patch errors are terminal — the controller stops retrying until the
  CloudflareGatewayParameters resource is updated.

```yaml
spec:
  tunnel:
    patches:
      - op: replace
        path: /spec/template/spec/nodeSelector
        value:
          kubernetes.io/os: linux
      - op: add
        path: /spec/template/spec/tolerations
        value:
          - key: "CriticalAddonsOnly"
            operator: "Exists"
```

Each patch operation has the following fields:

- `op` (required): Patch operation. One of `add`, `remove`, `replace`, `move`,
  `copy`, `test`.
- `path` (required): JSON Pointer path for the operation.
- `from` (optional): Source path for `move` and `copy` operations.
- `value` (optional): Value for `add`, `replace`, and `test` operations.

#### Cloudflared container configuration

The `.spec.tunnel.cloudflared` field configures the cloudflared container.

##### Container resources

The `.spec.tunnel.cloudflared.resources` field configures compute resource
requirements for the cloudflared container. When absent, the controller uses
defaults (requests: 50m CPU, 64Mi memory; limits: 500m CPU, 256Mi memory).
When set, the provided values replace the defaults entirely.

```yaml
spec:
  tunnel:
    cloudflared:
      resources:
        requests:
          cpu: 100m
          memory: 128Mi
        limits:
          cpu: "1"
          memory: 512Mi
```

##### Container autoscaling

The `.spec.tunnel.cloudflared.autoscaling` field configures vertical pod
autoscaling for the cloudflared container.

```yaml
spec:
  tunnel:
    cloudflared:
      autoscaling:
        enabled: true
        minAllowed:
          cpu: 50m
          memory: 64Mi
        maxAllowed:
          cpu: "2"
          memory: 1Gi
        controlledResources: [cpu, memory]
        controlledValues: RequestsAndLimits
```

When `autoscaling.enabled` is `true`, the controller creates a
[VerticalPodAutoscaler](https://github.com/kubernetes/autoscaler/tree/master/vertical-pod-autoscaler)
(VPA) resource for each Deployment replica. The VPA produces resource
recommendations and applies them automatically using the `InPlaceOrRecreate`
update mode. A container policy with mode `Auto` is added for each container
that has autoscaling enabled.

The autoscaling fields are:

- `enabled` (required): Whether to enable VPA for this container.
- `minAllowed` (optional): Minimum recommended resources (floor).
- `maxAllowed` (optional): Maximum recommended resources (ceiling).
- `controlledResources` (optional): Which resource types to autoscale. Allowed
  values are `cpu` and `memory`. Defaults to both.
- `controlledValues` (optional): Which resource values to autoscale.
  `RequestsAndLimits` (default) scales both; `RequestsOnly` scales only
  requests.

The VPA CRD must be installed in the cluster for autoscaling to work. If the
CRD is not installed, VPA creation will fail and the error will be reported in
the Gateway status.

#### Sidecar configuration

The `.spec.tunnel.sidecar` field configures the sidecar reverse proxy that runs
alongside cloudflared for per-request load balancing through kube-proxy.

By default (when this field is absent), the sidecar is **enabled**. Set
`.spec.tunnel.sidecar.enabled` to `false` to disable the sidecar and let
cloudflared connect directly to backend Services.

```yaml
spec:
  tunnel:
    sidecar:
      enabled: false
```

When the sidecar is disabled, the cloudflared Deployment has a single container,
no ConfigMap/ServiceAccount/Role/RoleBinding resources are created, and tunnel
ingress rules point directly to backend Services instead of the sidecar proxy.
Because cloudflared uses persistent connections, kube-proxy cannot effectively
distribute traffic across pods. Traffic splitting (weighted `backendRefs`) and
session persistence are also not available, and HTTPRoutes with multiple
`backendRefs` in a single rule or `sessionPersistence` are rejected.

##### Sidecar container resources

The `.spec.tunnel.sidecar.resources` field configures compute resource
requirements for the sidecar container. Follows the same semantics as
`.spec.tunnel.cloudflared.resources`.

```yaml
spec:
  tunnel:
    sidecar:
      resources:
        requests:
          cpu: 100m
          memory: 128Mi
        limits:
          cpu: "1"
          memory: 512Mi
```

##### Sidecar container autoscaling

The `.spec.tunnel.sidecar.autoscaling` field configures vertical pod
autoscaling for the sidecar container. Follows the same semantics as
`.spec.tunnel.cloudflared.autoscaling`.

```yaml
spec:
  tunnel:
    sidecar:
      autoscaling:
        enabled: true
```
