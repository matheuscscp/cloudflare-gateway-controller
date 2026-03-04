# API Reference (v1)

This directory documents the Kubernetes resources managed by
cloudflare-gateway-controller.

## Gateway API resources

These are standard [Gateway API](https://gateway-api.sigs.k8s.io/) resources.
The documentation here covers only the controller-specific behavior,
annotations, and conditions.

- [GatewayClass](GatewayClass.md) — Cluster-scoped resource that identifies
  this controller and optionally references a default credentials Secret.
- [Gateway](Gateway.md) — Manages a Cloudflare tunnel and its associated
  Kubernetes resources. Supports reconciliation control, on-demand
  reconciliation, and on-demand token rotation via annotations.
- [HTTPRoute](HTTPRoute.md) — Routes traffic to backend Services by hostname
  and path. Supports traffic splitting, session persistence, and DNS CNAME
  management.

## Custom resources

These are CRDs defined by this controller.

- [CloudflareGatewayParameters](CloudflareGatewayParameters.md) — Optional
  typed configuration for a Gateway: credentials, DNS zones, tunnel replicas,
  patches, container resources, autoscaling, and token rotation.
- [CloudflareGatewayStatus](CloudflareGatewayStatus.md) — Read-only
  observability resource that mirrors Gateway conditions and tracks tunnel
  info, managed resource inventory, and token rotation timestamps.
