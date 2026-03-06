# CLI Reference

`cfgwctl` is the CLI for cloudflare-gateway-controller. Binaries are available
from [GitHub releases](https://github.com/matheuscscp/cloudflare-gateway-controller/releases).

## Commands

### `version`

Print CLI and controller version information. If a kubeconfig is available, it
also detects the running controller Deployment and prints its image.

```shell
cfgwctl version
```

### `suspend gateway`

Suspend reconciliation for a Gateway. While suspended, the controller skips all
reconciliation for the Gateway, and deleting the Gateway preserves Cloudflare
resources (tunnels, DNS records) and managed Kubernetes resources.

```shell
cfgwctl suspend gateway <name> [flags]
```

**Flags:**

- `-n, --namespace` — Namespace of the Gateway (defaults to kubeconfig context
  namespace).

### `resume gateway`

Resume reconciliation for a previously suspended Gateway. After resuming, the
command waits for the controller to complete one full reconciliation cycle.

```shell
cfgwctl resume gateway <name> [flags]
```

**Flags:**

- `-n, --namespace` — Namespace of the Gateway (defaults to kubeconfig context
  namespace).
- `--timeout` — Timeout waiting for reconciliation (default `5m`).

### `reconcile gateway`

Trigger an on-demand reconciliation for a Gateway. The command waits for the
controller to complete the reconciliation cycle.

```shell
cfgwctl reconcile gateway <name> [flags]
```

**Flags:**

- `-n, --namespace` — Namespace of the Gateway (defaults to kubeconfig context
  namespace).
- `--timeout` — Timeout waiting for reconciliation (default `5m`).

### `rotate gateway token`

Rotate the tunnel token for a Gateway on-demand. The controller generates a new
random secret, rotates it via the Cloudflare API, updates the in-cluster
Secret, and performs a rolling restart of the tunnel pods so they pick up the
new token. The command waits for the controller to complete the rotation.

```shell
cfgwctl rotate gateway token <name> [flags]
```

**Flags:**

- `-n, --namespace` — Namespace of the Gateway (defaults to kubeconfig context
  namespace).
- `--timeout` — Timeout waiting for token rotation (default `5m`).

## Internal commands

### `controller`

Run the Kubernetes controller. This is used by the controller Deployment and is
not intended for direct use.

### `tunnel`

Run the tunnel proxy. This is used by the tunnel Deployment and is not intended
for direct use.

### `test`

Test and inspection commands for Cloudflare resources. These commands are subject
to breaking changes and there are no backwards compatibility guarantees.
