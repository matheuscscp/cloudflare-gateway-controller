# E2E Test Local Iteration Guide

This guide is the source of truth for running and debugging e2e tests locally.
It is written for both humans and AI agents. Follow it carefully — it contains
hard-won lessons from debugging real failures.

## The #1 Priority: Fast Iteration

**Speed of iteration is everything.** A full e2e run (cluster create, install,
test, teardown) takes 5-10 minutes. If you repeat that cycle for every small
fix, you will waste hours. The tooling in this project is specifically designed
to let you skip expensive steps and re-run only what changed.

**Always ask yourself:** "Can I skip the cluster? Can I skip the image build?
Can I run just the one failing test?" The answer is almost always yes. Use the
`REUSE_CLUSTER`, `REUSE_CONTROLLER`, `RELOAD_CONTROLLER`, and `TEST` env vars
aggressively. A single-test re-run with a reused cluster takes seconds, not
minutes.

**Concrete rules for fast iteration:**
1. **Build the cluster once.** Set `REUSE_CLUSTER=1` for every subsequent run.
2. **Only rebuild the image when Go code changes.** If you're only changing
   shell scripts or test logic, use `REUSE_CONTROLLER=1` — no image rebuild
   needed.
3. **Use `RELOAD_CONTROLLER=1` after image rebuilds.** This reloads the image
   into kind and restarts the controller pod — faster than recreating the
   cluster.
4. **Target a single test with `TEST=<function_name>`.** Don't run the full
   suite while debugging one failure.
5. **NEVER block on a test run.** Always launch the test in the background and
   actively monitor while it runs. This is the single most important rule for
   fast iteration. A blocking test run means you can't see problems until the
   entire retry timeout expires (up to 3 minutes). Instead:
   - Launch the make command in the background (e.g. using `run_in_background`
     for the Bash tool, or `&` in a terminal).
   - Immediately start tailing the **controller log file** and the **test
     output file** to see progress and catch failures as they happen. The test
     framework automatically streams controller logs to a local file (e.g.
     `test-cfgw-e2e-ha-controller.log` — see `CONTROLLER_LOG_FILE` in
     `e2e-lib.sh`). The test output is also saved to a log file (e.g.
     `test-e2e-ha.log`). Tail both files — there is no need to run
     `kubectl logs` separately.
   - **Never sleep more than 30 seconds** between checks. You must wake up
     frequently so the user can interact with you. Use `sleep 30` between
     tail checks, never longer than 30 seconds.
   - If you spot an error in the logs, you can kill the test, fix the issue,
     and re-run immediately — saving the entire retry wait.
   - **Do NOT wrap the make command with another `tee`.** The Makefile already
     pipes output through `tee` to a log file (e.g. `test-e2e-ha.log`). Adding
     a second `tee` to the same file causes garbled, duplicated output that is
     unreadable. Just launch the make command directly and tail the log file
     separately.
6. **Clean up orphaned Cloudflare resources before retrying** if a previous
   run failed mid-way. Leftover tunnels/LBs will cause confusing failures.

## Prerequisites

- `kind` (Kubernetes in Docker)
- `kubectl`
- `helm`
- `jq`
- A Cloudflare API token with these **exact** permissions:
  - **Account**: Cloudflare Tunnel: Edit, Load Balancing: Monitors And Pools: Edit, Account Load Balancers: Edit
  - **All zones**: DNS: Edit, Load Balancers: Edit

**Permission pitfall:** "Account Load Balancers: Edit" and "Load Balancing:
Monitors And Pools: Edit" are two **separate** permissions. You need both.
Without the latter, account-level pool/monitor API calls
(`accounts/{id}/load_balancers/pools` and `accounts/{id}/load_balancers/monitors`)
will return 403.

Save your credentials to `api.token` in the repo root (gitignored):

```
CLOUDFLARE_API_TOKEN=<your-token>
CLOUDFLARE_ACCOUNT_ID=<your-account-id>
```

## Test Suites

| Make target | Script | Topology | Kind config |
|---|---|---|---|
| `test-e2e` | `hack/e2e-test.sh` | Simple (no LB) | Single node |
| `test-e2e-ha` | `hack/e2e-test-ha.sh` | HighAvailability | Multi-node (2 AZ workers) |
| `test-e2e-ts` | `hack/e2e-test-ts.sh` | TrafficSplitting | Single node |
| `test-e2e-ts-az` | `hack/e2e-test-ts-az.sh` | TrafficSplitting + AZs | Multi-node (2 AZ workers) |

All suites require `TEST_ZONE_NAME` set to a Cloudflare DNS zone you control.

## First Run (Full)

Build the image and run a full suite:

```bash
make docker-build
TEST_ZONE_NAME=my.zone.dev make test-e2e-ha
```

