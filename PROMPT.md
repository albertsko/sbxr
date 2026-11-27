## Stage 1: local product

### What we want

sbxr is a Go project. Stage 1 is a polished local product. Stage 2 (Kubernetes) comes later.

The main Stage 1 user is a developer who runs AI coding agents and other untrusted code in Docker. Defaults favor safety.

1. A local CLI (`sbxr`) and one per-user daemon on macOS arm64, Linux amd64, and Linux arm64. The daemon runs real Docker (`dockerd` and `runc`) inside an isolated Ubuntu 26.04 LTS libkrun microVM. Containers run as plain `runc` containers in that VM. There is no VM per container. The user reaches the VM through `docker context`.
2. Multiple host VMs: each host VM has a name, its own Docker context, CA (certificate authority), credentials, and egress policy.
3. Credentials through the egress proxy, like smolvm (https://github.com/smol-machines/smolvm). Real secret values and private CA keys stay on the physical host. A guest gets only placeholders and the public CA certificate it needs. Stage 1 injects HTTP API tokens (for example `Authorization` headers). The daemon resolves real values on demand from credential sources (see Credentials), so a daemon restart needs no CLI call.
4. Egress control per host VM. The user allows or denies destinations by domain (for example `example.com` and `*.example.com`) and by IP range (for example `203.0.113.0/24`). Rules match domains and IP ranges only, not URL paths. The policy covers all traffic that leaves the host VM, including image pulls by `dockerd`. DNS always goes to the resolver in sbxr: the network stack answers DNS on UDP and TCP port 53 itself, whatever the destination IP. Encrypted DNS (DoH, DoT, DoQ, and DNSCrypt) is ordinary egress and is denied unless a rule allows its endpoint. A new host VM denies all egress except an editable preset that lists exact registry domains for Docker Hub and common image registries, including their blob CDN domains.
5. Low cost when idle: after an idle timeout (default 15 minutes), the host VM shuts down. The first Docker API connection boots it again.
6. Host files: no shares by default. The user adds project directories per host VM, read-only or read-write.
7. Published ports listen only on loopback (`127.0.0.1` and `::1`) of the physical host. The daemon opens a host listener only for a port binding in a `ContainerCreate` request that passes through the Docker API proxy, and closes it when the container is removed. A plain guest `listen()` never creates a host listener. Any Docker API client in the host VM, including guest root, can create port bindings, but only on loopback and only within the port policy. sbxr rewrites an explicit host IP (for example `-p 0.0.0.0:8080:80`) to loopback and warns. The port policy is `ports.allow` (default `1024-65535`) and `ports.deny` in a host VM file, both live keys. Ports below 1024 need an explicit `ports.allow` entry and may still fail on the physical host. When a host port is outside `ports.allow`, inside `ports.deny`, or already in use, the Docker API proxy rejects the `ContainerCreate` request with a Docker-style error. sbxr never falls back to the wildcard address. A host VM cannot connect to the physical host's loopback or to ports that other host VMs publish.
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

- No Rust code in this repository. Normal builds and releases never compile libkrun. No upstream source ships a portable libkrun 1.x build for all three targets, so a separate CI workflow runs only when the pinned libkrun version changes. It builds the pinned tag once per target with a pinned Rust toolchain and minimal features (`make NET=1 BLK=1`, no GPU or sound), applies the patch files in `patches/libkrun/`, sets `@loader_path` install names, ad-hoc signs the macOS dylib, and publishes the files to this repository's releases under an annotated tag `libkrun-v<version>`, which the goreleaser release workflow ignores, with `SHA256SUMS` and GitHub artifact attestations. It checks each built library before publishing: exported `krun_*` symbols, the glibc floor on Linux, and the macOS deployment target. It prints the new `*_SHA256_*` values to paste into `mise.toml`. Everything else downloads and verifies those files.
- libkrun patches are files in `patches/libkrun/`, applied to the pinned tag. Do not use forks or git submodules. Use a third-party prebuilt only if it is the pinned version, has no extra shared-library dependencies, and has a verifiable checksum.
- Keep every version pin in one place, `mise.toml`: tools in `[tools]` (locked in `mise.lock`), and libkrun, libkrunfw (if used), the Ubuntu base image digest, the apt snapshot timestamp, the Ubuntu kernel, e2fsprogs, and the `docker-ce`, `containerd.io`, `docker-buildx-plugin`, and `docker-compose-plugin` versions in `[env]`. Checksums are `[env]` variables too, one per artifact and target (for example `LIBKRUN_SHA256_DARWIN_ARM64`, `LIBKRUN_SHA256_LINUX_AMD64`, and `LIBKRUN_SHA256_LINUX_ARM64`), and one per Docker `.deb` package. Do not add a separate lock file for artifacts. mise tasks, CI, goreleaser, and Go `-ldflags` read the pins only from `mise.toml`.
- `mise run deps` downloads each artifact into `$XDG_CACHE_HOME/sbxr/deps/<name>/<version>/<goos>-<goarch>/` and verifies it against its `*_SHA256_*` variable. The cache is shared by all worktrees. Go modules stay in `go.mod`.
- Load libkrun with `purego` (https://github.com/ebitengine/purego). Release binaries use `CGO_ENABLED=0`. Tests may use cgo for `-race`.
- Pin the latest stable release of each dependency. For libkrun, pin the latest stable 1.x release (for example v1.19.8 on 2026-11-28, but the pin in `mise.toml` wins), not 2.x. libkrun 1.x works only with libkrunfw 5.x. Patch libkrun only when a required behavior cannot be reached through its C API, and link each patch to an upstream issue.
- Build only for `darwin/arm64`, `linux/amd64`, and `linux/arm64`. Update `.goreleaser.yaml` to these three targets and bundle libkrun, the kernel, the initramfs, and the root image in the install layout (see Architecture).

Root image:

- Build it with BuildKit from a `Dockerfile` that starts with `FROM ubuntu:26.04@sha256:<index digest>`, the official Ubuntu image on Docker Hub. Install packages with `apt --snapshot <pinned snapshot.ubuntu.com timestamp>`, so builds are reproducible.
- Install Docker from Docker's apt repository for Ubuntu 26.04 (`resolute`): `docker-ce`, `containerd.io`, `docker-buildx-plugin`, and `docker-compose-plugin`, each pinned to the version in `mise.toml`. `apt --snapshot` does not cover `download.docker.com`, so verify each `.deb` against its `*_SHA256_*` variable in `mise.toml`.
- Add what the official image lacks: `systemd`, `systemd-sysv`, `kmod`, `nftables` and `iptables`, `ca-certificates`, and the `linux-modules` package that matches the pinned Ubuntu kernel.
- Keep the image simple and clean. In the same build step, remove apt lists and caches, apt and dpkg logs, `/usr/sbin/policy-rc.d`, and the official image's `docker-*` apt configs.
- Build with `SOURCE_DATE_EPOCH` and BuildKit `rewrite-timestamp`. CI builds the root image twice and fails if the hashes differ.
- Export the result as a rootfs tar and convert it into a read-only ext4 image with `mke2fs -d <tarball>` (pinned e2fsprogs 1.47.1 or later) inside the Linux build container, so ownership and xattrs survive. Set `-U`, `-E hash_seed=`, and `E2FSPROGS_FAKE_TIME` for a fixed UUID, hash seed, and time. Boot it as a virtio-blk root device. Do not boot the image as a krunvm-style virtiofs root: that path has no systemd, and Docker stores fail on virtiofs.
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
- `defaults:` may hold only `cpus`, `memory`, `disk_cap`, `idle_timeout`, `registry_mirrors`, and `ca_trust.inject`. A host VM value replaces the default. Lists are never merged.
- Built-in presets ship with sbxr. A user preset with the same name as a built-in preset is an error.
- State: `$XDG_STATE_HOME/sbxr`, default `~/.local/state/sbxr` (disks, audit log, CA keys). Cache: `$XDG_CACHE_HOME/sbxr`.
- Sockets: `$XDG_RUNTIME_DIR/sbxr` on Linux and `~/.sbxr/run` on macOS, in a `0700` directory owned by the user. Check that each socket path fits the length limit.
- The config files are the single source of truth. The daemon writes them when the CLI asks for a change. Read-only commands (`path`, `validate`, `fmt --check`, `show --effective`) also work without the daemon.
- The schema marks each key as live or restart-required. The daemon applies live keys (for example egress rules) without a host VM restart. `sbxr config status` shows, per host VM, whether the config is applied, rejected, or waiting for a restart.
- Provide `sbxr config path`, `edit`, `validate`, `fmt [--check]`, `show --effective`, `status`, and `migrate`.

Config file format:

- Use YAML with the latest stable `go.yaml.in/yaml/v4`. Read each file into a `yaml.Node`, then decode it into typed Go structs with known-field checks. Write by editing the `yaml.Node` tree, so comments survive and every value keeps its `file:line` for `config show --effective` and errors. Merge `defaults:`, host VM files, and presets in typed Go code. Do not use koanf, `sigs.k8s.io/yaml`, or another map-based or JSON-based layer, because they drop comments, positions, and key order. Generate the JSON Schema from the same structs with `github.com/invopop/jsonschema` (`FieldNameTag: "yaml"`).
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
- The private CA key stays on the physical host and never enters a guest. Each host VM's CA uses ECDSA P-256 and lasts as long as the host VM, so running containers and exported bundles stay valid across daemon restarts.
- The default CA key store is a `0600` file in a `0700` per-host-VM directory in the state directory, created with `O_EXCL` and `O_NOFOLLOW` and excluded from backups. The public certificate lives in a separate directory, the only one that sbxr mounts or exports. Other processes of the same user can often read a login keychain item without a prompt, so the keychain adds little against sbxr's threat model.
- `ca_key_store: keychain` in a host VM file selects the OS keychain (macOS Keychain or Linux Secret Service, through a cgo-free library). `ca_key_store` is restart-required and not allowed in `defaults:`. When the user changes it, the daemon moves the key to the new store at the next host VM start. sbxr never switches stores silently. When the configured store is locked, missing, or unreachable, the daemon does not decrypt TLS for that host VM, passthrough keeps working, and `sbxr status` names the cause and the fix.
- `sbxr ca rotate <host>` creates a new CA, updates the bundles, and lists the containers that need a restart. A lost key needs a rotation, never a silent new CA.
- Stage 1 does not name-constrain the CA to credential domains, because credential rules are live config. Revisit this in a decision record at the credentials milestone. TPM and Secure Enclave key stores may come later behind the same key store interface.
- The egress proxy decrypts TLS only for domains that have a credential rule. For each connection, the destination name is the SNI for TLS (with Encrypted Client Hello, the outer SNI) and the `Host` header for plain HTTP. Without a visible name, the name comes from the DNS answers the resolver served to that host VM for the destination IP. A visible name counts only if the destination IP is in the answers the resolver served to that host VM for that name. Otherwise the egress proxy treats the connection as unclassified. Requests within an allowed encrypted connection can reach other origins served by the same endpoint.
- The sbxr resolver answers only names that the policy allows, or names whose answers all fall in allowed IP ranges. It refuses other names and records them in the audit log. It removes the `ech` parameter from HTTPS and SVCB answers, so clients send a plain SNI. The map from served IP addresses to names is per host VM and expires with the record TTL.
- UDP egress other than DNS reaches only allowed IP ranges, never a destination allowed by domain, so QUIC to a domain falls back to TCP.
- Before injecting a credential, check every decrypted request against the credential domain and TLS SNI. Deny the connection if the request names a different host. For HTTP/2, check `:authority` and reject a conflicting `Host` header.

CA trust in containers:

- Credential injection needs the container to trust the host VM's CA, but container images have their own trust stores. sbxr sets up this trust by default, so injection does not fail silently.
- Inside the host VM, `dockerd` listens on a private socket. The Docker API proxy owns `/var/run/docker.sock`. Containers that mount `docker.sock` go through the same proxy.
- The Docker API proxy changes only `ContainerCreate` requests. It bind-mounts, read-only, a per-container directory at `/run/sbxr` that holds the host VM's CA (`ca.pem`) and a bundle (`ca-bundle.pem`). It mounts a directory, not files, so `sbxr ca rotate` can replace the files in place. Like smolvm, the guest agent builds the bundle inside the host VM at `ContainerCreate` from the image's own trust roots followed by the host VM's CA, and deletes it when the container is removed. It reads the image's roots, resolving symlinks inside the image root, from the first existing path among `/etc/ssl/certs/ca-certificates.crt`, `/etc/pki/tls/certs/ca-bundle.crt`, `/etc/ssl/ca-bundle.pem`, `/etc/pki/tls/cacert.pem`, and `/etc/ssl/cert.pem`. When the image has none, it uses the Mozilla roots (Mozilla's NSS root store, as shipped in the root image's pinned `ca-certificates` package). It never changes the image's own trust store.
- Variables that replace the default trust store (`SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`, `PIP_CERT`, `GIT_SSL_CAINFO`, `AWS_CA_BUNDLE`, and `CARGO_HTTP_CAINFO`) point to the bundle. Variables that add to it (`NODE_EXTRA_CA_CERTS` and `DENO_CERT`) point to `ca.pem`. The proxy never overrides a variable set by the request or by the image config. Java keystores and clients with built-in roots are not covered. The docs show how to add `ca.pem` to them.
- `ca_trust.inject` in a host VM file (default `true`, allowed in `defaults:`) turns CA injection on or off for the host VM. The container label `dev.sbxr.ca-trust` (`on` or `off`) overrides it for one container. sbxr ignores image labels. Labels are user-controlled, which is fine here: the CA certificate is public, and the label affects only that container.
- The Docker API proxy is a convenience, not a security control. Guest root can bypass it, and it holds no secrets.
- `sbxr doctor --container <id>` shows what the Docker API proxy injected and which variables it skipped because the request or the image set them.
- Build steps do not inherit container CA trust. Builds that use only passthrough domains need no change.
- For a `RUN` step that contacts a credential-rule domain, `sbxr ca export <host>` writes the public CA bundle (Mozilla roots plus the host VM's CA) to a file outside the build context. The user mounts it with a BuildKit build secret (`--secret id=sbxr-ca,...` and `RUN --mount=type=secret,id=sbxr-ca`). Document Docker and Compose (`build.secrets`) examples.
- If a Testcontainers client cannot pass build secrets, build the image first with `docker buildx build` and use its local tag.
- Do not change build requests, wrap the build executor's `runc`, or install the CA in image layers.

UX:

- Use `cobra` for the CLI and follow https://clig.dev/. sbxr is a CLI only. A TUI or web UI may come later, so do not use `bubbletea`, `bubbles`, `huh`, or `glamour`.
- Run cobra through `charm.land/fang/v2` for styled help, styled errors, `--version`, man pages, and completions. fang is young, so wrap it in one small function in `cmd/` that can fall back to plain cobra. Provide a custom error handler that prints JSON errors on stderr when `--json` is set. sbxr maps errors to its documented exit codes.
- Use `charm.land/lipgloss/v2` (with `lipgloss/table`) and `github.com/charmbracelet/colorprofile` for human output.
- Data goes to stdout. Logs, progress, prompts, and errors go to stderr. Style output only when the stream is a terminal, `NO_COLOR` is unset, and `TERM` is not `dumb`. `--json` and NDJSON output never have styling, progress, or prompts.
- Prompts are plain lines. They appear only when stdin and stderr are terminals, `--json` and `--yes` are not set, and `CI` is unset. Otherwise the command fails with a usage error that names the missing flag. Every prompt has a flag equivalent.
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
  - `internal/assert` and `internal/logging`: shared foundations.
- Enforce package boundaries with `depguard` rules in `.golangci.yml`:
  - `internal/proto` imports only the standard library.
  - `internal/host` and `internal/guest/agent` never import `internal/local` or `internal/guest/dockerproxy`.
  - `internal/guest` never imports `internal/host` or `internal/local`.
  - Mock libraries are banned everywhere.
  - `internal/assert` and `internal/logging` import only the standard library.
  - `charm.land/...` and `github.com/charmbracelet/...` modules are allowed only in `cmd/` and `internal/local/cli`.
  - koanf and `sigs.k8s.io/yaml` are banned.
- Repository tasks are discoverable through `mise run`, give fast local feedback, and fail with clear errors.
- `mise run ci` runs on hosted runners without VMs, finishes in under 5 minutes, and also vets and lints `e2e/`.
- `mise run test:e2e` runs against real host VMs.
- `mise run image:build` runs the root image build through any Docker context with BuildKit or a standalone `buildkitd`, or downloads the released root image for the pinned hash. Release CI builds the root image for both architectures. It caches the root image by content hash.
- `mise run dev` cross-compiles the guest agent and replaces it in a running host VM.
- `mise run build` writes `bin/sbxr` and links `libexec/sbxr/` to the files in the deps cache, so development builds use the release layout.
- To test a local libkrun, set `LIBKRUN_SRC` to a checkout in the gitignored `mise.local.toml` and run `mise run deps:libkrun:local`. It applies `patches/libkrun/`, builds into the deps cache with version `local-<commit>`, and makes `mise run build` use it. Only this task and the libkrun CI workflow install the pinned Rust toolchain.
- On macOS, `mise run build` ad-hoc signs the binary with the `com.apple.security.hypervisor` entitlement. Ad-hoc signed release binaries are fine for Stage 1. Releases run on Linux, so sign with `rcodesign` or add a macOS release job.
- Deliver in milestones: boot, Docker context, egress, credentials, idle and wake, Phase B. Each milestone ends with a demo and a decision record in `docs/decisions/`.
- When rules conflict or you are blocked, start independent investigator subagents, one per question, each with a fresh context. Act as the coordinator: compare their findings, choose the best solution, and record it in `docs/decisions/`. Never resolve a conflict by weakening a Security rule or an `AGENTS.md` rule. Ask the user about those. Without a subagent tool, investigate each question separately, one after another.

Operations:

- Each host VM gets small default CPU and memory. The user can change them per host VM.
- The data disk is sparse, has a size cap, and returns freed space to the physical host. `sbxr` shows disk usage.
- Root image updates are explicit (`sbxr host upgrade`). An upgrade keeps the host VM's machine ID, images, volumes, and other Docker data. In `/etc`, local edits and deletions survive an upgrade, and untouched files pick up new defaults. sbxr warns when the installed sbxr release bundles a newer root image than a host VM runs. This check is local and makes no network calls.
- Physical host sleep, resume, and network changes need no host VM restart. The guest clock resyncs. The guest keeps the same resolver address. The daemon's resolver forwards to the physical host's current DNS servers (macOS `SCDynamicStore`, Linux `/etc/resolv.conf`), and egress keeps working.
- Shut down a host VM only when it is really idle. Idle means no running containers, builds, exec or attach sessions, open Docker API connections (including event and log streams), and open connections on published ports for the whole timeout.
- Keep the physical host's Docker API listener open through host VM shutdown and wake. Serialize start and stop decisions per host VM so requests queued during shutdown share one wake.
- The guest agent reports guest-side activity. The daemon reports physical-host-side activity. When either signal is missing or unclear, the host VM stays up.
- `sbxr host status` shows why a host VM is not idle and when it will shut down.
- A local audit log records allowed and denied connections and credential injections. The user can query it with `sbxr`. It never contains real secret values.
- No phone-home.

Assertions and logging:

- Build `internal/assert` and `internal/logging` first.
- Assert invariants that sbxr's own code guarantees. Return errors for anything that crosses a boundary: user input, files, network, the guest, FFI, and time.
- Assertions stay on in release builds.
- `assert.That`, `assert.Equal`, and `assert.Unreachable` take a static message and key/value details. `cmd/` opens a crash file, passes it to `debug.SetCrashOutput` for runtime crashes, and gives it to `internal/assert`. A failed assertion writes the message, the details, and the stack to stderr and to that file, then calls `os.Exit(70)`. Exit code 70 is in the documented exit codes. It never panics, so `net/http` recovery cannot hide it. Test the package in a subprocess.
- `os.Exit` skips deferred calls, so state writes are atomic (write a temporary file, then rename it).
- Packages take a `*slog.Logger` and never use the global logger. Only `cmd/` chooses the handler: JSON for the daemon log, text on stderr for the CLI, and `t.Output()` in tests. Logs never go to stdout. Each host VM logger carries the host VM name. Log keys are constants in `internal/logging`, and `logging.Secret` redacts a value.
- The daemon restarts crashed host VM processes. A restarted daemon rebuilds its state from disk and from the running host VM processes.

Go style:

- Write idiomatic Go. Follow Effective Go (https://go.dev/doc/effective_go) and Go Code Review Comments (https://go.dev/wiki/CodeReviewComments).
- Use short, lowercase package names without stutter (`config.Hosts`, not `config.ConfigHosts`). Do not name a package after a standard library package.
- Accept interfaces and return concrete types. Define an interface in the package that uses it, and only when two real implementations exist (for example `creds.Source` and `ca.KeyStore`).
- Pass `context.Context` as the first argument. Wrap errors with `%w` and check them with `errors.Is` and `errors.As`.
- No magic strings. Use named types (for example `HostName`, `LabelKey`, and `EnvVar`) with constants in the package that owns them. Validate a value once where it enters, for example with `ParseHostName`. Generate enums with `enumer` through the `tool` directive in `go.mod`. YAML struct tags are the single source of config key names.
- Use the repository pattern for data on disk, as concrete types: for example `config.Hosts`, `config.Presets`, `state.VMs`, and `audit.Log`. They hide atomic, comment-preserving, and symlink-safe writes. Do not write generic repositories or interfaces that exist only for tests.
- golangci-lint enforces this: `sloglint` (`no-raw-keys`, `no-global: all`, `static-msg`, `key-naming-case: snake`), `exhaustive`, `goconst`, and `forbidigo` for `recover` and `os.Exit`. Allow `os.Exit` in `cmd/` and `internal/assert` with path rules in `linters.exclusions.rules`. The one `recover` that re-panics `http.ErrAbortHandler` carries `//nolint:forbidigo` with a reason.

Architecture:

- Run exactly one sbxr daemon per user on the physical host. It owns and supervises everything on the physical host.
- The CLI is a thin client. It changes state only through daemon calls.
- `sbxr init` installs one launchd or systemd user unit for the daemon, without `sudo`. The unit restarts the daemon when it crashes. The CLI also starts the daemon on demand when the unit is missing.
- Host VMs survive a daemon crash within the same login session. An intentional daemon stop or uninstall stops all host VMs. Choose the launchd and systemd settings that give both, and test both through the installed unit.
- The daemon runs each host VM in a child process (`sbxr __vm <name>`), because `krun_start_enter` takes over the process that calls it. The child process runs only libkrun. It holds no policy, secrets, or network logic.
- A restarted daemon adopts the running host VM processes after it verifies each one's identity from state on disk. A PID alone is not enough. Open guest connections reset during the restart. Document this.
- libkrun's unix socket network backends may disable networking when their peer goes away, so recreating the socket path may not reconnect. Before you build the daemon, prove recovery with the pinned libkrun on macOS and Linux: kill the daemon during traffic, then check that the guest boot ID is unchanged, egress stays closed during recovery, and new allowed connections work afterward. If recovery needs a process that keeps the socket open or a libkrun patch, investigate as described in DevEx and record the decision.
- The CLI talks to the daemon over a local unix socket using JSON-RPC 2.0.
- sbxr never runs a VM inside a host VM. CI may run sbxr on runners where KVM itself is nested.
- Real `dockerd` inside the host VM. Do not reimplement the Docker Engine API. The Docker API proxy, which changes requests, is allowed.
- Use systemd as init in the host VM. The `docker-ce` and `containerd.io` packages ship systemd units, and on cgroup v2 `dockerd` uses the systemd cgroup driver when systemd is available. The guest agent runs as a systemd service.
- Use the Ubuntu kernel by default, booted through libkrun's external kernel support (`krun_set_kernel`), and do not ship libkrunfw. libkrunfw 5.x (GPL-2.0 and LGPL-2.1) is an allowed option when measurements or boot problems show that it helps. Record the choice in a decision record. The first boot spike checks that libkrun boots the Ubuntu kernel without libkrunfw installed on macOS and Linux, that libkrun accepts the Ubuntu arm64 kernel image format, and which virtio modules the initramfs must load. If the spike fails, fall back to libkrunfw 5.x first. A minimal non-Ubuntu kernel comes after that and needs measurements and the user's approval.
- Every install uses one layout: `<prefix>/bin/sbxr` and `<prefix>/libexec/sbxr/`, which holds libkrun, libkrunfw (if used), the kernel, the initramfs, and the root image. Development builds, release tarballs, and Homebrew use the same layout.
- sbxr finds `libexec/sbxr/` from the resolved path of its executable (`os.Executable`, then `filepath.EvalSymlinks`, then `../libexec/sbxr`). It never searches `PATH` and never reads library paths from environment variables, because a library path runs code.
- Load libraries with `purego.Dlopen` and absolute paths. When libkrunfw is present, load it with `RTLD_NOW|RTLD_GLOBAL` before libkrun. Bind every required `krun_*` symbol at load time from one struct (like nerdbox's `internal/vm/libkrun`), and fail with an error that names each missing symbol and the expected libkrun version.
- macOS dylibs use `@loader_path` install names and are ad-hoc signed. Never rely on `DYLD_*` or `LD_LIBRARY_PATH` variables. If releases later use the hardened runtime, the dylibs need the same Team ID or the `com.apple.security.cs.disable-library-validation` entitlement.
- Networking uses virtio-net connected to a userspace network stack in the daemon, one instance per host VM. Egress enforcement and credential injection run in the daemon too. Do not use TSI, even with libkrunfw: libkrun's TSI port mapping exposes guest listening ports on the physical host. Call `krun_disable_implicit_vsock()` and add only the vsock ports that sbxr needs.
- For each port binding, the Docker API proxy asks the daemon to bind the host port before it forwards `ContainerCreate`. For an empty host port, the daemon picks and binds a free port, and the proxy writes it into the request, so `docker inspect` shows the real host port. IPv6 (`::1`) is best effort on the same port number. `sbxr host status` lists active published ports.

Memory:

- Minimize the memory growth that each running host VM adds. Target 384 MiB, hard limit 512 MiB. Both apply to measured growth, not to the configured size.
- Measure system-wide used memory growth on the physical host. On macOS, use the htop 3.5.3 Mem-used formula: (wired + max(anonymous - purgeable, 0) + compressor) times page size. No Linux method is chosen yet. Choose one and document it.
- Show per-process resident memory (RES) in `sbxr` status, but do not use it as the gate.
- Use libkrun resource features such as the memory balloon and free page reporting. smolvm is an example that probably does this well.

Security:

- The host VM, not the container, is the hard trust boundary.
- A guest never sees real secret values or private CA keys.
- Everything in a host VM shares that host VM's credentials, egress policy, and data. sbxr does not block privileged, `docker.sock`, or Docker-in-Docker workloads. It makes this shared scope clear, so users split trust by using separate host VMs.
- Enforce egress policy on the physical host, not in the guest. Deny traffic that the policy cannot classify, unless the user allows it. Rule precedence: first the fixed denies (the physical host's loopback and ports that other host VMs publish), then deny rules, then allow rules, then deny everything else. A deny rule beats an allow rule, whether it matches by domain or by IP range. Document this precedence.
- sbxr reads its own settings only from CLI flags and the user's sbxr config. It never reads sbxr settings from project directories, because untrusted code can write there.

Tests:

- No fakes, mocks, or other test doubles. Pure logic (for example the egress policy matcher, the config parser, and the JSON-RPC codec) gets unit tests and fuzz tests. Everything else is tested against real host VMs, real `dockerd`, and real `containerd`.
- Tests must be reliable and fail loudly. An empty test selection or an unexpected skip is a failure.
- A separate `e2e/` Go module drives the real `sbxr` binary, the `docker` CLI, Compose, and `testcontainers-go`, as a user would.
- E2e runs on hosted `ubuntu-latest` (amd64 with KVM) and on self-hosted Apple Silicon and Linux arm64 runners. Hosted macOS and Linux arm64 runners cannot run the host VMs.
- Workflow tests share one host VM. Security, upgrade, and crash tests each get a fresh host VM. After the suite, no host VMs, processes, sockets, or Docker contexts are left behind.
- Tests are hermetic. They set `registry_mirrors` to a local mirror that holds every image they use, including `testcontainers/ryuk`, and use a test HTTPS server on a non-loopback address. A separate non-hermetic smoke test pulls real images through the default preset.
- Bypass tests cover at least: direct IP, IPv6, DNS over HTTPS and TLS, QUIC, SNI that differs from the `Host` header, vsock to the physical host (CID 2), route changes by guest root, ports published by another host VM, Encrypted Client Hello with a hard-coded config, DoH to an IP address without SNI, an SNI or `Host` header that names an allowed domain toward an unrelated IP, and a guest root `listen()` or `--net=host` listener that must not appear on the physical host.
- Port tests check that a host port conflict and two host VMs that publish the same port fail with an error and no VM crash, and check privileged ports on macOS and Linux.
- Fault tests use real faults: `kill -9` of the daemon and of a host VM process, a paused host VM with a clock jump, a full disk, and corrupt state files. After `kill -9` of the daemon, host VMs keep running and get their network back. An intentional daemon stop stops all host VMs. An invalid config or an invalid referenced preset keeps the last applied policy during reload and keeps egress closed during crash recovery.
- Config tests check that parse, `fmt`, and parse again give the same result, that `fmt` is idempotent, that CLI edits keep comments, and that a write through a symlink keeps the symlink.
- Credential tests check each real source from the installed daemon after a restart without a CLI call, check a locked or unavailable keychain (for credential sources and for the `keychain` CA key store), and check that no secret value reaches logs, the audit log, or CLI output.
- On failure, collect the serial console, the guest journal, the daemon log, and the audit log as artifacts. Tests do not retry. A quarantined test needs a linked issue.
- CA isolation tests check that a container in one host VM rejects a certificate from another host VM's CA.
- Build trust tests use the embedded builder and a `docker-container` builder, with cache disabled. A passthrough domain works without the CA. A credential-rule domain fails without the build secret and works with it, for curl, Node, and Python Requests. Exported layers, image config, and provenance do not contain the CA bundle.
- Idle tests check that each activity in Operations keeps the host VM up past the timeout, and that a truly idle host VM shuts down.
- Starting in Phase A, measure memory growth with repeated paired VM-off and idle VM-on samples on a quiet physical host. Report the variation alongside the measured growth. Memory and wake-time tests write a JSON result on every run.

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
