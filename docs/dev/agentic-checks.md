# Agentic Checks

This document contains a list of checks an agent should perform against
the codebase when prompted to do so. They ensure the codebase is in a
good state, meeting all the necessary requirements and quality standards.

- **README.md**: The `README.md` file is always up-to-date, correctly
  describing the project, its purpose, and how to use it in minimal
  terms, but also showcasing the main features and capabilities of
  the controller. CLI commands should be covered minimally, with at
  most one line per command.
- **API docs**: The API docs at `docs/api/` are always up-to-date, correctly
  reflecting all the features and behaviors of the controller.
- **CLI docs**: The CLI docs at `docs/cli/README.md` are always up-to-date,
  correctly describing all the available commands and their usage. The `test`
  subcommand and its subcommands should not be documented and should just
  mention that they are subject to breaking changes and there are no backwards
  compatibility guarantees for them.
- **E2E docs**: The E2E docs at `docs/e2e/README.md` are always up-to-date,
  correctly describing and reflecting all the E2E test cases and their
  expected outcomes.
- **Test CLI**: The `test` subcommand of the CLI never implements more
  commands than necessary for the tasks in this project that rely on it,
  such as the E2E tests and the Cloudflare resource cleanup script.
- **Coverage Target**: The coverage target is as close to 100% as possible
  for the packages listed in the `make test` output, except the `api/v1`
  package which has mostly generated code (the non-generated code in this
  package should still be covered well!). When performing this check, the
  agent should look at the uncovered lines and determine if they can bring
  the coverage up. No need to touch `preflight.go` or `preflight_test.go`.
  VPA unit tests are not needed, we already have an E2E test for VPA.
- **Good Controller Requirements**: The controller always meets the
  requirements outlined in the good controller guide at `docs/dev/`.

For each of these checks, the agent should *VERY* thoroughly verify that
the last commit diff is *FULLY* compliant with the requirements.
