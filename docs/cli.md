# sbxr CLI reference

`sbxr` runs an OCI image as a small virtual machine with its own Linux kernel, using `libkrun`. It works on macOS arm64 (Hypervisor.framework) and on Linux with KVM, without root.

## Commands

| Command                                 | Purpose                                                                     |
| --------------------------------------- | --------------------------------------------------------------------------- |
| `sbxr run [flags] IMAGE [CMD [ARG...]]` | Run a workload in a new VM, in the foreground, and remove the VM after exit |
| `sbxr doctor [--json]`                  | Check the machine and print fix commands                                    |
| `sbxr doctor apparmor-profile`          | Print the AppArmor profile that lets `sbxr` use user namespaces (Linux)     |
| `sbxr prune [--force]`                  | Remove cached images and leftover run state                                 |
| `sbxr completion SHELL`                 | Print a shell completion script                                             |
| `sbxr --version`, `sbxr -v`             | Print the version                                                           |

`sbxr` without arguments prints a short usage text with example `run` commands.

Global flags: `--debug` (debug logs on stderr) and `--no-color`.

## `sbxr run`

### Flags

| Flag                            | Default                       | Meaning                                                                                                   |
| ------------------------------- | ----------------------------- | --------------------------------------------------------------------------------------------------------- |
| `--cpus N`                      | 2, at most the host CPU count | vCPUs, from 1 to the host CPU count                                                                       |
| `--memory SIZE`                 | `1G`                          | Guest RAM, at least 1 MiB and a whole number of MiB. A bare number is bytes, so use `512M` or `2G`        |
| `-e, --env KEY=VALUE`           | none                          | Set an env var. Repeatable                                                                                |
| `--env-file PATH`               | none                          | Read `KEY=VALUE` lines from a file. Empty lines and lines that start with `#` are skipped. Repeatable     |
| `-v, --volume HOST:GUEST[:ro]`  | none                          | Share a host directory at an absolute guest path. `:ro` makes it read-only. Repeatable                    |
| `--net`                         | off                           | Allow outbound IPv4 to public addresses (see "Network")                                                   |
| `--allow-lan`                   | off                           | Also allow LAN addresses. Requires `--net`                                                                |
| `-p, --publish [IP:]HOST:GUEST` | none                          | Publish a guest TCP port. Binds `127.0.0.1` unless an IPv4 address is given. Requires `--net`. Repeatable |
| `-w, --workdir PATH`            | image value, else `/`         | Working directory in the guest. Must be absolute                                                          |
| `-u, --user UID[:GID]`          | image value                   | Numeric user in the guest. Without `:GID`, the group is 0                                                 |
| `--entrypoint CMD`              | image value                   | Replace the image entrypoint and drop the image `Cmd`. An empty value removes the entrypoint              |
| `--pull missing\|always\|never` | `missing`                     | When to download the image                                                                                |
| `-i, --interactive`             | off                           | Pass stdin to the workload                                                                                |
| `-t, --tty`                     | off                           | Give the workload a terminal                                                                              |
| `-q, --quiet`                   | off                           | Hide progress output                                                                                      |

Flags after `IMAGE` belong to the workload. A `--` right after `IMAGE` is optional and dropped.

`sbxr run` checks every flag and value before it downloads anything or starts a VM. Invalid input fails with exit code 125.

### Workload

