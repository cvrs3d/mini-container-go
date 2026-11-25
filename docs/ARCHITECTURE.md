# Mini Container: how the code works

This document explains the most important paths through `main.go`.

## 1. Outer parent: `run(args, childMode)`

- Runs `/proc/self/exe child[-init] ...` in a **new PID, UTS and mount namespace** using `exec.Cmd.SysProcAttr.Cloneflags`.
- Relays stdin/stdout/stderr to the child.
- Registers `SIGTERM` and `SIGINT` *before* starting the process.
- After `cmd.Start()`, a goroutine receives the first termination signal and forwards it to the child PID 1.
- Starts a three-second timer after successful delivery. If the child is still alive, sends `SIGKILL`.
- In the main goroutine, `cmd.Wait()` collects that **direct** child's exit status. The `done`/`forwarderDone` channels ensure the signal handler exits before the function returns.

A namespace does not automatically restrict resource consumption. The `run-mem` and `run-limits` paths start the runtime in a systemd-managed cgroup scope first.

## 2. Inner process: `child(args, createDevices, useInit)`

This function is called only in the subprocess, after `Cloneflags` has created new namespaces. The `os.Getpid() == 1` guard is an extra sanity check.

1. Recursively marks mount propagation private using `MS_REC|MS_PRIVATE` **before any mounts**.
2. Resolves `rootfs`, calls `chroot()`, then `chdir("/")`.
3. Mounts a fresh `procfs` at `/proc`, `tmpfs` at `/tmp`, and `tmpfs` at `/dev`.
4. In privileged mode, creates `/dev/null` (Linux character device with major=1, minor=3).
5. In `run`, calls `syscall.Exec` so the workload becomes PID 1. In `run-init`, calls `runInit` so Go remains PID 1.

Important: `chroot()` alone is not a security barrier, particularly when run as root with capabilities.

## 3. Inner init/reaper: `runInit(args)`

- Subscribes to `SIGCHLD`, `SIGTERM` and `SIGINT` before spawning anything.
- Looks up the workload executable and calls `syscall.ForkExec()`; it does **not** call `exec.Cmd.Wait()`.
- In a single event loop, calls `reapExitedChildren(mainPID)` before waiting for the next signal. When a main process exits extremely quickly, this eager drain prevents depending exclusively on a later notification.
- On `SIGTERM`/`SIGINT`, forwards the signal to the main workload PID.
- Tracks the *main* workload status separately from orphaned children and returns the workload's exit code.

An init process can adopt orphan children whose original parent has exited. Its reaper therefore uses `wait4(-1, WNOHANG)` instead of `wait4(mainPID, ...)`.

## 4. Why `wait4(-1, WNOHANG)` loops

One `SIGCHLD` need not correspond to one exited process. Signals can coalesce, and multiple children can already be zombies. Each `wait4` returns at most one child. A loop drains every currently waitable child, stopping at `pid == 0` (none ready) or `ECHILD` (none waitable). `EINTR` retries. The main workload exit status is remembered separately.

The init exits when the *main* workload exits, not when all possible descendants finish. The kernel terminates remaining tasks in its PID namespace when PID 1 exits. A production supervisor would need a more explicit lifecycle policy.

## 5. Exit codes

- Normal exit: `status.ExitStatus()`.
- Signal termination: conventional `128 + int(status.Signal())`, e.g. SIGKILL → 137.
- A numeric 137 alone does not prove SIGKILL: a program could explicitly `exit 137`.
- `workloadExitError` carries a code from the custom wait4 reaper back to `main`.
- `exec.ExitError` represents a process launched through `os/exec` in the outer parent and systemd launch paths.

Expected nonzero workload exits are not printed twice as generic runtime errors.

## 6. User namespace

`parseHostIDs` validates nonzero `SUDO_UID`/`SUDO_GID`, and `userNSAttributes` maps container UID/GID 0 to those host IDs. The tested environment required a privileged setup with `sudo` due to kernel/AppArmor restrictions. It is not a fully rootless implementation, and no device nodes are created in that mode.

## 7. Cgroups v2

`parseLimits` is a pure input parser that is unit-tested. `buildScopeArgs` builds the systemd-run argv as **an array**, avoiding a shell-string interpolation layer. It runs `mini-container run ...` inside a transient scope and not the workload command directly on the host. `MemoryMax` maps to the cgroup `memory.max`, while `CPUQuota` controls the ratio in `cpu.max`.

`systemd-run --scope --same-dir --expand-environment=no` was chosen for the Ubuntu VM in which the tutorial was developed. Because systemd manages the scope lifecycle, no direct cgroupfs mutation is required.

## 8. Test strategy

- `go test` tests parsing, argument formation, UID/GID mapping configurations, status conversion, and CLI validation without root.
- `go test -race` checks unit tests with the Go race detector.
- `tests/smoke.sh`, run intentionally as root on a disposable VM with Alpine rootfs, tests namespace behavior, process exit codes, signal forwarding, reaping, and optionally cgroups.
- CI runs only safe unprivileged Go tests; it does not call `sudo`, mount filesystems or write cgroup settings.
