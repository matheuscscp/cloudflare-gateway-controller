# GatewayClass

The controller watches **GatewayClass** resources with
`.spec.controllerName: cloudflare-gateway-controller.io/controller`.

This document covers only the conditions set by this controller. For the full
GatewayClass spec, see the
[Gateway API documentation](https://gateway-api.sigs.k8s.io/api-types/gatewayclass/).

## Example

The following example shows a GatewayClass that uses the controller with a
default credentials Secret:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: cloudflare
spec:
  controllerName: cloudflare-gateway-controller.io/controller
  parametersRef:
    group: ""
    kind: Secret
    name: cloudflare-creds
    namespace: cloudflare-gateway-controller-system
```

The `parametersRef` field is optional and must reference a `core/v1` Secret
containing `CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID` keys. When set,
the Secret is used as the default credentials for all Gateways using this class
that do not specify their own `.spec.infrastructure.parametersRef`.

Note that `parametersRef` on a GatewayClass only supports Secrets, not
`CloudflareGatewayParameters`. Use the Gateway's
`.spec.infrastructure.parametersRef` to reference a
[CloudflareGatewayParameters](CloudflareGatewayParameters.md) instead.

## Validations

The controller validates the GatewayClass on every reconciliation. If any
validation fails, the GatewayClass is rejected with `Accepted=False`.

### Gateway API CRD version

The installed Gateway API CRD version must be compatible with the controller
binary. The CRD's major version must match the binary's major version, and the
CRD's minor version must be greater than or equal to the binary's minor version.

Rejection: `Accepted=False/UnsupportedVersion`, `SupportedVersion=False/UnsupportedVersion`.

### parametersRef

When `.spec.parametersRef` is set, the controller validates:

- `kind` must be `Secret`.
- `group` must be empty or `v1` (core API group).
- `namespace` must be specified (Secrets are namespaced resources).
- The referenced Secret must exist.
- The Secret must contain `CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID` keys.

Rejection: `Accepted=False/InvalidParameters`.

## GatewayClass Status

### Conditions

A GatewayClass enters various states during its lifecycle, reflected as
Kubernetes Conditions. It can be [accepted](#accepted-gatewayclass), it can
have a [supported version](#supported-version), or it can be
[ready](#ready-gatewayclass).

#### Accepted GatewayClass

Standard Gateway API condition. The controller marks a GatewayClass as
_accepted_ when the
`.spec.controllerName` matches and the optional `parametersRef` is valid.

When the GatewayClass is accepted, the controller sets a Condition with the
following attributes in the GatewayClass's `.status.conditions`:

- `type: Accepted`
- `status: "True"`
- `reason: Accepted`

When the GatewayClass is not accepted, the Condition has the following
attributes:

- `type: Accepted`
- `status: "False"`
- `reason: UnsupportedVersion | InvalidParameters`

Reasons for rejection:

- `UnsupportedVersion`: The installed Gateway API CRD version is not compatible
  with this controller build.
- `InvalidParameters`: `parametersRef` points to a resource that does not exist,
  is not a `Secret`, or is in a different namespace.

#### Supported version

Standard Gateway API condition. The controller reports whether the installed
Gateway API CRD version is compatible.

When the CRD version is compatible, the Condition has the following attributes:

- `type: SupportedVersion`
- `status: "True"`
- `reason: SupportedVersion`

When the CRD version is not compatible:

- `type: SupportedVersion`
- `status: "False"`
- `reason: UnsupportedVersion`

#### Ready GatewayClass

Custom condition, not part of the Gateway API spec.
[kstatus](https://github.com/kubernetes-sigs/cli-utils/blob/master/pkg/kstatus/README.md)-compatible
condition that summarizes the overall reconciliation state.

When the GatewayClass is ready, the controller sets a Condition with the
following attributes:

- `type: Ready`
- `status: "True"`
- `reason: ReconciliationSucceeded`

When the GatewayClass has a terminal failure:

- `type: Ready`
- `status: "False"`
- `reason: UnsupportedVersion | InvalidParameters`