This creates a kind cluster, installs the controller via Helm, runs all tests,
and tears down the cluster on exit. Takes ~5-10 minutes per suite.

## Fast Iteration

### Reuse the cluster (skip creation/deletion)

```bash
REUSE_CLUSTER=1 REUSE_CONTROLLER=1 TEST_ZONE_NAME=my.zone.dev make test-e2e-ha
```

This saves ~90s by reusing the existing kind cluster and skipping controller
installation (if the pod is already Running). The cluster persists after tests
finish.

### Run a single test

```bash
REUSE_CLUSTER=1 REUSE_CONTROLLER=1 TEST=test_ha_httproute TEST_ZONE_NAME=my.zone.dev make test-e2e-ha
```

Set `TEST=<function_name>` to run one specific test function. Available test
names are listed at the bottom of each script in the `run_tests` call. If you
pass an invalid name, the script prints the list of available tests.

**Note:** Some tests depend on state created by earlier tests (e.g.
`test_ha_httproute` needs the Gateway created by `test_ha_basic`). If running
a test in isolation, make sure its prerequisites are already in place from a
prior run.

### Reload controller after code changes

When you change controller code:

```bash
make docker-build
REUSE_CLUSTER=1 RELOAD_CONTROLLER=1 TEST_ZONE_NAME=my.zone.dev make test-e2e-ha
```

`RELOAD_CONTROLLER=1` reloads the image into kind and does a rolling restart of
the controller Deployment. Combined with `REUSE_CLUSTER=1`, you skip cluster
creation entirely.

### Typical debug loop

This is the workflow that minimizes wasted time. **Never skip straight to a
full clean run when debugging** — always reuse what you can.

```bash
# 1. First run — creates the cluster (only do this once per debugging session)
make docker-build
TEST_ZONE_NAME=my.zone.dev make test-e2e-ha

# 2. A test failed. Fix the controller bug, then rebuild + reload (no cluster recreate):
make docker-build
REUSE_CLUSTER=1 RELOAD_CONTROLLER=1 TEST=test_ha_route_deletion TEST_ZONE_NAME=my.zone.dev make test-e2e-ha

# 3. Still failing? Fix the test script and re-run instantly (no image rebuild needed):
REUSE_CLUSTER=1 REUSE_CONTROLLER=1 TEST=test_ha_route_deletion TEST_ZONE_NAME=my.zone.dev make test-e2e-ha

# 4. Test passes! Run the full suite to confirm nothing else broke:
REUSE_CLUSTER=1 REUSE_CONTROLLER=1 TEST_ZONE_NAME=my.zone.dev make test-e2e-ha

# 5. All green. Delete the cluster when completely done:
kind delete cluster --name cfgw-e2e-ha
```

**Time comparison:**
- Full run (create cluster + install + all tests + teardown): ~5-10 min
- Reused cluster + single test: ~10-30 sec
- Reused cluster + reload controller + single test: ~30-60 sec

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `TEST_ZONE_NAME` | *(required)* | Cloudflare DNS zone for test hostnames |
| `IMAGE` | `cloudflare-gateway-controller:dev` | Controller image name:tag |
| `CREDENTIALS_FILE` | `./api.token` | Path to Cloudflare credentials file |
| `CFGWCTL` | `./bin/cfgwctl` | Path to cfgwctl binary |
| `KIND_CLUSTER_NAME` | per-script default | Kind cluster name |
| `REUSE_CLUSTER` | *(unset)* | Set to `1` to reuse existing cluster |
| `REUSE_CONTROLLER` | *(unset)* | Set to `1` to skip controller install |
| `RELOAD_CONTROLLER` | *(unset)* | Set to `1` to reload image + restart |
| `TEST` | *(unset)* | Run only this test function |

## Debugging Failures

### Rule #1: Always watch controller logs

This is the most important debugging technique. When a test step appears stuck
or takes more than a few seconds, the controller logs will tell you exactly
what's wrong — a permission error, a validation rejection, a retry loop.
**Do not wait for retry timeouts.** Check the logs immediately.

The test framework automatically streams controller logs to a local file via
`start_controller_log_stream` in `e2e-lib.sh`. The file path follows the
pattern `test-<cluster-name>-controller.log` (e.g.
`test-cfgw-e2e-ha-controller.log`). Tail this file — there is no need to run
`kubectl logs` separately:

```bash
tail -f test-cfgw-e2e-ha-controller.log
```

### Common failure patterns

**403 Forbidden on LB API calls:** The API token is missing a permission.
Check the error URL:
- `accounts/.../load_balancers/pools` or `accounts/.../load_balancers/monitors`
  = missing "Load Balancing: Monitors And Pools: Edit" (account level)
- `zones/.../load_balancers` = missing "Load Balancers: Edit" (zone level)

