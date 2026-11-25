//go:build linux

// Mini Container is an educational Linux container runtime, not a security boundary.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const shutdownGracePeriod = 3 * time.Second

// workloadExitError carries the exit code reaped by our own init process.
// A normal exec.ExitError cannot be manufactured by syscall.Wait4.
type workloadExitError struct{ code int }

func (e *workloadExitError) Error() string {
	return fmt.Sprintf("workload exited with status %d", e.code)
}

// statusExitCode implements the common shell convention: 128 + signal.
func statusExitCode(status syscall.WaitStatus) int {
	switch {
	case status.Exited():
		return status.ExitStatus()
	case status.Signaled():
		return 128 + int(status.Signal())
	default:
		return 1
	}
}

func errorExitCode(err error) int {
	if err == nil {
		return 0
	}
	var workloadErr *workloadExitError
	if errors.As(err, &workloadErr) {
		return workloadErr.code
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return statusExitCode(status)
		}
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
	}
	return 1
}

// reapExitedChildren drains *all currently waitable* children. It must be
// the ONLY wait mechanism in the init process: do not also call cmd.Wait().
func reapExitedChildren(mainPID int) (mainExited bool, mainStatus syscall.WaitStatus, err error) {
	for {
		var status syscall.WaitStatus
		pid, waitErr := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		switch {
		case errors.Is(waitErr, syscall.EINTR):
			continue
		case errors.Is(waitErr, syscall.ECHILD):
			return mainExited, mainStatus, nil
		case waitErr != nil:
			return mainExited, mainStatus, fmt.Errorf("wait4: %w", waitErr)
		case pid == 0:
			return mainExited, mainStatus, nil
		}

		fmt.Printf("reaped PID: %d\n", pid)
		if pid == mainPID {
			mainExited, mainStatus = true, status
		}
	}
}

// runInit is PID 1 inside the container. It is the sole process reaper and
// forwards TERM/INT to the main workload (not to its entire process tree).
func runInit(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("expected command")
	}
	fmt.Printf("Go Init PID: %d\n", os.Getpid())

	sigCh := make(chan os.Signal, 16)
	signal.Notify(sigCh, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	path, err := exec.LookPath(args[0])
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}
	mainPID, err := syscall.ForkExec(path, args, &syscall.ProcAttr{
		Env:   os.Environ(),
		Files: []uintptr{os.Stdin.Fd(), os.Stdout.Fd(), os.Stderr.Fd()},
	})
	if err != nil {
		return fmt.Errorf("forkexec: %w", err)
	}

	// SIGCHLD is not one notification per child. Always drain waitable children
	// *before* waiting for the next notification, including the first iteration.
	var mainExited bool
	var mainStatus syscall.WaitStatus
	for {
		exited, status, err := reapExitedChildren(mainPID)
		if err != nil {
			return fmt.Errorf("reap children: %w", err)
		}
		if exited {
			mainExited, mainStatus = true, status
		}
		if mainExited {
			code := statusExitCode(mainStatus)
			if code == 0 {
				return nil
			}
			return &workloadExitError{code: code}
		}

		sig := <-sigCh
		switch sig {
		case syscall.SIGCHLD:
			// The next loop iteration drains ALL exited children.
		case syscall.SIGTERM, syscall.SIGINT:
			if err := syscall.Kill(mainPID, sig.(syscall.Signal)); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("forward signal to main workload: %w", err)
			}
		}
	}
}

// linuxDevNull is the Linux encoded dev_t for character device 1:3
// (major=1, minor=3). Keeping this single device literal avoids an external
// dependency; more devices would merit a general mkdev helper.
const linuxDevNull = (1 << 8) | 3

