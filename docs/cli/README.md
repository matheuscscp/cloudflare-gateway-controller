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

Resume reconciliation for a previously suspended Gateway. By default, the
command waits for the controller to complete one full reconciliation cycle.

```shell
cfgwctl resume gateway <name> [flags]
```

**Flags:**

- `-n, --namespace` — Namespace of the Gateway (defaults to kubeconfig context
  namespace).
- `--wait` — Wait for reconciliation to complete (default `true`).
- `--timeout` — Timeout waiting for reconciliation (default `5m`).

### `reconcile gateway`

Trigger an on-demand reconciliation for a Gateway. By default, the command
waits for the controller to complete the reconciliation cycle.

```shell
cfgwctl reconcile gateway <name> [flags]
```

**Flags:**

- `-n, --namespace` — Namespace of the Gateway (defaults to kubeconfig context
  namespace).
- `--wait` — Wait for reconciliation to complete (default `true`).
- `--timeout` — Timeout waiting for reconciliation (default `5m`).

### `rotate gateway token`

Rotate the tunnel token for a Gateway on-demand. The controller generates a new
random secret, rotates it via the Cloudflare API, updates the in-cluster
Secret, and performs a rolling restart of the tunnel pods so they pick up the
new token. With `--watch`, the command monitors the rolling update in real-time
until all Deployments have completed their rollout.

```shell
cfgwctl rotate gateway token <name> [flags]
```

**Flags:**

- `-n, --namespace` — Namespace of the Gateway (defaults to kubeconfig context
  namespace).
- `--watch` — Watch the rolling update in real-time until completion (default
  `false`).
- `--timeout` — Timeout waiting for token rotation, only used with `--watch`
  (default `10m`).

### `watch gateway token`

Watch an ongoing token rotation for a Gateway. Attaches to an in-progress
rolling update and monitors each Deployment until all have been updated with
the new token hash and fully rolled out. Prints a timeline report on
completion.

```shell
cfgwctl watch gateway token <name> [flags]
```

**Flags:**

- `-n, --namespace` — Namespace of the Gateway (defaults to kubeconfig context
  namespace).
- `--timeout` — Timeout waiting for rotation to complete (default `30m`).

## Internal commands

### `controller`

Run the Kubernetes controller. This is used by the controller Deployment and is
not intended for direct use.

### `tunnel`

Run the tunnel proxy. This is used by the tunnel Deployment and is not intended
for direct use.

### `test`

Test and inspection commands for Cloudflare resources. These commands are subject
to breaking changes, there are no backwards compatibility guarantees.
