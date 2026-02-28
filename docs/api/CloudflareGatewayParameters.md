# CloudflareGatewayParameters CRD

**CloudflareGatewayParameters** (short name: `cgp`) provides typed configuration
for Gateways managed by this controller. It is referenced via
`.spec.infrastructure.parametersRef` from a [Gateway](Gateway.md).

## Example

The following example shows a CloudflareGatewayParameters that configures
DNS record management and deployment patches:

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
    deployment:
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
in the attached [HTTPRoutes](HTTPRoute.md).

### Tunnels configuration

The `.spec.tunnels` field is optional and configures Cloudflare tunnel settings.

#### Deployment patches

The `.spec.tunnels.deployment.patches` field is optional and specifies
[RFC 6902](https://datatracker.ietf.org/doc/html/rfc6902) JSON Patch operations
applied to the cloudflared Deployment after it is built.

```yaml
spec:
  tunnels:
    deployment:
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