- The image `Entrypoint`, `Cmd`, `Env`, `WorkingDir`, and `User` apply. Arguments after `IMAGE` replace `Cmd`.
- `sbxr` does not resolve commands on the host. The guest init searches the image `PATH`.
- A `User` name from the image is resolved from the image's `/etc/passwd` and `/etc/group`. An unknown name fails with exit code 125.
- The workload gets no supplementary groups. This differs from `docker run`.
- The guest env holds only the image env, the `--env-file` entries, and the `-e` entries, in this order. A later entry with the same name wins. `HOME=/`, `TERM=linux`, and a default `PATH` are added when missing. The host env never reaches the guest.
- Env names must start with a letter or underscore and contain only letters, digits, and underscores.
- Names that start with `KRUN_`, and the name `HOSTNAME`, are reserved by the guest init. `-e` and `--env-file` reject them. Image env entries with such names, or with invalid names or values, are dropped with a warning.
- Arguments, env values, the working directory, and volume paths must be valid UTF-8 without control characters other than tab and newline.
- A uid or gid of 4294967295 is rejected.
- Too many arguments and env entries for the guest init fail with exit code 125.
- Each run starts from a fresh copy of the cached image. Changes that a workload makes to its root never reach the cache or other runs.
- An image without a variant for the host architecture fails and names the available platforms. `sbxr` does not emulate other architectures.
- On a case-insensitive file system, such as the default on macOS, an image with paths that differ only by letter case fails.

### Volumes

- A relative `HOST` path starts from the current directory. `HOST` must be an existing directory.
- `GUEST` must be absolute and not `/`. Each guest path may appear once.
- These guest paths and everything below them are reserved: `/dev`, `/proc`, `/sys`, `/.sbxr-mount`, `/.krun_config.json`, `/init.krun`.
- A missing mount point is created as a directory. A mount point inside an earlier volume is created in that volume, so it appears in the host directory. Under a `:ro` volume it cannot be created, and the run fails with exit code 125.
- Volumes are mounted with `nosuid` and `nodev`. `:ro` is enforced by the host and by the guest mount.
- On macOS, every guest user sees itself as the owner of the volume files and can read and write them. Files that the guest creates belong to the invoking user on the host, and the guest cannot change ownership.
- On Linux, guest root is the invoking user. Its new files belong to the invoking user. Other guest users map to the user's subordinate ids: they see the user's files as owned by root, get only the "other" permission bits, and their new files belong to subordinate ids on the host.

### Standard streams and terminals

- Without `-t`, workload stdout goes to stdout and workload stderr to stderr.
- With `-t`, the workload has a terminal, and its stdout and stderr both go to stdout. Terminal resizes reach the guest only on Linux.
- Without `-i`, the workload reads an empty stdin and never reads the user's terminal.
- `-i -t` requires stdin to be a terminal.
- With `-i -t`, Ctrl-C goes to the guest terminal, which turns it into SIGINT unless the workload reads it as input.
- With `-i -t`, the guest cannot take Ctrl-C before the workload writes its first output, or while a write of its output to stdout waits for the reader. A Ctrl-C then stops the run with exit code 130, even when the workload would read it as input.

### Exit codes

| Code          | Meaning                                                     |
| ------------- | ----------------------------------------------------------- |
| workload code | The workload ran and exited with this code                  |
| 125           | `sbxr` failed: invalid input, preflight, image, or VM setup |
| 126           | The command exists in the image but cannot run              |
| 127           | The command does not exist in the image                     |
| 129           | Stopped by SIGHUP, for example a closed terminal            |
| 130           | Interrupted by SIGINT (Ctrl-C)                              |
| 141           | The reader of stdout went away, like SIGPIPE in a pipeline  |
| 143           | Stopped by SIGTERM                                          |

A workload that exits 125, 126, or 127 cannot be told apart from an `sbxr` failure by the exit code alone. An `sbxr` failure always prints an error line that starts with `sbxr:` on stderr. A workload exit code, including a non-zero one, never prints an `sbxr` error.

A signal stops the VM at once. The workload gets no graceful shutdown.

The other commands exit 0 on success, 1 on failure, and 2 on a usage error. `sbxr doctor` exits 1 when any check fails.

## Output

- stdout carries only workload output (`run`) or the command result (`doctor`, `doctor --json`, `doctor apparmor-profile`).
- stderr carries logs, progress, warnings, and errors.
- On success, `run` prints only the pull progress (see below) and warnings, such as `dropped image env "KEY=VALUE": REASON` for an image env entry that it cannot pass to the guest. With `--debug`, each published port is logged too, for example `published 127.0.0.1:8080 -> 80/tcp`.
- Progress shows only when stderr is a terminal and `-q` is not set: one line, `Pulling IMAGE: layer N/M, X MB`, redrawn in place and cleared when the pull ends.
- Output has no ANSI escape codes when stdout is not a terminal, when `NO_COLOR` is set and not empty, or with `--no-color`.
- Help and errors never wait for the terminal to answer color queries.