// child runs only AFTER exec has created the new PID and mount namespaces.
// Never perform these mounts in the privileged parent process.
func child(args []string, createDevices, useInit bool) error {
	if len(args) == 0 {
		return fmt.Errorf("expected command")
	}
	if os.Getpid() != 1 {
		return fmt.Errorf("child must be PID 1 inside a PID namespace")
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mount propagation private: %w", err)
	}

	rootfsPath, err := filepath.Abs("rootfs")
	if err != nil {
		return fmt.Errorf("resolve rootfs: %w", err)
	}
	if err := syscall.Chroot(rootfsPath); err != nil {
		return fmt.Errorf("chroot %q: %w", rootfsPath, err)
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}
	if err := syscall.Mount("proc", "/proc", "proc", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}
	if err := syscall.Mount("tmpfs", "/tmp", "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "size=16m,mode=1777"); err != nil {
		return fmt.Errorf("mount /tmp: %w", err)
	}
	// Do not use MS_NODEV here: /dev/null needs device-node semantics.
	if err := syscall.Mount("tmpfs", "/dev", "tmpfs", syscall.MS_NOSUID|syscall.MS_NOEXEC, "size=1m,mode=755"); err != nil {
		return fmt.Errorf("mount /dev: %w", err)
	}
	if createDevices {
		if err := syscall.Mknod("/dev/null", syscall.S_IFCHR|0666, linuxDevNull); err != nil {
			return fmt.Errorf("create /dev/null: %w", err)
		}
		if err := os.Chmod("/dev/null", 0666); err != nil {
			return fmt.Errorf("chmod /dev/null: %w", err)
		}
	}

	if useInit {
		return runInit(args)
	}
	return syscall.Exec(args[0], args, os.Environ())
}

// run launches the inner process with the requested child mode. The parent
// forwards the first termination signal and forces shutdown after 3 seconds.
func run(args []string, childMode string) error {
	if len(args) == 0 {
		return fmt.Errorf("expected command")
	}
	cmd := exec.Command("/proc/self/exe", append([]string{childMode}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWUTS | syscall.CLONE_NEWNS,
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start child: %w", err)
	}
	done := make(chan struct{})
	forwarderDone := make(chan struct{})
	go func() {
		defer close(forwarderDone)
		select {
		case sig := <-sigCh:
			if err := cmd.Process.Signal(sig); err != nil {
				if !errors.Is(err, os.ErrProcessDone) {
					fmt.Fprintln(os.Stderr, "forward signal:", err)
				}
				return
			}
			timer := time.NewTimer(shutdownGracePeriod)
			defer timer.Stop()
			select {
			case <-done:
				return
			case <-timer.C:
				if err := cmd.Process.Kill(); err != nil {
					if !errors.Is(err, os.ErrProcessDone) {
						fmt.Fprintln(os.Stderr, "kill container:", err)
					}
				} else {
					fmt.Fprintln(os.Stderr, "graceful shutdown timeout: SIGKILL sent")
				}
			}
		case <-done:
			return
		}
	}()

	err := cmd.Wait()
	close(done)
	<-forwarderDone
	return err
}

func parseHostIDs(uidText, gidText string) (int, int, error) {
	uid, err := strconv.Atoi(uidText)
	if err != nil {
		return 0, 0, fmt.Errorf("parse SUDO_UID: %w", err)
	}
	if uid <= 0 {
		return 0, 0, fmt.Errorf("invalid SUDO_UID: %d", uid)
	}
	gid, err := strconv.Atoi(gidText)
	if err != nil {
		return 0, 0, fmt.Errorf("parse SUDO_GID: %w", err)
	}
	if gid <= 0 {
		return 0, 0, fmt.Errorf("invalid SUDO_GID: %d", gid)
	}
	return uid, gid, nil
}

func userNSAttributes(hostUID, hostGID int, extraCloneflags uintptr) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | extraCloneflags,
		UidMappings: []syscall.SysProcIDMap{{
			ContainerID: 0, HostID: hostUID, Size: 1,
		}},
		GidMappings: []syscall.SysProcIDMap{{
			ContainerID: 0, HostID: hostGID, Size: 1,
		}},
		GidMappingsEnableSetgroups: true,
		Credential: &syscall.Credential{
			Uid: 0, Gid: 0, NoSetGroups: true,
		},
	}
}

