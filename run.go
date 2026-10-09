package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// runParent is executed in the CLI process. It prepares the namespace
// flags + uid/gid maps and re-executes itself as the hidden "__init"
// stage inside the freshly created namespaces.
func runParent(spec *Spec) int {
	abs, err := filepath.Abs(spec.Rootfs)
	if err != nil {
		fatal("resolve rootfs: %v", err)
	}
	spec.Rootfs = abs
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		fatal("rootfs %q is not a directory (extract one first, e.g. `goroot extract alpine.tar.gz rootfs`)", spec.Rootfs)
	}
	spec.HostUID = os.Getuid()
	spec.HostGID = os.Getgid()

	exe, err := os.Executable()
	if err != nil {
		exe = "/proc/self/exe"
	}

	self := "/proc/self/exe"
	if _, err := os.Stat(self); err != nil {
		self = exe
	}

	cmd := exec.Command(self, "__init")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "GOROOT_SPEC="+marshalSpec(spec))

	cloneFlags := uintptr(unix.CLONE_NEWUSER | unix.CLONE_NEWNS)
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
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
		GidMappingsEnableSetgroups: false,
		Pdeathsig:                  syscall.SIGKILL,
	}

	// Interactive terminal: put the container in the foreground process
	// group of the inherited controlling terminal so that job control and
	// Ctrl-C work. We deliberately do NOT setsid(): claiming an already
	// owned tty as the controlling terminal of a brand new session fails
	// with EPERM, so we stay in the parent's session and just take the
	// foreground.
	tty := isTerminal(os.Stdin.Fd())
	if tty {
		attr.Setpgid = true
		attr.Foreground = true
		attr.Ctty = 0
	}

	cmd.SysProcAttr = attr

	if err := cmd.Start(); err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EINVAL) {
			fatal("cannot create namespaces: %v\n(unprivileged user namespaces may be disabled)", err)
		}
		fatal("start container: %v", err)
	}

	// Forward a few signals to the container in non-tty mode.
	if !tty {
		fwd := make(chan os.Signal, 8)
		signal.Notify(fwd, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
		go func() {
			for sig := range fwd {
				if cmd.Process != nil {
					_ = cmd.Process.Signal(sig)
				}
			}
		}()
	}

	err = cmd.Wait()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ws.ExitStatus()
		}
		return ee.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "goroot: %v\n", err)
	return 127
}