### Errors

An error is one line on stderr:

```text
sbxr: <source> "<value>": <problem>; <fix>
```

The value and the fix are optional. Values are quoted, so control characters are visible. Examples:

```text
sbxr: -e "KRUN_INIT=/x": KRUN_* names are reserved by the VM init; rename the variable
sbxr: -w "app": must be absolute; for example /app
sbxr: -p "8080:80": requires --net
sbxr: vm: the VM process was killed by signal 9; run again with --debug and report a bug
```

## Environment variables

| Variable         | Meaning                                                                     |
| ---------------- | --------------------------------------------------------------------------- |
| `SBXR_LIB_DIR`   | Directory with `libkrun` and `libkrunfw`                                    |
| `SBXR_CACHE_DIR` | Cache directory. Default: the platform user cache directory plus `sbxr`     |
| `SBXR_LOG_LEVEL` | `debug`, `info`, `warn`, or `error`. Default `warn`. `--debug` overrides it |
| `NO_COLOR`       | Disable colors when set and not empty                                       |

Flags win over environment variables. `sbxr` has no configuration file.

Without `SBXR_LIB_DIR`, `sbxr` uses the first directory that holds both `libkrun` and `libkrunfw`:

- macOS: `/opt/homebrew/lib`, `/usr/local/lib`.
- Linux: `/home/linuxbrew/.linuxbrew/lib`, `/usr/local/lib`, the multiarch directory (`/usr/lib/x86_64-linux-gnu` or `/usr/lib/aarch64-linux-gnu`), `/usr/lib64`, `/usr/lib`.

`sbxr` loads `libkrun` by its ABI 1 file name (`libkrun.1.dylib`, `libkrun.so.1`) as an absolute path, so it never loads a `libkrun` 2.x install.

## `sbxr doctor`

`sbxr doctor` runs every check of the platform, in this order, and prints one line per check. Each failed check has a fix command. When more than one check fails, the report ends with all fixes together. On Linux, a failed report also ends with the setup block for a fresh Ubuntu 24.04 machine from the README.

`sbxr run` runs the same checks first. When a check fails, `sbxr run` prints the first failure and exits 125 before any VM process starts.

| Check ID      | Platform | Passes when                                                                                                          |
| ------------- | -------- | -------------------------------------------------------------------------------------------------------------------- |
| `hypervisor`  | all      | `kern.hv_support` is 1 (macOS), or `/dev/kvm` opens read-write and reports KVM API version 12 (Linux)                |
| `libkrun`     | all      | The ABI 1 library loads from the library directory and exports every required symbol                                 |
| `libkrunfw`   | all      | The library loads, reports ABI version 5, and its resolved path shows version 5.6.4 or later                         |
| `entitlement` | macOS    | The running binary is signed with the `com.apple.security.hypervisor` entitlement                                    |
| `userns`      | Linux    | Inside a new user namespace with the user's subordinate ids, a chown to uid 0 and gid 42 and a private mount succeed |
| `cache`       | all      | The cache directory is writable                                                                                      |

When the `userns` check fails while AppArmor restricts unprivileged user namespaces (Ubuntu 23.10 and later), the fix depends on the resolved path of the running binary. For `/usr/bin/sbxr` or `/usr/local/bin/sbxr`, it prints the profile install commands:

```sh
sbxr doctor apparmor-profile | sudo tee /etc/apparmor.d/sbxr && sudo apparmor_parser -r /etc/apparmor.d/sbxr
```

For any other path, it tells the user to install `sbxr` to `/usr/local/bin`, because the profile covers only those two root-owned paths.

### JSON output

