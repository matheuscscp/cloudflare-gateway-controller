# CloudflareGatewayParameters CRD

**CloudflareGatewayParameters** (short name: `cgp`) provides typed configuration
for Gateways managed by this controller. It is referenced via
`.spec.infrastructure.parametersRef` from a [Gateway](Gateway.md).

## Example

The following example shows a CloudflareGatewayParameters that configures
DNS record management and tunnel patches:

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

#### Token rotation

The `.spec.tunnel.token` field configures tunnel token management.

```yaml
spec:
  tunnel:
    token:
      rotation:
        enabled: true
        interval: 24h
```

When automatic token rotation is enabled, the controller periodically rotates
the tunnel token via the Cloudflare API and updates the in-cluster Secret. The
rotation is seamless — cloudflared picks up the new token without pod restarts.

The token rotation fields are:

- `rotation.enabled` (optional): Whether automatic rotation is active.
  Defaults to `true` when the `rotation` struct is present.
- `rotation.interval` (optional): Interval between automatic rotations.
  Defaults to `24h`.

On-demand rotation can also be triggered via `cfgwctl rotate gateway token`,
regardless of whether automatic rotation is configured. See the
[Gateway annotation](Gateway.md#on-demand-token-rotation) for details.

#### Health check URL

The `.spec.tunnel.health` field configures an additional health check for the
tunnel. The value must be an HTTPS origin — `https://` followed by a hostname
only (no path, query, or fragment). When set, the tunnel's `/healthz` and
`/readyz` endpoints probe the URL via HTTPS GET. If the probe fails (network
error or non-2xx response), the health check fails and Kubernetes may restart
the pod.

The embedded reverse proxy serves a 200 OK response at the root path of the
health hostname, so no backend service is required for the health check. A
startup probe is automatically added to the tunnel container so the pod has
time to establish connectivity through Cloudflare before the liveness probe
starts.

```yaml
spec:
  tunnel:
    health:
      url: "https://health.example.com"
```

The hostname of the health URL is automatically included in the set of desired
DNS CNAME records pointing to the tunnel, subject to the same DNS zone filtering
rules as HTTPRoute hostnames.

**Validation:** The URL must be `https://` followed by a hostname only — no
path, query, or fragment. When `dns.zones` is set, the hostname must be a
single-level subdomain of at least one configured zone.

#### Patches

The `.spec.tunnel.patches` field is optional and specifies
[RFC 6902](https://datatracker.ietf.org/doc/html/rfc6902) JSON Patch operations
applied to the cloudflared Deployment.

Patches run **after** the controller builds the base Deployment but **before**
replica placement fields (`affinity`, `zone`, `nodeSelector`) are applied on top.
This means:

- Patches can target any field of the base Deployment, including tunnel
  container fields (resources, probes, etc.).
- Replica placement always takes priority over user patches — setting affinity
  or nodeSelector via patches will be overwritten by the replica config.
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

#### Replicas

The `.spec.tunnel.replicas` field configures multiple replicas of the tunnel pods
for high availability. There are no guarantees about how requests from Cloudflare
will be distributed among replicas, but the embedded reverse proxy improves load
balancing when proxying to backend Services.

There are three behaviors:

**Single replica (default)** — omit the `replicas` field entirely. The controller
creates one Deployment named `gateway-<gatewayName>-primary`.

**Scale to zero** — set an explicitly empty list:

```yaml
spec:
  tunnel:
    replicas: []
```

No Deployments are created.

**Multiple replicas** — list each replica with a unique name:

```yaml
spec:
  tunnel:
    replicas:
      - name: alpha
        zone: us-east-1a
      - name: beta
        zone: eu-west-1a
```

Each entry creates a separate Deployment named
`gateway-<gatewayName>-<replicaName>`. All replicas share the same tunnel,
Secret, and ConfigMap resources.

Each replica has the following fields:

- `name` (required): Identifies the replica. Must be 1–63 characters, lowercase
  alphanumeric with hyphens (DNS label format). Names must be unique within the
  list.
- `zone` (optional): Shorthand for `topology.kubernetes.io/zone` node affinity.
  Mutually exclusive with `affinity`.
- `nodeSelector` (optional): Map of label key-value pairs for node selection.
- `affinity` (optional): Full Kubernetes affinity spec for pod placement.
  Mutually exclusive with `zone`.

Replica placement fields (`affinity`, `zone`, `nodeSelector`) are applied after
base Deployment construction and user patches, so they always take priority.

#### Tunnel container configuration

The `.spec.tunnel` field configures the tunnel container, which embeds both
cloudflared and the reverse proxy in a single container.

##### Container resources

The `.spec.tunnel.resources` field configures compute resource requirements
for the tunnel container. When absent, the controller uses defaults
(requests: 50m CPU, 64Mi memory; limits: 500m CPU, 256Mi memory). When set,
the provided values replace the defaults entirely.

```yaml
spec:
  tunnel:
    resources:
      requests:
        cpu: 100m
        memory: 128Mi
      limits:
        cpu: "1"
        memory: 512Mi
```

##### Container autoscaling

The `.spec.tunnel.autoscaling` field configures vertical pod autoscaling
for the tunnel container.

```yaml
spec:
  tunnel:
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
update mode.

The autoscaling fields are:

- `enabled` (required): Whether to enable VPA for the tunnel container.
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
