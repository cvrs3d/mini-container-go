//go:build linux

package main

import (
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestParseLimits(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    limitsConfig
		wantErr string
	}{
		{
			name: "valid",
			args: []string{"--memory", "256", "--cpu", "50", "/bin/sh", "-c", "echo hello"},
			want: limitsConfig{memoryMiB: 256, cpuPercent: 50, command: []string{"/bin/sh", "-c", "echo hello"}},
		},
		{
			name: "multiple CPUs allowed",
			args: []string{"--memory=512", "--cpu=200", "/bin/true"},
			want: limitsConfig{memoryMiB: 512, cpuPercent: 200, command: []string{"/bin/true"}},
		},
		{name: "no memory", args: []string{"--cpu", "50", "/bin/sh"}, wantErr: "memory limit must be positive"},
		{name: "no CPU", args: []string{"--memory", "256", "/bin/sh"}, wantErr: "CPU quota must be positive"},
		{name: "negative memory", args: []string{"--memory", "-1", "--cpu", "50", "/bin/sh"}, wantErr: "memory limit must be positive"},
		{name: "negative CPU", args: []string{"--memory", "256", "--cpu", "-5", "/bin/sh"}, wantErr: "CPU quota must be positive"},
		{name: "missing command", args: []string{"--memory", "256", "--cpu", "50"}, wantErr: "expected container command"},
		{name: "invalid memory", args: []string{"--memory", "not-a-number", "--cpu", "50", "/bin/sh"}, wantErr: "invalid value"},
		{name: "unknown flag", args: []string{"--nope", "1"}, wantErr: "flag provided but not defined"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLimits(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseLimits(%q): error=%v, want containing %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v; want %+v", got, tc.want)
			}
		})
	}
}

func TestBuildScopeArgs(t *testing.T) {
	got := buildScopeArgs("/home/dev/mini-container", 256, 50, []string{"/bin/sh", "-c", "echo hello"})
	want := []string{
		"--scope", "--same-dir", "--expand-environment=no",
		"--property=MemoryMax=256M", "--property=CPUQuota=50%",
		"/home/dev/mini-container", "run", "/bin/sh", "-c", "echo hello",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q; want %q", got, want)
	}
	got = buildScopeArgs("/runtime", 128, 0, []string{"/bin/true"})
	for _, arg := range got {
		if strings.HasPrefix(arg, "--property=CPUQuota") {
			t.Fatalf("unexpected CPUQuota in memory-only mode: %q", got)
		}
	}
}

func TestParseHostIDs(t *testing.T) {
	uid, gid, err := parseHostIDs("1000", "1001")
	if err != nil || uid != 1000 || gid != 1001 {
		t.Fatalf("got uid=%d gid=%d err=%v", uid, gid, err)
	}
	for _, input := range [][2]string{{"", "1000"}, {"0", "1000"}, {"abc", "1000"}, {"1000", "0"}, {"1000", "abc"}} {
		if _, _, err := parseHostIDs(input[0], input[1]); err == nil {
			t.Fatalf("expected an error for %q", input)
		}
	}
}

func TestUserNSAttributes(t *testing.T) {
	attr := userNSAttributes(1000, 1001, syscall.CLONE_NEWPID|syscall.CLONE_NEWNS)
	if attr.Cloneflags&syscall.CLONE_NEWUSER == 0 || attr.Cloneflags&syscall.CLONE_NEWPID == 0 || attr.Cloneflags&syscall.CLONE_NEWNS == 0 {
		t.Fatalf("missing namespace flags: %#x", attr.Cloneflags)
	}
	if len(attr.UidMappings) != 1 || attr.UidMappings[0] != (syscall.SysProcIDMap{ContainerID: 0, HostID: 1000, Size: 1}) {
		t.Fatalf("bad uid mapping: %+v", attr.UidMappings)
	}
	if len(attr.GidMappings) != 1 || attr.GidMappings[0].HostID != 1001 {
		t.Fatalf("bad gid mapping: %+v", attr.GidMappings)
	}
	if attr.Credential == nil || attr.Credential.Uid != 0 || attr.Credential.Gid != 0 || !attr.Credential.NoSetGroups {
		t.Fatalf("bad child credential: %+v", attr.Credential)
	}
}

func TestStatusExitCode(t *testing.T) {
	// The low byte contains the terminating signal; the next byte has the exit code.
	if got := statusExitCode(syscall.WaitStatus(42 << 8)); got != 42 {
		t.Fatalf("normal exit got %d; want 42", got)
	}
	if got := statusExitCode(syscall.WaitStatus(syscall.SIGKILL)); got != 137 {
		t.Fatalf("SIGKILL got %d; want 137", got)
	}
	if got := statusExitCode(syscall.WaitStatus(syscall.SIGTERM)); got != 143 {
		t.Fatalf("SIGTERM got %d; want 143", got)
	}
}

func TestErrorExitCode(t *testing.T) {
	if got := errorExitCode(nil); got != 0 {
		t.Fatalf("nil error got %d", got)
	}
	if got := errorExitCode(&workloadExitError{code: 42}); got != 42 {
		t.Fatalf("workload error got %d", got)
	}
	if got := errorExitCode(errors.New("failure")); got != 1 {
		t.Fatalf("generic error got %d", got)
	}
	// Exercise the *real* exec.ExitError path without namespace privileges.
	cmd := exec.Command("/bin/sh", "-c", "exit 23")
	if got := errorExitCode(cmd.Run()); got != 23 {
		t.Fatalf("exec.ExitError got %d; want 23", got)
	}
}

func TestDispatchValidation(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"badcommand"},
		{"run"},
		{"run-init"},
		{"run-mem"},
		{"run-mem", "0", "/bin/true"},
		{"run-limits", "--memory", "0", "--cpu", "50", "/bin/true"},
	} {
		if err := dispatch(args); err == nil {
			t.Fatalf("dispatch(%q) should fail", args)
		}
	}
}
