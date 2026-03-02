# E2E Tests

**The #1 priority is fast iteration.** A full e2e cycle takes 5-10 minutes.
With `REUSE_CLUSTER=1`, `REUSE_CONTROLLER=1`, and `TEST=<name>`, a single-test
re-run takes seconds. Follow these rules:

1. **Never recreate what you can reuse.** Build the cluster once, reuse it for
   every subsequent run. Only rebuild the image when Go code changes.
2. **Never run the full suite when debugging one test.** Use `TEST=<name>`.
3. **Never block on a test run.** Always launch in the background and actively
   monitor by tailing the controller log file (`test-<cluster>-controller.log`)
   and the test output file (`test-e2e.log`). Catch failures in real time
   instead of waiting 3+ minutes for retry timeouts.
4. **Never sleep more than 30 seconds** between checks on a running test. You
   must stay responsive.
5. **Do NOT wrap the make command with another `tee`.** The Makefile already
   pipes output through `tee` to `test-e2e.log`. A second `tee` to the same
   file causes garbled output. Just launch make directly and tail the log file.
6. **Clean up orphaned Cloudflare resources** before retrying after a failed
   run (`hack/cleanup-cloudflare.sh`). Leftover tunnels cause confusing
   failures.

## Prerequisites

- `kind`, `kubectl`, `helm`, `jq`
- A Cloudflare API token with **Account: Cloudflare Tunnel: Edit** and
  **All zones: DNS: Edit** permissions.

Save credentials to `api.token` in the repo root (gitignored):

```
CLOUDFLARE_API_TOKEN=<your-token>
CLOUDFLARE_ACCOUNT_ID=<your-account-id>
```

## Running

```bash
# Full run (first time)
make docker-build
TEST_ZONE_NAME=my.zone.dev make test-e2e

# Reuse cluster + controller, single test
REUSE_CLUSTER=1 REUSE_CONTROLLER=1 TEST=test_gateway_lifecycle TEST_ZONE_NAME=my.zone.dev make test-e2e

# After Go code changes: rebuild image + reload
make docker-build
REUSE_CLUSTER=1 RELOAD_CONTROLLER=1 TEST=test_gateway_lifecycle TEST_ZONE_NAME=my.zone.dev make test-e2e
```

Available test names are in the `run_tests` call at the bottom of
`hack/e2e-test.sh`. Environment variables and their defaults are documented in
the header of `hack/e2e-lib.sh`.

## CI parallelization

In CI, each test runs as a separate GitHub Actions matrix job with
`KIND_CLUSTER_NAME` set to the test name. This makes each test produce unique
Cloudflare resource names (the tunnel name includes the cluster name), so all
tests safely share the same Cloudflare account concurrently.

## Multi-zone tests

`test_multi_zone_dns` and `test_multi_gateway_overlapping_zones` require
`TEST_ZONE_NAME_2` and `TEST_ZONE_NAME_3`. These can be subdomains of the
same Cloudflare zone (e.g. `z2.example.com` and `z3.example.com` when
`TEST_ZONE_NAME=dev.example.com`). If unset, the tests skip.

```bash
TEST=test_multi_zone_dns \
  TEST_ZONE_NAME_2=z2.example.com \
  TEST_ZONE_NAME_3=z3.example.com \
  make test-e2e
```

Test cases are documented in `docs/tests/e2e.md`.