// This educational userns mode still requires sudo on the tested Ubuntu VM.
// SUDO_UID/GID are used as lab configuration, NOT a trusted security boundary.
func runUserNS(args []string, probe bool) error {
	if !probe && len(args) == 0 {
		return fmt.Errorf("expected command")
	}
	uid, gid, err := parseHostIDs(os.Getenv("SUDO_UID"), os.Getenv("SUDO_GID"))
	if err != nil {
		return err
	}

	var cmd *exec.Cmd
	if probe {
		cmd = exec.Command("/bin/sh", "-c", "id; cat /proc/self/uid_map; cat /proc/self/gid_map")
		cmd.SysProcAttr = userNSAttributes(uid, gid, 0)
	} else {
		cmd = exec.Command("/proc/self/exe", append([]string{"child-userns"}, args...)...)
		cmd.SysProcAttr = userNSAttributes(uid, gid, syscall.CLONE_NEWPID|syscall.CLONE_NEWUTS|syscall.CLONE_NEWNS)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

type limitsConfig struct {
	memoryMiB  int
	cpuPercent int
	command    []string
}

func parseLimits(args []string) (limitsConfig, error) {
	fs := flag.NewFlagSet("run-limits", flag.ContinueOnError)
	memory := fs.Int("memory", 0, "Memory limit in MiB")
	cpu := fs.Int("cpu", 0, "CPU quota in percent")
	if err := fs.Parse(args); err != nil {
		return limitsConfig{}, err
	}
	if *memory <= 0 {
		return limitsConfig{}, fmt.Errorf("memory limit must be positive")
	}
	if *cpu <= 0 {
		return limitsConfig{}, fmt.Errorf("CPU quota must be positive")
	}
	if len(fs.Args()) == 0 {
		return limitsConfig{}, fmt.Errorf("expected container command")
	}
	return limitsConfig{memoryMiB: *memory, cpuPercent: *cpu, command: fs.Args()}, nil
}

// buildScopeArgs always runs the *runtime* inside the scope, not /bin/sh on host.
func buildScopeArgs(executable string, memoryMiB, cpuPercent int, command []string) []string {
	args := []string{"--scope", "--same-dir", "--expand-environment=no"}
	if memoryMiB > 0 {
		args = append(args, fmt.Sprintf("--property=MemoryMax=%dM", memoryMiB))
	}
	if cpuPercent > 0 {
		args = append(args, fmt.Sprintf("--property=CPUQuota=%d%%", cpuPercent))
	}
	args = append(args, executable, "run")
	return append(args, command...)
}

func runScope(memoryMiB, cpuPercent int, command []string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	cmd := exec.Command("systemd-run", buildScopeArgs(executable, memoryMiB, cpuPercent, command)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func runWithLimits(args []string) error {
	cfg, err := parseLimits(args)
	if err != nil {
		return err
	}
	return runScope(cfg.memoryMiB, cfg.cpuPercent, cfg.command)
}

func runWithMemoryLimit(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: run-mem <MiB> <command> [args...]")
	}
	memoryMiB, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("invalid memory limit: %w", err)
	}
	if memoryMiB <= 0 {
		return fmt.Errorf("memory limit must be positive")
	}
	return runScope(memoryMiB, 0, args[1:])
}

func dispatch(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: mini-container <run|run-init|run-userns|run-mem|run-limits> <args...>")
	}
	switch args[0] {
	case "run":
		return run(args[1:], "child")
	case "run-init":
		return run(args[1:], "child-init")
	case "child":
		return child(args[1:], true, false)
	case "child-init":
		return child(args[1:], true, true)
	case "child-userns":
		return child(args[1:], false, false)
	case "userns-probe":
		return runUserNS(nil, true)
	case "run-userns":
		return runUserNS(args[1:], false)
	case "run-mem":
		return runWithMemoryLimit(args[1:])
	case "run-limits":
		return runWithLimits(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func main() {
	err := dispatch(os.Args[1:])
	if err == nil {
		return
	}
	// Expected non-zero exit status is not a runtime error; keep stderr clean.
	var workloadErr *workloadExitError
	var exitErr *exec.ExitError
	if !errors.As(err, &workloadErr) && !errors.As(err, &exitErr) {
		fmt.Fprintln(os.Stderr, "Error:", err)
	}
	os.Exit(errorExitCode(err))
}
