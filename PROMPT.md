## Stage 1: local product

### What we want

sbxr is a Go project. Stage 1 is a polished local product. Stage 2 (Kubernetes) comes later.

The main Stage 1 user is a developer who runs AI coding agents and other untrusted code in Docker. Defaults favor safety.

1. A local CLI (`sbxr`) and one per-user daemon on macOS arm64, Linux amd64, and Linux arm64. The daemon runs real Docker (`dockerd` and `runc`) inside an isolated Ubuntu 26.04 LTS libkrun microVM. Containers run as plain `runc` containers in that VM. There is no VM per container. The user reaches the VM through `docker context`.
2. Multiple host VMs: each host VM has a name, its own Docker context, CA (certificate authority), credentials, and egress policy.
3. Credentials through the egress proxy, like smolvm (https://github.com/smol-machines/smolvm). Real secret values and private CA keys stay on the physical host. A guest gets only placeholders and the public CA certificate it needs. Stage 1 injects HTTP API tokens (for example `Authorization` headers). The daemon resolves real values on demand from credential sources (see Credentials), so a daemon restart needs no CLI call.
4. Egress control per host VM. The user allows or denies destinations by domain (for example `example.com` and `*.example.com`) and by IP range (for example `203.0.113.0/24`). Rules match domains and IP ranges only, not URL paths. The policy covers all traffic that leaves the host VM, including image pulls by `dockerd`. DNS always goes to the resolver in sbxr. A new host VM denies all egress except an editable preset that lists exact registry domains for Docker Hub and common image registries, including their blob CDN domains.
5. Low cost when idle: after an idle timeout (default 15 minutes), the host VM shuts down. The first Docker API connection boots it again.
6. Host files: no shares by default. The user adds project directories per host VM, read-only or read-write.
7. Published ports listen only on loopback (`127.0.0.1` and `::1`) of the physical host. sbxr rewrites an explicit host IP (for example `-p 0.0.0.0:8080:80`) to loopback and warns.
8. Images for a foreign architecture (for example amd64 images on Apple Silicon) run through emulation.
9. Docker workflows that must work: `run`, `exec`, `logs`, `build`, Compose, and Testcontainers.
10. Install through a Homebrew tap on macOS and a tarball on Linux, without `sudo`.
11. Stage 1 is done when all of these hold:
    - The Docker workflows pass on real host VMs in CI on macOS and Linux. Do not skip the tests.
    - Egress, credential, and isolation tests pass, including bypass attempts by guest root.
    - Memory growth is under the 512 MiB hard limit and is reported against the 384 MiB target. Wake time is measured and reported.
    - The quickstart in the docs runs as an e2e test and passes.

### What we care about

Terms:

- Physical host: the Mac or Linux machine.
- Host VM: the Ubuntu VM that runs `dockerd`. Each host VM has one Docker context.
- Guest: everything that runs inside a host VM.
- Guest agent: the sbxr process inside a host VM. Its binary is `cmd/sbxr-guest`.
- Root image: the read-only Ubuntu file system that a host VM boots.
- Egress proxy: the sbxr component on the physical host that enforces egress policy and injects credentials.
- Docker API proxy: the part of the guest agent that sits between Docker clients and `dockerd`.

Language and dependencies:

- No Rust code in this repository. The build may use a pinned Rust toolchain only to build libkrun.
- Load libkrun with `purego` (https://github.com/ebitengine/purego). Release binaries use `CGO_ENABLED=0`. Tests may use cgo for `-race`.
- Pin the latest stable release of each dependency. For libkrun, pin the latest stable 1.x release, not the 2.0 development branch. Keep libkrun patches minimal.
- Build only for `darwin/arm64`, `linux/amd64`, and `linux/arm64`. Update `.goreleaser.yaml` to these three targets and bundle libkrun, the kernel, and the root image.

Root image:

- Build it with `mmdebstrap --variant=minbase` from a pinned `snapshot.ubuntu.com` timestamp, so builds are reproducible.
- Install Docker from Docker's `docker-ce` packages (with buildx and the Compose plugin), pinned to one version.
- Ship the root image read-only, with an empty `/etc/machine-id`, so each host VM generates its own at first boot.
- Keep Docker data, containerd data, `/etc`, and `/var` on a separate sparse ext4 data disk. `sbxr host upgrade` replaces only the root image.
- Boot with a minimal initramfs that mounts the data disk, overlays `/etc` and `/var` onto it, and then executes systemd. Check which virtio drivers the Ubuntu kernel builds in and which ones the initramfs must load.
- Work in two phases. Phase A: the root image boots and all Docker workflows pass. Phase B: strip the root image.
- Gate Phase B on measurements: compressed root image size, time from boot to a working `docker info`, and idle memory growth. Record each removed package and masked systemd unit with its measured effect.

Config and state:

- Config: `$XDG_CONFIG_HOME/sbxr`, default `~/.config/sbxr`, on macOS too.
  - `config.yaml` holds global settings (`default_host`) and a `defaults:` block.
  - `hosts/<name>.yaml` holds one host VM: CPU, memory, disk, idle timeout, shares, registry mirrors, egress rules, and credentials.
  - `presets/<name>.yaml` holds a user egress preset.
- Egress rules, credentials, and shares belong to one host VM. A host VM file can include presets by name.
- `defaults:` may hold only `cpus`, `memory`, `disk_cap`, `idle_timeout`, and `registry_mirrors`. A host VM value replaces the default. Lists are never merged.
- Built-in presets ship with sbxr. A user preset with the same name as a built-in preset is an error.
- State: `$XDG_STATE_HOME/sbxr`, default `~/.local/state/sbxr` (disks, audit log, CA key fallback). Cache: `$XDG_CACHE_HOME/sbxr`.
- Sockets: `$XDG_RUNTIME_DIR/sbxr` on Linux and `~/.sbxr/run` on macOS, in a `0700` directory owned by the user. Check that each socket path fits the length limit.
- The config files are the single source of truth. The daemon writes them when the CLI asks for a change. Read-only commands (`path`, `validate`, `fmt --check`, `show --effective`) also work without the daemon.
- The schema marks each key as live or restart-required. The daemon applies live keys (for example egress rules) without a host VM restart. `sbxr config status` shows, per host VM, whether the config is applied, rejected, or waiting for a restart.
- Provide `sbxr config path`, `edit`, `validate`, `fmt [--check]`, `show --effective`, `status`, and `migrate`.

Config file format:

- Use YAML with the latest stable `go.yaml.in/yaml/v4`.
- Output is deterministic, so dotfile diffs stay small. Keys follow schema order. Lists without meaningful order are sorted: `allow`, `deny`, `presets`, `shares` by `path`, and `credentials` by `name`. Use block style, 2-space indent, and quotes only where a value would otherwise not parse as a string.
- New files hold only values that differ from the defaults. Explicit values stay, even when they equal the current default. Only a user edit removes them. `sbxr config show --effective` prints the merged result and marks the source of each value (default, file and line, or preset).
- Every file sbxr writes starts with a `yaml-language-server` modeline that points to a versioned JSON Schema URL. sbxr generates the schema from its Go types and publishes it per version. The editor fetches the schema. sbxr itself makes no network calls.
- Loading is strict. Unknown keys, duplicate keys, anchors, aliases, and custom tags are errors. `sbxr config validate` warns about values that YAML 1.1 tools read differently (for example `no`, `on`, and `1.10`).
- Errors show `file:line:column`, a snippet, and a suggestion for misspelled keys.
- Config files store credential source references only, never secret values. Reject values that look like tokens (for example `sk-`, `ghp_`, and `xoxb-` prefixes) and tell the user how to store them with `sbxr cred add`.
- Only `~` is expanded. There is no environment variable interpolation.
- Every write produces the canonical form and keeps comments. The first CLI edit of a hand-formatted file can therefore reformat it once. Commands that change config accept `--dry-run`, which prints a unified diff. `sbxr config fmt` rewrites a file into the canonical form.
- Writes follow symlinks, because dotfile managers such as stow and chezmoi link these files. Replace the symlink target atomically. Refuse to write when the file changed since sbxr read it. Keep one `.bak` copy.
- `sbxr config edit` opens `$EDITOR` on a private temporary copy. After the editor exits, the daemon validates the copy and writes it only if the original file has not changed. When the copy is invalid, the CLI reopens it with the errors as comments at the top, or cancels.
- Validate the whole effective config of a host VM (host file, defaults, and presets) before applying any part of it. When it is invalid, the daemon keeps the last applied config and reports the error in `sbxr config status` and `sbxr host status`.
- When the daemon starts and a host VM has no valid effective config, its egress stays closed until a valid config exists.
- Each file has a `version` key. `sbxr config migrate` shows a diff and writes the new version. For a file with a newer version, sbxr reports that version and the highest version it supports, asks the user to upgrade sbxr, and never rewrites the file.
- Config files must be regular files owned by the user and not writable by group or others. Check the symlink and its target. Parent directories must be owned by the user or root and not writable by others. The error names the unsafe path and the fix.
- Config can name commands that run on the physical host, so untrusted code must not be able to write it. `sbxr share add` refuses a share that contains the sbxr config or state directory.

Credentials:

- A credential has a name, a domain, a header, a value template (for example `Bearer {secret}`), and one source.
- Sources: `keychain` (macOS Keychain, Linux Secret Service), `command` (an argv run without a shell, with a timeout), and `file` (a file with mode `0600`). Each source has an optional `ttl` (default 5 minutes, `0` disables caching).
- The daemon resolves a secret when the egress proxy first needs it and caches it in memory for the `ttl`, per host VM and credential. A change to or removal of the credential clears its cache entry. After a daemon restart, it resolves again on demand.
- Resolution never prompts and has a timeout. When a keychain is locked, unavailable, or asks for approval, deny the request and tell the user how to restore access. Never log command output.
- A daemon started by launchd gets a minimal `PATH`. `sbxr cred add` resolves `argv[0]` of a `command` source to an absolute path and stores that path.
- `sbxr cred test <name>` resolves the secret inside the daemon, without the cache, and prints only success and the source. It never prints the value.
- When a source fails, the request fails. The audit log and `sbxr egress denied` show the error without the value.
- Credential sources form a Go interface, so Stage 2 can add a Kubernetes Secrets source.

Egress proxy:

- One CA per host VM. sbxr generates it when it creates the host VM and deletes it with the host VM. Host VMs never share a CA, so a certificate from one host VM's CA is not trusted in another host VM.
- The private CA key stays on the physical host. sbxr stores it in the OS keychain and falls back to a `0600` file in the state directory.
- The egress proxy decrypts TLS only for domains that have a credential rule. It classifies other TLS by SNI and by the DNS answers it served.
- When SNI and the HTTP `Host` header differ on a decrypted connection, deny the connection.

CA trust in containers:

- Credential injection needs the container to trust the host VM's CA, but container images have their own trust stores. sbxr sets up this trust by default, so injection does not fail silently.
- Inside the host VM, `dockerd` listens on a private socket. The Docker API proxy owns `/var/run/docker.sock`. Containers that mount `docker.sock` go through the same proxy.
- The Docker API proxy changes only `ContainerCreate` requests. It mounts a read-only CA bundle (Mozilla roots plus the host VM's CA) and sets `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`, `PIP_CERT`, `GIT_SSL_CAINFO`, `AWS_CA_BUNDLE`, `DENO_CERT`, and `CARGO_HTTP_CAINFO`. It never overrides a variable set by the request or by the image config.
- A container label opts out of CA injection.
- The Docker API proxy is a convenience, not a security control. Guest root can bypass it, and it holds no secrets.
- `sbxr doctor --container <id>` shows what the Docker API proxy injected.
- Build steps do not inherit container CA trust. Builds that use only passthrough domains need no change.
- For a `RUN` step that contacts a credential-rule domain, `sbxr ca export <host>` writes the public CA bundle (Mozilla roots plus the host VM's CA) to a file outside the build context. The user mounts it with a BuildKit build secret (`--secret id=sbxr-ca,...` and `RUN --mount=type=secret,id=sbxr-ca`). Document Docker and Compose (`build.secrets`) examples.
- If a Testcontainers client cannot pass build secrets, build the image first with `docker buildx build` and use its local tag.
- Do not change build requests, wrap the build executor's `runc`, or install the CA in image layers.

UX:

- Use `cobra` for the CLI and follow https://clig.dev/.
- Every command that prints data supports `--json`. `--follow` prints NDJSON. Document exit codes. Honor `NO_COLOR`.
- The golden path is: install, `sbxr init`, `sbxr host create dev`, `docker --context sbxr-dev run ...`, see a blocked connection, `sbxr egress allow ...`. The docs quickstart shows this path.
- `sbxr init` checks the physical host, creates the config directory, and installs the daemon user unit. `sbxr host create` creates a host VM and makes the first one the default host VM.
- `sbxr host use <name>` sets `default_host` in `config.yaml`. It never changes the Docker context. Commands that act on one host VM accept `--host <name>` and otherwise use the default host VM.
- `sbxr env [<name>]` prints `export DOCKER_CONTEXT=sbxr-<name>` for a shell or an agent.
- `sbxr host shell <name>` opens a debug shell in the guest.
- sbxr coexists with Docker Desktop, OrbStack, and Colima. It never changes the user's current Docker context unless asked.
- When egress policy blocks a connection, the user sees it quickly and can fix the rule. Return HTTP 403 with an explanation body where the protocol allows it, and a TCP reset otherwise. `sbxr egress denied [--follow]` queries the audit log. `sbxr egress allow --last` allows the last denied destination.
- Agents often need package registries that the default preset does not allow. Ship named presets for npm, PyPI, and the Go proxy.
- `sbxr doctor` checks `/dev/kvm` access, the macOS hypervisor entitlement, socket path length, and Docker context conflicts.
- Testcontainers starts a `ryuk` container that mounts `docker.sock`, and tests connect to published ports from the physical host. Both must work.

DevEx:

- Clear package boundaries:
  - `internal/host`: physical-host code that Stage 2 reuses (`vmm`, `network`, `egress`, `ca`, `creds`, `audit`, `rootimage`). These packages take plain option structs, never config types.
  - `internal/local`: Stage 1-only code (`daemon`, `cli`, `config`, `dockerctx`, `idle`). `config` translates YAML into the option structs.
  - `internal/guest`: guest code. `agent` is reusable. `dockerproxy` is Stage 1-only.
  - `internal/proto`: the protocols, with no logic. `api` is CLI to daemon. `guestapi` is physical host to guest.
  - `internal/platform/{darwin,linux}`: platform-specific code.
- Enforce package boundaries with `depguard` rules in `.golangci.yml`:
  - `internal/proto` imports only the standard library.
  - `internal/host` and `internal/guest/agent` never import `internal/local` or `internal/guest/dockerproxy`.
  - `internal/guest` never imports `internal/host` or `internal/local`.
  - Mock libraries are banned everywhere.
- Repository tasks are discoverable through `mise run`, give fast local feedback, and fail with clear errors.
- `mise run ci` runs on hosted runners without VMs, finishes in under 5 minutes, and also vets and lints `e2e/`.
- `mise run test:e2e` runs against real host VMs.
- `mise run image:build` runs `mmdebstrap` in a Linux container through any Docker context, or downloads the released root image for the pinned hash. It caches the root image by content hash.
- `mise run dev` cross-compiles the guest agent and replaces it in a running host VM.
- On macOS, `mise run build` ad-hoc signs the binary with the `com.apple.security.hypervisor` entitlement. Ad-hoc signed release binaries are fine for Stage 1. Releases run on Linux, so sign with `rcodesign` or add a macOS release job.
- Deliver in milestones: boot, Docker context, egress, credentials, idle and wake, Phase B. Each milestone ends with a demo and a decision record in `docs/decisions/`.
- When rules conflict or you are blocked, stop and ask.

Operations:

- Each host VM gets small default CPU and memory. The user can change them per host VM.
- The data disk is sparse, has a size cap, and returns freed space to the physical host. `sbxr` shows disk usage.
- Root image updates are explicit (`sbxr host upgrade`). An upgrade keeps images, volumes, and other Docker data. sbxr warns when the installed sbxr release bundles a newer root image than a host VM runs. This check is local and makes no network calls.
- Physical host sleep, resume, and network changes need no host VM restart. The guest clock resyncs, and DNS and egress keep working.
- Shut down a host VM only when it is really idle. Idle means no running containers, builds, exec or attach sessions, open Docker API connections (including event and log streams), and open connections on published ports for the whole timeout.
- The guest agent reports guest-side activity. The daemon reports physical-host-side activity. When either signal is missing or unclear, the host VM stays up.
- `sbxr host status` shows why a host VM is not idle and when it will shut down.
- A local audit log records allowed and denied connections and credential injections. The user can query it with `sbxr`. It never contains real secret values.
- No phone-home.

Assertions:

- Assert invariants that sbxr's own code guarantees. Return errors for anything that crosses a boundary: user input, files, network, the guest, FFI, and time.
- Assertions stay on in release builds.
- A failed assertion calls `os.Exit`, or panics outside `net/http` handlers, so no recovery hides it. The daemon restarts crashed host VM processes. A restarted daemon rebuilds its state from disk and from the running host VM processes.

Architecture:

- Run exactly one sbxr daemon per user on the physical host. It owns and supervises everything on the physical host.
- The CLI is a thin client. It changes state only through daemon calls.
- `sbxr init` installs one launchd or systemd user unit for the daemon, without `sudo`. The unit restarts the daemon when it crashes. The CLI also starts the daemon on demand when the unit is missing.
- Host VMs survive a daemon crash within the same login session. An intentional daemon stop or uninstall stops all host VMs. Choose the launchd and systemd settings that give both, and test both through the installed unit.
- The daemon runs each host VM in a child process (`sbxr __vm <name>`), because `krun_start_enter` takes over the process that calls it. The child process runs only libkrun. It holds no policy, secrets, or network logic.
- A restarted daemon adopts the running host VM processes after it verifies each one's identity from state on disk. A PID alone is not enough. Open guest connections reset during the restart. Document this.
- libkrun's unix socket network backends may disable networking when their peer goes away, so recreating the socket path may not reconnect. Before you build the daemon, prove recovery with the pinned libkrun on macOS and Linux: kill the daemon during traffic, then check that the guest boot ID is unchanged, egress stays closed during recovery, and new allowed connections work afterward. If recovery needs a process that keeps the socket open or a libkrun patch, stop and ask.
- The CLI talks to the daemon over a local unix socket using JSON-RPC 2.0.
- sbxr never runs a VM inside a host VM. CI may run sbxr on runners where KVM itself is nested.
- Real `dockerd` inside the host VM. Do not reimplement the Docker Engine API. The Docker API proxy, which changes requests, is allowed.
- Use systemd as init in the host VM. The `docker-ce` and `containerd.io` packages ship systemd units, and on cgroup v2 `dockerd` uses the systemd cgroup driver when systemd is available. The guest agent runs as a systemd service.
- Use the Ubuntu kernel, booted through libkrun's external kernel support (`krun_set_kernel`). Use libkrunfw or a minimal non-Ubuntu kernel only if measurements show it is needed, and only after the user approves it.
- Networking uses virtio-net connected to a userspace network stack in the daemon, one instance per host VM. Egress enforcement and credential injection run in the daemon too. TSI needs libkrunfw kernel patches, so do not use it. Call `krun_disable_implicit_vsock()` and add only the vsock ports that sbxr needs.

Memory:

- Minimize the memory growth that each running host VM adds. Target 384 MiB, hard limit 512 MiB. Both apply to measured growth, not to the configured size.
- Measure system-wide used memory growth on the physical host. On macOS, use the htop 3.5.3 Mem-used formula: (wired + max(anonymous - purgeable, 0) + compressor) times page size. No Linux method is chosen yet. Choose one and document it.
- Show per-process resident memory (RES) in `sbxr` status, but do not use it as the gate.
- Use libkrun resource features such as the memory balloon and free page reporting. smolvm is an example that probably does this well.

Security:

- The host VM, not the container, is the hard trust boundary.
- A guest never sees real secret values or private CA keys.
- Everything in a host VM shares that host VM's credentials, egress policy, and data. sbxr does not block privileged, `docker.sock`, or Docker-in-Docker workloads. It makes this shared scope clear, so users split trust by using separate host VMs.
- Enforce egress policy on the physical host, not in the guest. Deny traffic that the policy cannot classify, unless the user allows it. Document the precedence between domain and IP rules.
- sbxr reads its own settings only from CLI flags and the user's sbxr config. It never reads sbxr settings from project directories, because untrusted code can write there.

Tests:

- No fakes, mocks, or other test doubles. Pure logic (for example the egress policy matcher, the config parser, and the JSON-RPC codec) gets unit tests and fuzz tests. Everything else is tested against real host VMs, real `dockerd`, and real `containerd`.
- Tests must be reliable and fail loudly. An empty test selection or an unexpected skip is a failure.
- A separate `e2e/` Go module drives the real `sbxr` binary, the `docker` CLI, Compose, and `testcontainers-go`, as a user would.
- E2e runs on hosted `ubuntu-latest` (amd64 with KVM) and on self-hosted Apple Silicon and Linux arm64 runners. Hosted macOS and Linux arm64 runners cannot run the host VMs.
- Workflow tests share one host VM. Security, upgrade, and crash tests each get a fresh host VM. After the suite, no host VMs, processes, sockets, or Docker contexts are left behind.
- Tests are hermetic. They set `registry_mirrors` to a local mirror that holds every image they use, including `testcontainers/ryuk`, and use a test HTTPS server on a non-loopback address. A separate non-hermetic smoke test pulls real images through the default preset.
- Bypass tests cover at least: direct IP, IPv6, DNS over HTTPS and TLS, QUIC, SNI that differs from the `Host` header, vsock to the physical host (CID 2), route changes by guest root, and ports published by another host VM.
- Fault tests use real faults: `kill -9` of the daemon and of a host VM process, a paused host VM with a clock jump, a full disk, and corrupt state files. After `kill -9` of the daemon, host VMs keep running and get their network back. An intentional daemon stop stops all host VMs. An invalid config or an invalid referenced preset keeps the last applied policy during reload and keeps egress closed during crash recovery.
- Config tests check that parse, `fmt`, and parse again give the same result, that `fmt` is idempotent, that CLI edits keep comments, and that a write through a symlink keeps the symlink.
- Credential tests check each real source from the installed daemon after a restart without a CLI call, check a locked or unavailable keychain, and check that no secret value reaches logs, the audit log, or CLI output.
- On failure, collect the serial console, the guest journal, the daemon log, and the audit log as artifacts. Tests do not retry. A quarantined test needs a linked issue.
- CA isolation tests check that a container in one host VM rejects a certificate from another host VM's CA.
- Build trust tests use the embedded builder and a `docker-container` builder, with cache disabled. A passthrough domain works without the CA. A credential-rule domain fails without the build secret and works with it, for curl, Node, and Python Requests. Exported layers, image config, and provenance do not contain the CA bundle.
- Idle tests check that each activity in Operations keeps the host VM up past the timeout, and that a truly idle host VM shuts down.
- Memory and wake-time tests write a JSON result on every run.

Earlier findings. Treat them as leads to verify, not facts, because upstream projects change:

- libkrun's implicit vsock let a guest reach `127.0.0.1` on the physical host. Implicit vsock and TSI (libkrun's transparent socket impersonation) may expose similar paths.
- With default settings, gvproxy let one host VM reach ports that another host VM published on host loopback. Two gvproxy instances collided on one loopback port, and libkrun then panicked a vCPU.
- Clones of one booted base image shared `/etc/machine-id`.
- macOS `phys_footprint` over-counted libkrun guest memory, so it looked larger than the real cost.
- Docker failed to create containers with the containerd EROFS snapshotter. A Docker store on a macOS virtiofs share broke image-layer operations. The Docker and containerd stores worked on the VM's ext4 disk.
- Any Docker API user can set Docker labels, so labels could not safely grant credentials.
- Unix socket paths have a length limit (104 bytes on macOS and 108 bytes on Linux, including the trailing NUL). A 104-byte path failed on macOS.
- `net/http` recovers handler panics, so a panic inside a handler did not stop the process.

Repository rules:

- Follow `AGENTS.md`.
- Keep `LICENSE.md`. Copied Apache-2.0 files (for example from nerdbox or smolvm) keep their license headers. Modified copies also get a change notice.

## Stage 2: Kubernetes

### What we want

1. A containerd shim (`containerd-shim-sbxr-v2`) used as a Kubernetes `RuntimeClass` handler.
2. One VM per pod by default (many containers per VM).
3. Adapt mostly the nerdbox design (https://github.com/containerd/nerdbox).

### What we care about

- Do not build Stage 2 until asked. Avoid Stage 1 choices that would block it.
- Stage 1 rules apply, except those specific to the per-user daemon and the host VM.
- Linux nodes only.
- The shim runs standalone on the node. It does not depend on the per-user daemon from Stage 1.
- Reuse the `internal/host` packages and `internal/guest/agent` instead of building second copies.
- Add a Kubernetes Secrets credential source. There is no keychain on a node.
- Test against real Kubernetes and real `containerd`. No fakes.

Earlier findings. Treat them as leads to verify, not facts:

- When `dockerd` starts a containerd shim, it passes only `spec.Root.Path`, an already mounted overlay inside the VM kernel, not the image layers. Check what containerd passes through CRI before you design rootfs handling.
