#!/usr/bin/env bash
# Privileged integration tests. Run on a trusted disposable Ubuntu/Linux VM.
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PROJECT_ROOT"

if (( EUID != 0 )); then
  echo "ERROR: these integration tests require root: sudo ./tests/smoke.sh" >&2
  exit 2
fi
if [[ ! -x ./mini-container || ! -x rootfs/bin/sh ]]; then
  echo "ERROR: build ./mini-container and prepare Alpine rootfs/bin/sh first" >&2
  exit 2
fi

TMPDIR_TEST="$(mktemp -d)"
ACTIVE_PID=""
cleanup() {
  if [[ -n "$ACTIVE_PID" ]]; then
    kill -TERM "$ACTIVE_PID" 2>/dev/null || true
    sleep 0.2
    kill -KILL "$ACTIVE_PID" 2>/dev/null || true
    wait "$ACTIVE_PID" 2>/dev/null || true
  fi
  rm -rf "$TMPDIR_TEST"
}
trap cleanup EXIT

pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1" >&2; exit 1; }

assert_exit_code() {
  local name="$1" expected="$2" actual
  shift 2
  if "$@" >"$TMPDIR_TEST/exit.out" 2>&1; then
    actual=0
  else
    actual=$?
  fi
  if [[ "$actual" == "$expected" ]]; then
    pass "$name (exit=$actual)"
  else
    cat "$TMPDIR_TEST/exit.out" >&2
    fail "$name: expected exit=$expected, got exit=$actual"
  fi
}

