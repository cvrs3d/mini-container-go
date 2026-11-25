# Mini Container — a tiny Linux container runtime in Go

**Educational project, not a secure container sandbox.** A minimal Linux container launcher written in Go. It demonstrates namespaces, a separate Alpine root filesystem, virtual filesystems, cgroups v2, PID 1 signal handling, and orphan-process reaping.

This is the Stage 10 version of the hands-on project: refactored code, unit tests, privileged integration smoke tests, documentation, and a lightweight CI workflow.

## Features

- `CLONE_NEWPID`, `CLONE_NEWUTS`, and `CLONE_NEWNS` namespaces.
- Alpine root filesystem via `chroot`; private mount propagation.
- A namespaced `procfs` at `/proc`, isolated `tmpfs` at `/tmp` and `/dev`.
- `/dev/null` device node in the privileged mode.
- Experimental User Namespace with UID/GID mapping (`0` inside → invoking user's UID/GID outside).
- cgroups v2 CPU and memory limits managed by systemd transient scopes.
- Optional Go init (PID 1), with `SIGTERM`/`SIGINT` forwarding, `SIGCHLD` collection, and a 3-second graceful-stop timeout in the host parent.
- Exit-status propagation (`exit N` → `N`, signals → `128 + signal number`).

## Requirements

- Linux; tested by the project author on Ubuntu ARM64 VM (not on Docker Desktop/macOS).
- Go 1.23+ to build the code in this distribution.
- Root permission for the privileged modes; the `run-userns` exercise still starts via `sudo` on the author's Ubuntu VM.
- A trusted Alpine filesystem in `./rootfs`, containing `/bin/sh` and `/bin/busybox` (and directories `/proc`, `/tmp`, `/dev`). The filesystem is **not distributed** with this source tree.
- systemd with `systemd-run` for `run-mem` and `run-limits`.

The simple runtime resolves `rootfs` relative to the current working directory: launch it from the project root. All demos assume that you prepared the Alpine rootfs in the earlier stages.

## Build and run

```bash
cd ~/projects/mini-container-go
go build -o mini-container .

# Old behavior: the shell replaces PID 1
sudo ./mini-container run /bin/sh -c 'echo "PID: $$"; cat /etc/os-release'

# New behavior: Go Init is PID 1, shell is its child
sudo ./mini-container run-init /bin/sh -c 'echo "Shell PID: $$"; ps'

# Privileged user namespace demonstration: 0 inside maps to SUDO_UID/SUDO_GID
sudo ./mini-container run-userns /bin/sh -c 'id; cat /proc/self/uid_map'

# Memory limit: 256 MiB
sudo ./mini-container run-mem 256 /bin/sh -c 'cat /proc/self/cgroup'

# CPU quota: 50% of one logical CPU; memory: 256 MiB
sudo ./mini-container run-limits --memory 256 --cpu 50 /bin/sh -c 'cat /proc/self/cgroup'
```

In `run-userns`, `/dev` is an empty tmpfs: this mode **does not yet provide `/dev/null`**. The exercise intentionally avoids creating a character device after it was denied by the kernel. This mode is **not rootless** and must not be presented as a strong security boundary.

### CLI

| Command | Behavior |
| --- | --- |
| `run <program> [args...]` | Run a program as PID 1 with PID, UTS and mount namespaces |
| `run-init <program> [args...]` | Run with Go init as PID 1, forwarding signals and reaping orphan children |
| `run-userns <program> [args...]` | Add an experimental user namespace with `SUDO_UID`/`SUDO_GID` mapping |
| `userns-probe` | Print UID/GID maps using a separate user namespace |
| `run-mem <MiB> <program> [args...]` | Run the original `run` mode inside systemd scope with a memory limit |
| `run-limits --memory <MiB> --cpu <percent> <program> [args...]` | Run inside systemd scope with memory and CPU quota |

**Note:** flags for `run-limits` are parsed before the command. The `--cpu` value can exceed 100; for example, 200% is equivalent to two logical CPUs of aggregate time, not fixed CPU affinity.

## Tests

### Unit tests (no root required)

```bash
go test ./...
go test -race ./...
go vet ./...
```

They validate flag parsing, systemd argument construction, UID/GID mapping configuration, exit codes, and CLI validation. They do not create Linux namespaces.

### Integration smoke tests (root required)

Run these **only on a disposable Linux VM you administer** with the Alpine rootfs already present:

```bash
go build -o mini-container .
sudo ./tests/smoke.sh
```

The script verifies:

1. PID 1 inside `run`.
2. Alpine root filesystem isolation.
3. `/proc`, `/tmp` as tmpfs, `/dev/null` as character device.
4. Exit code forwarding (17 and 42).
5. Go Init and the workload having distinct PIDs.
6. Orphaned `sleep` reaped without a lingering zombie.
7. External SIGTERM reaching the workload and preserving exit code 42.
8. Fallback SIGKILL after 3 seconds, producing code 137.
9. UID/GID mapping if invoked via `sudo` with `SUDO_UID`/`SUDO_GID` available.

The longer cgroup probe is opt-in:

```bash
sudo env RUN_CGROUP_TEST=1 ./tests/smoke.sh
```

It starts a temporary systemd scope and checks the live `memory.max` (256 MiB) and `cpu.max` (50%). Scopes are managed and cleaned up by systemd. No test disables AppArmor or writes directly into `/sys/fs/cgroup`.

### CI

The GitHub Actions workflow runs unprivileged `go test`, `go test -race`, `go vet` and a Linux ARM64 compile check. It **does not** run privileged smoke tests on shared runners.

## Architecture

```text
Terminal (host)
   |
   +-- mini-container run-init ...           [host parent]
         |  CLONE_NEWPID, CLONE_NEWUTS, CLONE_NEWNS
         |  SIGTERM -> PID 1; SIGKILL after timeout
         v
       Go Init [PID 1 in container]
         |  private mount propagation, chroot to rootfs
         |  mount /proc, /tmp, /dev; create /dev/null
         |  SIGCHLD -> wait4(-1, WNOHANG) loop
         |  SIGTERM/INT -> main workload PID
         v
       Workload [PID > 1]
         +-- children (orphans are adopted by PID 1)
```

The `run` command has a simpler execution path: `syscall.Exec()` replaces PID 1 with the workload, so there is no Go init/reaper. The two modes are deliberately kept for comparison.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for an explanation of the key functions and important caveats. For a detailed Russian walkthrough of Stage 10, read [docs/STAGE10_WALKTHROUGH_RU.md](docs/STAGE10_WALKTHROUGH_RU.md).

## Important limitations / threat model

**Do not run untrusted programs in this runtime.** It is an educational implementation, **not Docker, OCI, LXC, or a hardened sandbox**.

- `chroot` is not a security boundary against a privileged adversary. The privileged mode retains substantial host privileges/capabilities; it should only run trusted commands.
- No seccomp, capability bounding set, LSM profile, network namespace, container networking, read-only rootfs, bind-mount allowlist, or mount cleanup/recovery policy beyond namespace teardown.
- Only `/dev/null` is provided in privileged mode; the user namespace mode has an empty `/dev`.
- The user namespace mode was tested with `sudo`; it does not implement fully rootless operation. `SUDO_UID`/`SUDO_GID` are lab inputs and should **not** be trusted authorization data.
- `run-init` forwards signals only to its **main workload PID**, not to the entire descendant process group. Processes that survive after main exits are terminated when PID 1 leaves the PID namespace. The init does not wait for every still-running descendant to exit gracefully.
- The 3-second forced-stop timeout is in the outer Go parent. The init does not have an independent timeout.
- systemd scope limits apply to the entire scope, not just an individual process; quotas restrict aggregate CPU/memory usage.
- Paths and filesystem operations are Linux-specific; project code intentionally favors readability over production hardening.

## Project layout

```text
.
├── main.go                 # CLI, namespace setup, init/reaper and systemd scope launcher
├── main_test.go            # No-root unit tests
├── go.mod                  # Go module (stdlib only in this version)
├── Makefile
├── tests/
│   └── smoke.sh           # Explicit, privileged Linux integration tests
├── docs/
│   ├── ARCHITECTURE.md
│   └── STAGE10_WALKTHROUGH_RU.md
├── .github/workflows/
│   └── ci.yml
└── rootfs/                 # Prepared locally; ignored by git
```

## License

Educational example. Add a license consistent with the rest of your repository before publishing or distributing broadly.
