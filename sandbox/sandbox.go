//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Sandbox is a started (or startable) container.
type Sandbox struct {
	spec *Spec
	cmd  *exec.Cmd
}

// prepare resolves the rootfs and applies shared defaults.
func prepare(spec *Spec) error {
	abs, err := filepath.Abs(spec.Rootfs)
	if err != nil {
		return fmt.Errorf("resolve rootfs: %w", err)
	}
	spec.Rootfs = abs
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return fmt.Errorf("rootfs %q is not a directory", abs)
	}
	spec.HostUID = os.Getuid()
	spec.HostGID = os.Getgid()
	addDefaultResolv(spec)
	return nil
}

// addDefaultResolv binds the host's /etc/resolv.conf read-only into the
// container unless disabled or already mounted by the caller.
func addDefaultResolv(spec *Spec) {
	if spec.NoResolv {
		return
	}
	if _, err := os.Stat("/etc/resolv.conf"); err != nil {
		return
	}
	for _, b := range spec.Binds {
		dst := b.Dst
		if dst == "" {
			dst = b.Src
		}
		if dst == "/etc/resolv.conf" {
			return
		}
	}
	spec.Binds = append(spec.Binds, Bind{Src: "/etc/resolv.conf", Dst: "/etc/resolv.conf", RO: true})
}

func effectiveTTY(io Stdio) bool {
	if io.TTY {
		return true
	}
	if f, ok := io.Stdin.(*os.File); ok && isTerminal(f.Fd()) {
		return true
	}
	if f, ok := io.Stdout.(*os.File); ok && isTerminal(f.Fd()) {
		return true
	}
	return false
}

// buildCommand probes capabilities, applies degradation and returns an
// unstarted exec.Cmd that re-execs us into the container init stage.
func buildCommand(spec *Spec, io Stdio, caps Caps) (*exec.Cmd, error) {
	realRoot := caps.RealRoot
	if !realRoot && !caps.User {
		return nil, errors.New("user namespaces are unavailable and you are not root " +
			"(enable kernel.unprivileged_userns_clone, or run as root)")
	}
	useUser := !realRoot && caps.User
	if !useUser {
		spec.NoUser = true
	}

	if !spec.NoUTS && !caps.Uts {
		warnf("UTS namespace unavailable; keeping the host hostname")
		spec.NoUTS = true
	}
	if !spec.NoIPC && !caps.Ipc {
		warnf("IPC namespace unavailable; continuing without it")
		spec.NoIPC = true
	}
	if !spec.NoPID && !caps.Pid {
		warnf("PID namespace unavailable; the host /proc will be visible")
		spec.NoPID = true
	}
	if !spec.ShareNet && !caps.Net {
		warnf("network namespace unavailable; sharing the host network")
		spec.ShareNet = true
	}

	cloneFlags := uintptr(unix.CLONE_NEWNS)
	if useUser {
		cloneFlags |= unix.CLONE_NEWUSER
	}
	if !spec.NoUTS {
		cloneFlags |= unix.CLONE_NEWUTS
	}
	if !spec.NoIPC {
		cloneFlags |= unix.CLONE_NEWIPC
	}
	if !spec.NoPID {
		cloneFlags |= unix.CLONE_NEWPID
	}
	if !spec.ShareNet {
		cloneFlags |= unix.CLONE_NEWNET
	}

	attr := &syscall.SysProcAttr{
		Cloneflags: cloneFlags,
		Pdeathsig:  syscall.SIGKILL,
	}
	if useUser {
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
		attr.GidMappingsEnableSetgroups = false
	}

	// Interactive terminal: take the foreground process group of the inherited
	// tty (never setsid+TIOCSCTTY, which fails with EPERM on an owned tty).
	if effectiveTTY(io) {
		attr.Setpgid = true
		attr.Foreground = true
		attr.Ctty = 0
	}

	cmd := exec.Command(execPath())
	cmd.Env = append(os.Environ(), "GOROOT_INIT=1", "GOROOT_SPEC="+marshalSpec(spec))
	cmd.Stdin = io.Stdin
	cmd.Stdout = io.Stdout
	cmd.Stderr = io.Stderr
	cmd.SysProcAttr = attr
	return cmd, nil
}

// Start prepares and launches the container without waiting for it.
func Start(spec *Spec, io Stdio) (*Sandbox, error) {
	if err := prepare(spec); err != nil {
		return nil, err
	}
	cmd, err := buildCommand(spec, io, Detect())
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start container: %w", err)
	}
	return &Sandbox{spec: spec, cmd: cmd}, nil
}

// Wait waits for the container and returns its process exit code.
func (s *Sandbox) Wait() (int, error) {
	return waitExitCode(s.cmd)
}

// Pid returns the pid (in the caller's PID namespace) of the container init.
func (s *Sandbox) Pid() int {
	if s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// Signal sends a signal to the container init process.
func (s *Sandbox) Signal(sig syscall.Signal) error {
	if s.cmd.Process == nil {
		return errors.New("sandbox not started")
	}
	return s.cmd.Process.Signal(sig)
}

// Kill forcibly kills the container.
func (s *Sandbox) Kill() error {
	if s.cmd.Process == nil {
		return nil
	}
	return s.cmd.Process.Kill()
}

// Process exposes the underlying os.Process.
func (s *Sandbox) Process() *os.Process {
	return s.cmd.Process
}

// Run starts the container, waits for the context (if any) to be cancelled,
// and returns the exit code.
func Run(ctx context.Context, spec *Spec, io Stdio) (int, error) {
	sb, err := Start(spec, io)
	if err != nil {
		return -1, err
	}
	if ctx != nil {
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = sb.Signal(syscall.SIGTERM)
				time.Sleep(5 * time.Second)
				_ = sb.Kill()
			case <-done:
			}
		}()
		defer close(done)
	}
	return sb.Wait()
}

// waitExitCode maps an exec result to a process exit code.
func waitExitCode(cmd *exec.Cmd) (int, error) {
	err := cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal()), nil
			}
			return ws.ExitStatus(), nil
		}
		return ee.ExitCode(), nil
	}
	return 127, err
}
