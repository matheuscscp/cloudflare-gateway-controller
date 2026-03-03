# HTTPRoute

The controller processes **HTTPRoute** resources attached to managed Gateways
and sets conditions on `.status.parents` entries with
`.controllerName: cloudflare-gateway-controller.io/controller`.

This document covers only the conditions set by this controller. For the full
HTTPRoute spec, see the
[Gateway API documentation](https://gateway-api.sigs.k8s.io/api-types/httproute/).

## Example

The following example shows an HTTPRoute that routes traffic for
`app.example.com` to a backend Service:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: my-route
  namespace: default
spec:
  parentRefs:
    - name: my-gateway
  hostnames:
    - "app.example.com"
  rules:
    - backendRefs:
        - name: my-backend
          port: 80
```

When DNS is configured in the [CloudflareGatewayParameters](CloudflareGatewayParameters.md),
the controller creates CNAME records for each hostname in the HTTPRoute.

## Validations

The controller validates each HTTPRoute on every reconciliation. If any
validation fails, the HTTPRoute is rejected with `Accepted=False/UnsupportedValue`.

### Unsupported fields

The following fields are not supported and must not be set:

- `spec.parentRefs[*].port`
- `spec.rules[*].filters`
- `spec.rules[*].timeouts`
- `spec.rules[*].retry`
- `spec.rules[*].backendRefs[*].filters`

### Backend references

- Only `core` `Service` backends are supported. Other `group`/`kind`
  combinations (e.g. custom backend resources) are rejected.
- Multiple `backendRefs` in a single rule are supported. The embedded reverse
  proxy distributes requests across backends according to their `weight` fields
  (traffic splitting).
- Cross-namespace `backendRefs` must be allowed by a `ReferenceGrant`.
  Without one, the HTTPRoute is accepted but the condition
  `ResolvedRefs=False/RefNotPermitted` is set.

### Match rules

- Only `PathPrefix` match type is supported. Other path match types
  (`Exact`, `RegularExpression`) are rejected.
- Header matches (`spec.rules[*].matches[*].headers`) are not supported.
- Query parameter matches (`spec.rules[*].matches[*].queryParams`) are not
  supported.
- Method matches (`spec.rules[*].matches[*].method`) are not supported.

### Session persistence

Session persistence pins a client to the same backend across requests.

**Cookie-based** (default): The proxy sets a cookie on the first response.
Subsequent requests with that cookie are routed to the same backend.

```yaml
rules:
  - sessionPersistence:
      type: Cookie
      sessionName: my-session          # optional, default "cgw-session"
      absoluteTimeout: 1h              # optional
      idleTimeout: 10m                 # optional
      cookieConfig:
        lifetimeType: Permanent        # "Session" (default) or "Permanent"
    backendRefs:
      - name: svc-a
        port: 80
        weight: 50
      - name: svc-b
        port: 80
        weight: 50
```

**Header-based**: The client supplies a header with the backend ID. No cookie
is set by the proxy.

```yaml
rules:
  - sessionPersistence:
      type: Header
      sessionName: X-My-Session        # optional, default "X-Cgw-Session"
    backendRefs:
      - name: svc-a
        port: 80
```

**Cookie lifetime types:**

- `Session` (default): Browser session cookie (no `Max-Age`). If
  `absoluteTimeout` is set, the proxy enforces it server-side by checking
  the timestamp encoded in the cookie value.
- `Permanent`: Sets `Max-Age` on the cookie. When only `absoluteTimeout` is
  set, `Max-Age` equals `absoluteTimeout`. When only `idleTimeout` is set,
  `Max-Age` equals `idleTimeout`. When both are set, `Max-Age` is the minimum
  of the remaining absolute timeout and `idleTimeout`.

**Idle timeout** (`idleTimeout`): Supported for cookie-based sessions only. When
configured, the proxy re-issues the cookie on every response with an updated
last-activity timestamp. If the time since the last activity exceeds
`idleTimeout`, the session expires and a new backend is selected. When both
`absoluteTimeout` and `idleTimeout` are set, both are enforced independently —
the session expires if either timeout is exceeded.

`idleTimeout` is **not supported** for header-based sessions (no mechanism to
push updated timestamps to the client) and is rejected if configured with
`type: Header`.

### Namespace restrictions

The HTTPRoute must be in a namespace allowed by the Gateway's listener
`allowedRoutes` configuration. Routes from disallowed namespaces are
rejected with `Accepted=False/NotAllowedByListeners`.

## HTTPRoute Status

### Conditions

An HTTPRoute enters various states during its lifecycle, reflected as Kubernetes
Conditions on each `.status.parents` entry. It can be
[accepted](#accepted-httproute), have its
[references resolved](#resolved-refs),
have [DNS records applied](#dns-records-applied), or be
[ready](#ready-httproute).

#### Accepted HTTPRoute

Standard Gateway API condition. The controller marks an HTTPRoute as _accepted_
when it passes validation.

When the HTTPRoute is accepted:

- `type: Accepted`
- `status: "True"`
- `reason: Accepted`

When the HTTPRoute is not accepted:

- `type: Accepted`
- `status: "False"`
- `reason: NotAllowedByListeners | UnsupportedValue`

Reasons for rejection:

- `NotAllowedByListeners`: The HTTPRoute is in a namespace not allowed by the
  Gateway's listener `allowedRoutes`.
- `UnsupportedValue`: The HTTPRoute uses unsupported features (e.g. unsupported
  match types, filters, or backendRef kinds).

#### Resolved refs

Standard Gateway API condition. The controller reports whether all `backendRef`
references could be resolved.

When all references are resolved:

- `type: ResolvedRefs`
- `status: "True"`
- `reason: ResolvedRefs`

When a reference cannot be resolved:

- `type: ResolvedRefs`
- `status: "False"`
- `reason: RefNotPermitted`

The `RefNotPermitted` reason indicates a cross-namespace backendRef is not
permitted by a `ReferenceGrant`.

#### DNS records applied

Custom condition, not part of the Gateway API spec. Reports whether DNS CNAME
records have been applied for the route's hostnames. This condition is only
present when DNS management is enabled (see
[CloudflareGatewayParameters](CloudflareGatewayParameters.md) for configuration
details).

When DNS records are applied successfully:

- `type: DNSRecordsApplied`
- `status: "True"`
- `reason: ReconciliationSucceeded`

In all-hostnames mode (default), the `message` lists all applied hostnames. In
specific-zones mode, the `message` lists applied and skipped hostnames.

When DNS record creation or update fails:

- `type: DNSRecordsApplied`
- `status: "Unknown"`
- `reason: ProgressingWithRetry`

This condition is removed from the status when DNS is disabled.

#### Ready HTTPRoute

Custom condition, not part of the Gateway API spec.
[kstatus](https://github.com/kubernetes-sigs/cli-utils/blob/master/pkg/kstatus/README.md)-compatible
condition. This condition starts from the parent Gateway's `Ready` value but may
be downgraded to `Unknown`/`ProgressingWithRetry` when the Gateway is ready but
a DNS error occurred for this route's hostnames.

When the Gateway is ready and DNS records (if configured) applied successfully:

- `type: Ready`
- `status: "True"`
- `reason: ReconciliationSucceeded`

When the Gateway has a terminal failure:

- `type: Ready`
- `status: "False"`
- `reason: ReconciliationFailed`

When the Gateway is waiting for the Deployment:

- `type: Ready`
- `status: "Unknown"`
- `reason: Progressing`

When the Gateway is retrying after transient errors, or the Gateway is ready
but a DNS error occurred for this route:

- `type: Ready`
- `status: "Unknown"`
- `reason: ProgressingWithRetry`