wait_for_text() {
  local file="$1" pattern="$2" i
  for ((i=0; i<100; i++)); do
    if grep -q -- "$pattern" "$file" 2>/dev/null; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

echo '=== Mini Container integration smoke tests ==='

output="$(./mini-container run /bin/sh -c 'echo "$$"')" || fail 'PID isolation: runtime failed'
[[ "$output" == "1" ]] || fail "PID isolation: got $output"
pass 'PID 1 inside container'

output="$(./mini-container run /bin/cat /etc/os-release)" || fail 'Alpine rootfs: runtime failed'
[[ "$output" == *'ID=alpine'* ]] || fail 'Alpine rootfs: wrong OS'
pass 'Alpine rootfs'

assert_exit_code '/proc mounted' 0 ./mini-container run /bin/sh -c \
  'test -r /proc/1/status && grep -q "^Name:" /proc/1/status'
assert_exit_code '/tmp is tmpfs' 0 ./mini-container run /bin/sh -c \
  'awk '\''$2 == "/tmp" && $3 == "tmpfs" {found=1} END {exit !found}'\'' /proc/mounts'
assert_exit_code '/dev/null character device' 0 ./mini-container run /bin/sh -c \
  'test -c /dev/null && test "$(wc -c </dev/null)" = 0'

assert_exit_code 'run exit code propagation' 17 ./mini-container run /bin/sh -c 'exit 17'
assert_exit_code 'run-init exit code propagation' 42 ./mini-container run-init /bin/sh -c 'exit 42'

output="$(./mini-container run-init /bin/sh -c 'cat /proc/1/comm; echo SHELL_PID=$$')" || fail 'Go Init: runtime failed'
[[ "$output" == *'Go Init PID: 1'* ]] || fail 'Go Init: PID 1 not verified'
if grep -q '^SHELL_PID=1$' <<< "$output"; then fail 'Go Init: shell should not be PID 1'; fi
pass 'Go Init is PID 1; workload is a child'

output="$(./mini-container run-init /bin/sh -c '
  /bin/sh -c "sleep 0.2 &"
  sleep 1
  ps -o pid,ppid,stat,comm
')" || fail 'orphan reaping: runtime failed'
[[ "$output" == *'reaped PID:'* ]] || fail 'orphan reaping: no child reaped'
if grep -Eq '^[[:space:]]*[0-9]+[[:space:]]+1[[:space:]]+Z[^[:space:]]*[[:space:]]+sleep[[:space:]]*$' <<< "$output"; then
  fail 'orphan reaping: zombie sleep remains'
fi
pass 'Orphan adopted and reaped'

# External SIGTERM must travel host parent -> Go Init -> workload.
./mini-container run-init /bin/sh -c '
  trap "echo TERM_SEEN; exit 42" TERM
  echo READY
  while :; do sleep 1; done
' >"$TMPDIR_TEST/signal.out" 2>&1 &
ACTIVE_PID=$!
wait_for_text "$TMPDIR_TEST/signal.out" READY || fail 'signal: workload never became ready'
kill -TERM "$ACTIVE_PID"
if wait "$ACTIVE_PID"; then code=0; else code=$?; fi
ACTIVE_PID=""
[[ "$code" == 42 ]] || { cat "$TMPDIR_TEST/signal.out" >&2; fail "signal: got exit=$code, want 42"; }
grep -q TERM_SEEN "$TMPDIR_TEST/signal.out" || fail 'signal: workload did not see SIGTERM'
pass 'SIGTERM forwarding and exit code 42'

# The same path must force-kill a workload that ignores TERM after a deadline.
./mini-container run /bin/sh -c '
  trap "" TERM
  echo READY
  while :; do sleep 1; done
' >"$TMPDIR_TEST/kill.out" 2>&1 &
ACTIVE_PID=$!
wait_for_text "$TMPDIR_TEST/kill.out" READY || fail 'SIGKILL fallback: workload never became ready'
kill -TERM "$ACTIVE_PID"
if wait "$ACTIVE_PID"; then code=0; else code=$?; fi
ACTIVE_PID=""
[[ "$code" == 137 ]] || { cat "$TMPDIR_TEST/kill.out" >&2; fail "SIGKILL fallback: got exit=$code, want 137"; }
grep -q 'graceful shutdown timeout' "$TMPDIR_TEST/kill.out" || fail 'SIGKILL fallback: missing timeout message'
pass 'SIGKILL fallback after 3 seconds'

if [[ -n "${SUDO_UID:-}" && -n "${SUDO_GID:-}" ]]; then
  output="$(./mini-container run-userns /bin/sh -c 'id; cat /proc/self/uid_map; cat /proc/self/gid_map')" || fail 'userns: runtime failed'
  if ! grep -Eq "^[[:space:]]*0[[:space:]]+$SUDO_UID[[:space:]]+1[[:space:]]*$" <<< "$output"; then
    fail 'userns: missing UID mapping'
  fi
  if ! grep -Eq "^[[:space:]]*0[[:space:]]+$SUDO_GID[[:space:]]+1[[:space:]]*$" <<< "$output"; then
    fail 'userns: missing GID mapping'
  fi
  pass 'User Namespace UID/GID mapping'
else
  echo 'SKIP  User Namespace mapping (requires SUDO_UID/SUDO_GID)'
fi

if [[ "${RUN_CGROUP_TEST:-0}" == 1 ]]; then
  if ! command -v systemd-run >/dev/null; then fail 'cgroup test requires systemd-run'; fi
  ./mini-container run-limits --memory 256 --cpu 50 /bin/sh -c \
    'cat /proc/self/cgroup; sleep 10' >"$TMPDIR_TEST/cgroup.out" 2>&1 &
  ACTIVE_PID=$!
  wait_for_text "$TMPDIR_TEST/cgroup.out" '^0::/' || fail 'cgroups: container did not start'
  cgroup_path="$(sed -n 's/^0:://p' "$TMPDIR_TEST/cgroup.out" | head -n1)"
  [[ -n "$cgroup_path" && -r "/sys/fs/cgroup${cgroup_path}/memory.max" ]] || fail 'cgroups: memory.max not accessible'
  memory="$(cat "/sys/fs/cgroup${cgroup_path}/memory.max")"
  read -r quota period < "/sys/fs/cgroup${cgroup_path}/cpu.max"
  [[ "$memory" == 268435456 ]] || fail "cgroups: memory.max=$memory, want 268435456"
  [[ "$quota" =~ ^[0-9]+$ && "$period" =~ ^[0-9]+$ ]] || fail "cgroups: unexpected cpu.max=$quota $period"
  (( quota * 100 == period * 50 )) || fail "cgroups: CPU quota=$quota period=$period; want 50%"
  pass 'cgroups v2 memory.max=256MiB and cpu.max=50%'
  # Finishing a scope is systemd's responsibility; no manual cgroup deletion.
  if wait "$ACTIVE_PID"; then :; else fail 'cgroups: scoped container failed'; fi
  ACTIVE_PID=""
else
  echo 'SKIP  cgroup CPU/memory probe (set RUN_CGROUP_TEST=1 to enable)'
fi

echo '=== All selected smoke tests passed ==='