**Gateway stuck deleting:** The finalizer is failing — usually a Cloudflare API
error during cleanup. Check controller logs for the exact error. If the cluster
was deleted while a Gateway was stuck, orphaned Cloudflare resources will remain
(see cleanup section below).

**Monitor creation fails with "interval is not in range" or "probe_timeout"
errors:** The Cloudflare **free plan does not support load balancer monitors**.
The API constraints are contradictory on the free plan (interval must be 0 but
timeout must be >= 1, and interval must be > (retries+1)*timeout). You **must**
have a paid plan (at least Pro, or a Load Balancing add-on) to test any LB
topology (HA, TS, TS+AZ). The simple (no-LB) topology works on the free plan.

**Test passes locally but tunnel config check is slow:** The `retry` helper
suppresses output. If a check takes more than ~10 seconds, look at the
controller logs to see if reconciliation completed or is erroring.

### Credential changes require a new cluster

The Cloudflare credentials are stored in a Kubernetes Secret. If you update
`api.token` (e.g. adding new permissions), the existing Secret in the cluster
still has the old value. Either:
- Delete the cluster and start fresh (simplest)
- Or manually update the Secret: `kubectl delete secret cloudflare-creds -n <test-ns>`,
  then let the test script recreate it

## Cleaning Up Orphaned Cloudflare Resources

If a test run fails and the cluster is deleted before cleanup, Cloudflare
resources (tunnels, DNS records, LB pools, monitors) may be left behind. Use
the cleanup script:

```bash
make build-cfgwctl
hack/cleanup-cloudflare.sh
```

This deletes all load balancers, pools, monitors, DNS CNAME records, and
tunnels associated with the credentials. Run this before starting a fresh test
suite if you suspect orphaned resources.

## Test Case Documentation

Every e2e test case is documented in `docs/tests/`. When adding or modifying
tests, update the corresponding file:

| File | Suite |
|------|-------|
| `docs/tests/simple.md` | Simple (no LB) |
| `docs/tests/high-availability.md` | HighAvailability |
| `docs/tests/traffic-splitting.md` | TrafficSplitting |
| `docs/tests/traffic-splitting-with-azs.md` | TrafficSplitting + AZs |

## Architecture Notes

### Shared library: `hack/e2e-lib.sh`

All test scripts source `hack/e2e-lib.sh` which provides:
- `log`, `pass`, `fail` — test output helpers (`fail` dumps diagnostic info)
- `retry max delay cmd...` — retry a command with delay
- `cfgwctl` — shorthand for `$CFGWCTL --credentials-file $CREDENTIALS_FILE`
  (exported via `export -f` so it works in `bash -c` subshells)
- Cluster lifecycle: `setup_cluster`, `cleanup_cluster`, `register_cleanup`
- Controller lifecycle: `install_controller` (handles REUSE/RELOAD)
- Test infra: `create_test_namespace`, `ensure_gatewayclass`, `run_tests`

### Shell function export

The `cfgwctl` function is exported via `export -f cfgwctl` so that `bash -c`
subshells (used inside `retry` calls) can use it. This is a bash-specific
feature. All e2e scripts use `#!/usr/bin/env bash`.

### CI zone separation

In CI, the 4 suites run in parallel (each in a separate GitHub Actions matrix
job) using separate DNS zones to avoid cleanup interference:

- `ci.cloudflare-gateway-controller.dev` (simple)
- `ci-ha.cloudflare-gateway-controller.dev` (HA)
- `ci-ts.cloudflare-gateway-controller.dev` (TrafficSplitting)
- `ci-tsaz.cloudflare-gateway-controller.dev` (TrafficSplitting + AZs)

This is necessary because `cleanupAllLBResources` during Gateway finalization
deletes ALL load balancers in a zone, not just those owned by the Gateway being
deleted. Parallel tests sharing a zone would interfere with each other.

Locally, you can use a single zone and run suites sequentially.

## Summary: What Makes You Fast

If you remember nothing else from this guide, remember this:

1. **Reuse the cluster** (`REUSE_CLUSTER=1`). Creating a kind cluster takes
   ~60-90 seconds every time. Do it once per session.
2. **Reuse the controller** (`REUSE_CONTROLLER=1`). Only reload when Go code
   changed.
3. **Target a single test** (`TEST=function_name`). Don't run all 5 tests when
   debugging 1.
4. **Watch logs, not timeouts.** Open controller logs the instant something
   looks slow. The answer is always in the logs.
5. **Clean up before retrying.** If a run failed and left orphaned Cloudflare
   resources, run `hack/cleanup-cloudflare.sh` before trying again. Stale
   tunnels/LBs/pools cause mysterious failures.

The difference between using these techniques and not is the difference between
a 30-second iteration cycle and a 10-minute one. Over a debugging session with
20 iterations, that's 10 minutes vs 3+ hours.
