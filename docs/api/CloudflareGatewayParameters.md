# CloudflareGatewayParameters CRD

**CloudflareGatewayParameters** (short name: `cgp`) provides typed configuration
for Gateways managed by this controller. It is referenced via
`.spec.infrastructure.parametersRef` from a [Gateway](Gateway.md).

## Example

The following example shows a CloudflareGatewayParameters that configures
DNS record management, two availability zones, and a geographic load balancer
with health monitoring:

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
    zone:
      name: "example.com"
  tunnels:
    availabilityZones:
      - name: az-a
        zone: us-east-1a
      - name: az-b
        zone: us-east-1b
    cloudflared:
      patches:
        - op: replace
          path: /spec/template/spec/nodeSelector
          value:
            kubernetes.io/os: linux
  loadBalancer:
    topology: HighAvailability
    steeringPolicy: Geographic
    sessionAffinity: Cookie
    monitor:
      type: HTTPS
      path: /ready
      interval: 60
      timeout: 5
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

The `.spec.dns` field is optional and configures DNS CNAME record management.

```yaml
spec:
  dns:
    zone:
      name: "example.com"
```

The `.spec.dns.zone.name` field is required when `.spec.dns` is set and
specifies the DNS zone name (e.g. `example.com`).

When DNS is configured, the controller creates CNAME records for each hostname
in the attached [HTTPRoutes](HTTPRoute.md). This field is required when
`.spec.loadBalancer` is set.

### Tunnels configuration

The `.spec.tunnels` field is optional and configures Cloudflare tunnel settings.

#### Availability zones

The `.spec.tunnels.availabilityZones` field is optional and specifies multi-AZ
tunnel deployment. Each entry creates a separate tunnel and cloudflared
Deployment.

Example using the `zone` shorthand for `topology.kubernetes.io/zone` node
affinity:

```yaml
spec:
  tunnels:
    availabilityZones:
      - name: az-a
        zone: us-east-1a
      - name: az-b
        zone: us-east-1b
```

Example using `nodeSelector` for label-based pod placement:

```yaml
spec:
  tunnels:
    availabilityZones:
      - name: az-1
        nodeSelector:
          workload: gateway-az1
      - name: az-2
        nodeSelector:
          workload: gateway-az2
```

Example using a full Kubernetes `affinity` spec:

```yaml
spec:
  tunnels:
    availabilityZones:
      - name: az-1
        affinity:
          nodeAffinity:
            requiredDuringSchedulingIgnoredDuringExecution:
              nodeSelectorTerms:
                - matchExpressions:
                    - key: topology.kubernetes.io/zone
                      operator: In
                      values:
                        - us-west-2a
```

Each availability zone entry has the following fields:

- `name` (required): Identifies this AZ. Used in Deployment, tunnel, and pool
  naming. Must be 1-63 lowercase alphanumeric characters or hyphens, starting
  and ending with an alphanumeric character.
- `zone` (optional): Shorthand for `topology.kubernetes.io/zone` node affinity.
- `nodeSelector` (optional): Label key-value pairs for node selection.
- `affinity` (optional): Full Kubernetes
  [Affinity](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.31/#affinity-v1-core)
  spec for pod placement.

Exactly one of `zone`, `nodeSelector`, or `affinity` must be set.

**Cross-field validations:**

- `.spec.loadBalancer` is required when `.spec.tunnels.availabilityZones` is set.
- `.spec.tunnels.availabilityZones` is required when `.spec.loadBalancer.topology`
  is `HighAvailability`.

#### Cloudflared deployment patches

The `.spec.tunnels.cloudflared.patches` field is optional and specifies
[RFC 6902](https://datatracker.ietf.org/doc/html/rfc6902) JSON Patch operations
applied to the cloudflared Deployment after it is built.

```yaml
spec:
  tunnels:
    cloudflared:
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

### Load balancer configuration

The `.spec.loadBalancer` field is optional and configures Cloudflare Load
Balancer settings.

```yaml
spec:
  loadBalancer:
    topology: HighAvailability
    steeringPolicy: Geographic
    sessionAffinity: Cookie
    monitor:
      type: HTTPS
      path: /ready
      interval: 60
      timeout: 5
```

This field is required when `.spec.tunnels.availabilityZones` is set. When
`.spec.loadBalancer` is set, `.spec.dns` is also required.

#### Topology

The `.spec.loadBalancer.topology` field is required and specifies the load
balancer topology.

Supported values:

- `HighAvailability`: One tunnel per AZ, each carrying full ingress rules.
  The LB steers traffic between zones. Requires
  `.spec.tunnels.availabilityZones`.
- `TrafficSplitting`: One tunnel per Service backendRef (times per AZ if
  `availabilityZones` is set). Each tunnel routes traffic to one service. The
  LB distributes traffic by HTTPRoute backendRef weights.

#### Steering policy

The `.spec.loadBalancer.steeringPolicy` field is optional and specifies the
traffic distribution policy.

Supported values:

- `Off`: No active steering.
- `Geographic`: Routes to closest pool by client region.
- `Random`: Distributes traffic proportionally using pool weights.
- `DynamicLatency`: Learns latency over time and routes to the fastest pool.
- `Proximity`: Routes to lowest-latency pool based on health check RTT.
- `LeastOutstandingRequests`: Routes to pool with fewest pending requests.
- `LeastConnections`: Routes to pool with fewest active connections.

#### Session affinity

The `.spec.loadBalancer.sessionAffinity` field is optional and specifies the
session persistence mode. The default value is `None`.

Supported values:

- `None`: No session affinity.
- `Cookie`: Routes subsequent requests to the same pool using a cookie.
- `IPCookie`: Like `Cookie` but based on client IP address.
- `Header`: Routes based on a request header value.

#### Health monitor

The `.spec.loadBalancer.monitor` field is optional and configures health
checking for tunnel origins.

```yaml
spec:
  loadBalancer:
    monitor:
      type: HTTPS
      path: /ready
      interval: 60
      timeout: 5
```

The monitor fields are:

- `type` (optional): Health check protocol. One of `HTTP`, `HTTPS`, `TCP`.
  Default is `HTTPS`.
- `path` (optional): Health check endpoint path. Default is `/ready`.
- `interval` (optional): Seconds between health checks. When `0` or omitted,
  Cloudflare uses the plan-specific default.
- `timeout` (optional): Seconds before marking a health check as failed. When
  `0` or omitted, Cloudflare uses the plan-specific default.

## Validations

The following cross-field validations are enforced by CEL rules on the CRD
and by the controller at reconciliation time. Violations are reported on the
referencing Gateway as `Accepted=False/InvalidParameters`.

- `.spec.dns` is required when `.spec.loadBalancer` is set. The load
  balancer needs a DNS zone to create load balancers in.
- `.spec.loadBalancer` is required when `.spec.tunnels.availabilityZones`
  is set. Multiple tunnels without a load balancer would leave traffic
  unbalanced.
- `.spec.tunnels.availabilityZones` is required when
  `.spec.loadBalancer.topology` is `HighAvailability`. The HA topology
  distributes traffic across zones, so at least one AZ must be defined.
- Each availability zone entry must have exactly one of `zone`,
  `nodeSelector` (non-empty), or `affinity`. Setting none or more than one
  is rejected.
