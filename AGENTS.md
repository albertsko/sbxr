### Commands

- Run `mise run <task> [args...]` to execute a task. When no `default` task is defined, bare `mise run` opens the task picker or, outside a terminal, lists tasks and exits with status 1.
- Run `mise run ci` before considering a change complete.

### Tools

- Install with `mise install`.
- Add a tool to `[tools]` in `mise.toml`.
- When adding a Go module dependency, run `golangci-lint linters` and check for a linter for that dependency.

### Commits

- Commit and push only when asked.
- Use _Conventional Commits_ for commits and pull request titles. Mark breaking changes with `!`.
- The squash merge uses the pull request title.

### Project layout

- `cmd/` has one directory per binary. `cmd/sbxr/entitlements.plist` holds the macOS hypervisor entitlement, and the release archive carries it at its root.
- `internal/` holds all Go packages.
- `internal/kube/` holds all Kubernetes code: `installer`, `shim`, `agent`, `podnet`, `podvm`, and `subid`. Only `internal/kube/` and the Kubernetes commands `cmd/sbxr-node`, `cmd/sbxr-agent`, and `cmd/containerd-shim-sbxr-v1` may import it.
- `internal/libkrun/` loads and starts `libkrun`. It imports no other package of this module, so the CLI and the shim share it.
- `internal/e2e/` holds only `_test.go` files: the end-to-end tests of the release binary, which they build themselves. These are the host-only CLI tests, the behavior scenarios, and the `vm` and `kube` tests.
- `internal/testenv/` holds the test helper `SkipOrFailf` and the layout test that checks the target file names, build tags, and test names.
- Tests run against real environments, with no fake targets or simulated VMs. A unit test may replace an outside system only to inject failures that a real environment cannot produce.
- Tests come in three kinds: unit tests (no build tag, `mise run test`), `vm` (build tag `vm`, `mise run test:vm`, test names start with `TestVM`), and `kube` (build tag `kube`, `mise run test:kube`, test names start with `TestKube`).
- The hosts are `darwin` and `linux`. Lint covers both GOOS values.
- `SBXR_TEST_REQUIRE=1` makes a `vm` or `kube` test fail instead of skip when its environment is missing. CI sets it.
- `deploy/node/` holds the node installer image: the Dockerfile with `Dockerfile.dockerignore`, its build scripts, the manifest template, and `render.sh`, which renders the manifests.
- `tasks/` holds mise tasks only.
- `scripts/` holds helper scripts that tests and tasks call, such as `scripts/gotest-exec.sh` and `scripts/kind/`.
- `scripts/ci/` holds scripts that only GitHub Actions calls.
- `docs/` holds current user documentation only, never plans or history.
- The root holds only `mise.toml`, `hk.pkl`, `.golangci.yml`, `.goreleaser.yaml`, `.editorconfig`, `.gitignore`, `go.mod`, `go.sum`, `README.md`, `CONTRIBUTING.md`, `LICENSE.md`, `AGENTS.md`, and the `CLAUDE.md` symlink.

### Dependency rules

- `cmd/sbxr` and the Kubernetes commands only wire packages together. `internal/cli` alone parses the `sbxr` flags, prints command output, and uses `cobra` and the charm libraries. The `internal/e2e` tests may also use the charm ANSI helper.
- `internal/run` runs one workload with `runspec`, `image`, `preflight`, and `vm`. `internal/cli` passes it the network from `internal/netstack`.
- Lower packages, such as `runspec`, `image`, `netstack`, `preflight`, `vm`, and `libkrun`, never import `internal/cli` or `internal/run`.
- Only `internal/libkrun` loads `libkrun`. `internal/vm`, `internal/preflight`, and `internal/kube/shim` use it.
- `internal/kube` is a separate product. Outside `internal/kube`, only the Kubernetes commands import it, and it never imports `internal/cli`, `internal/run`, or `internal/image`.
- `depguard` in `.golangci.yml` enforces these import boundaries. Add a new lower package to its `lower-layer` rule.

### CI

- Put executable task scripts in `tasks/` with their descriptions in `#MISE` headers.
- CI installs the tools configured in `mise.toml`. Pin every tool to an exact version, so that all jobs use the same one. `mise run upgrade` bumps them.
- `.github/workflows/ci.yml` runs on pull requests and on main. Its job `CI result` fails when any other job fails, so it is the one check to require.
- `.github/workflows/nightly.yml` runs the shuffled tests without the test cache, the shuffled `vm` tests on Linux KVM, and the long fuzz run.
- `.github/workflows/release.yml` calls `ci.yml` on the tagged commit before it publishes.
- Every job sets up with `.github/actions/setup`, which installs mise and restores the Go cache. Only a push to main saves caches, with `.github/actions/save-go-cache`.
- Give every job and step a readable `name`. Put a `run:` that needs more than one line into a script in `scripts/ci/` with `set -euo pipefail`.
- Pin actions by full commit SHA with a version comment, and give each workflow the least `permissions`, a timeout per job, and a concurrency group.

### Releases

- Before you tag, run `SBXR_TEST_REQUIRE=1 mise run test:vm` on an Apple silicon Mac. CI has no macOS hypervisor, so this local run is the macOS release gate.
- Tag with `git tag -a v1.2.3 -m "Summary."` and push. Lightweight tags fail.
- The tag message opens the release notes.

### This file

- `CLAUDE.md` is a symlink to `AGENTS.md`.
- Write each list item on one line.
