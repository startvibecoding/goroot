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

// prepareSpec resolves the rootfs (falling back to the embedded Alpine) and
// applies default behaviors shared by `run` and the daemon.
func prepareSpec(spec *Spec) error {
	// No -r/--root: fall back to the Alpine minirootfs embedded in the binary.
	if spec.Rootfs == "" {
		dir, err := ensureEmbeddedRootfs()
		if err != nil {
			return fmt.Errorf("prepare built-in rootfs: %w", err)
		}
		info("using built-in Alpine minirootfs (cache: %s)", dir)
		spec.Rootfs = dir
	}

	abs, err := filepath.Abs(spec.Rootfs)
	if err != nil {
		return fmt.Errorf("resolve rootfs: %w", err)
	}
	spec.Rootfs = abs
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return fmt.Errorf("rootfs %q is not a directory (extract one first, e.g. `goroot extract alpine.tar.gz rootfs`)", spec.Rootfs)
	}
	spec.HostUID = os.Getuid()
	spec.HostGID = os.Getgid()

	// Default UX: make the host resolver available read-only inside the
	// container so DNS works out of the box (unless the user overrode it or
	// opted out).
	addDefaultResolv(spec)
	return nil
}

// buildContainerCmd probes kernel capabilities, degrades gracefully and returns
// an unstarted command that re-executes us as the hidden "__init" stage inside
// the chosen namespaces, wired to the given stdio.
func buildContainerCmd(spec *Spec, stdin *os.File, stdout, stderr *os.File, tty bool) (*exec.Cmd, error) {
	realRoot := os.Geteuid() == 0

	// --- Capability probing & graceful degradation -----------------------
	// Probe the user namespace first: without it an unprivileged process
	// cannot mount, pivot_root or map ids, so we must stop early with an
	// actionable message instead of failing deep inside the child.
	haveUser := probeNS("user", false)
	if !realRoot && !haveUser {
		return nil, errors.New("user namespaces are unavailable and you are not root.\n" +
			"  Enable them:  sudo sysctl -w kernel.unprivileged_userns_clone=1\n" +
			"  Or use proot/bwrap, which fake the rootfs in userspace")
	}

	// Non-root must use a userns. Root gets full access without one (and
	// is then able to represent *all* uids, not just 0).
	useUser := !realRoot && haveUser
	if !useUser {
		spec.NoUser = true
	}

	if !spec.NoUTS && !probeNS("uts", !realRoot) {
		warn("UTS namespace unavailable; keeping the host hostname")
		spec.NoUTS = true
	}
	if !spec.NoIPC && !probeNS("ipc", !realRoot) {
		warn("IPC namespace unavailable; continuing without it")
		spec.NoIPC = true
	}
	if !spec.NoPID && !probeNS("pid", !realRoot) {
		warn("PID namespace unavailable; the host /proc will be visible")
		spec.NoPID = true
	}
	if !spec.ShareNet && !probeNS("net", !realRoot) {
		warn("network namespace unavailable; sharing the host network")
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
		attr.UidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		}
		attr.GidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		}
		attr.GidMappingsEnableSetgroups = false
	}

	// Interactive terminal: put the container in the foreground process
	// group of the inherited controlling terminal so that job control and
	// Ctrl-C work. We deliberately do NOT setsid(): claiming an already
	// owned tty as the controlling terminal of a brand new session fails
	// with EPERM, so we stay in the parent's session and just take the
	// foreground.
	if tty {
		attr.Setpgid = true
		attr.Foreground = true
		attr.Ctty = 0
	}

	cmd := exec.Command(selfPath(), "__init")
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(), "GOROOT_SPEC="+marshalSpec(spec))
	cmd.SysProcAttr = attr
	return cmd, nil
}

// runParent implements the foreground `run` command: start one container
// attached to the current terminal and wait for it.
func runParent(spec *Spec) int {
	if err := prepareSpec(spec); err != nil {
		fatal("%v", err)
	}

	tty := isTerminal(os.Stdin.Fd())
	cmd, err := buildContainerCmd(spec, os.Stdin, os.Stdout, os.Stderr, tty)
	if err != nil {
		fatal("%v", err)
	}

	if err := cmd.Start(); err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EINVAL) {
			fatal("cannot create namespaces: %v\n"+
				"  Try `goroot doctor` to see what is supported here.", err)
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

	return waitExitCode(cmd)
}

// waitExitCode waits for cmd and maps its result to a process exit code.
func waitExitCode(cmd *exec.Cmd) int {
	err := cmd.Wait()
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

func warn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "goroot: warning: "+format+"\n", a...)
}

func info(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "goroot: "+format+"\n", a...)
}

// addDefaultResolv binds the host's /etc/resolv.conf read-only into the
// container unless the user disabled it or already mounted something there.
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