`sbxr doctor --json` prints the report as JSON on stdout. The schema is [doctor.schema.json](doctor.schema.json).

```json
{
  "schemaVersion": 1,
  "ok": false,
  "checks": [
    { "id": "hypervisor", "status": "pass", "detail": "Hypervisor.framework available" },
    {
      "id": "libkrun",
      "status": "fail",
      "detail": "libkrun.1.dylib and libkrunfw.5.dylib not found in /opt/homebrew/lib, /usr/local/lib",
      "fix": "brew install libkrun"
    }
  ]
}
```

`status` is `pass` or `fail`. `fix` is present only when `status` is `fail`. `ok` is true when every check passed.

## Network

Without `--net`, the VM has no network device and no socket forwarding. The workload can neither connect out nor listen on the host.

With `--net`, the guest gets a virtio-net device connected to a userspace network stack inside the `sbxr run` process. The guest configures itself with DHCP and uses the gateway for DNS. `libkrun` socket forwarding (TSI) is never used.

Guest addresses are fixed: subnet `192.168.127.0/24`, gateway `192.168.127.1`, guest `192.168.127.2`. A host LAN in the same range is not reachable from the guest, even with `--allow-lan`.

### Egress policy

`sbxr` checks every new TCP connection and UDP flow from the guest:

- Always blocked:
  - `0.0.0.0/8`, `127.0.0.0/8`, `169.254.0.0/16` (including cloud metadata), `224.0.0.0/4`, `240.0.0.0/4`.
  - The rest of the guest subnet.
  - `255.255.255.255`, except DHCP.
  - DHCP to any address other than the gateway or broadcast.
  - Traffic to the gateway, except DNS over UDP and TCP, DHCP, and replies from published ports.
  - All non-IPv4 traffic except ARP.
  - Frames whose source MAC address is not the guest MAC, and ARP from any other sender address.
- Blocked unless `--allow-lan`: `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, and every address of the host's own interfaces. `sbxr` reads the host addresses again every 2 seconds while the run is active.
- Allowed: all other IPv4 destinations.

A blocked TCP connection gets a reset, so it fails at once. Other blocked traffic is dropped silently.

### Limits

- Each run allows at most 1024 TCP connections and 256 UDP flows. Past the cap, new TCP connections get a reset and new UDP flows are dropped. Published connections and DNS TCP connections count against the TCP cap.
- The gateway resolves at most 64 DNS queries at once and drops further queries. Each query has a 2-second timeout.

### DNS

- `A` queries go to the host resolver.
- `AAAA` queries get an empty answer, because the guest has no IPv6 route.
- Other query types go to the nameservers in the host's `/etc/resolv.conf`. Forwarded queries are rebuilt, so guest flags and options never reach the nameservers. A truncated UDP answer is asked again over TCP.
- The gateway refuses classes other than `IN` and does not implement other opcodes or zone transfers.

### Published ports

`-p` creates one TCP listener on the host per mapping, on `127.0.0.1` by default. Only listed ports are reachable. A bind address must be IPv4. A busy host port fails the run before the VM starts.

## Cleanup and prune

- Every run, including failed and interrupted runs, ends with the VM process gone, the network stack and its listeners closed, and the run directory removed.
- Concurrent runs share the image cache read-only. Each run has its own root and state.
- Each run holds a shared lock on the cache from before the pull until its run directory is removed. The VM process inherits this lock and the lock of its run directory.
- When the `sbxr run` process dies without cleanup (for example by SIGKILL), the VM process sees its lifeline pipe close, removes the run directory, and exits.
- When both processes die, the next `sbxr run` removes the run directories whose lock is free.
- `sbxr prune` removes every cached image and all run state. It takes the cache lock exclusively without waiting, so it fails while any run is active. It asks for confirmation on a terminal and needs `--force` when stdin is not a terminal.
- On Linux, cache files belong to subordinate ids, so `sbxr prune` removes them inside the user namespace. Use `sbxr prune --force` instead of `rm -rf`.
