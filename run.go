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

// runParent is executed in the CLI process. It decides which namespaces this
// kernel/environment can actually provide, degrades gracefully, then
// re-executes itself as the hidden "__init" stage inside them.
func runParent(spec *Spec) int {
	// No -r/--root: fall back to the Alpine minirootfs embedded in the binary.
	if spec.Rootfs == "" {
		dir, err := ensureEmbeddedRootfs()
		if err != nil {
			fatal("prepare built-in rootfs: %v", err)
		}
		info("using built-in Alpine minirootfs (cache: %s)", dir)
		spec.Rootfs = dir
	}

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

	// Default UX: make the host resolver available read-only inside the
	// container so DNS works out of the box (unless the user overrode it or
	// opted out).
	addDefaultResolv(spec)

	realRoot := os.Geteuid() == 0

	// --- Capability probing & graceful degradation -----------------------
	// Probe the user namespace first: without it an unprivileged process
	// cannot mount, pivot_root or map ids, so we must stop early with an
	// actionable message instead of failing deep inside the child.
	haveUser := probeNS("user", false)
	if !realRoot && !haveUser {
		fatal("user namespaces are unavailable and you are not root.\n" +
			"  Enable them:  sudo sysctl -w kernel.unprivileged_userns_clone=1\n" +
			"  Or use proot/bwrap, which fake the rootfs in userspace.")
	}

	// Non-root must use a userns. Root gets full access without one (and
	// is then able to represent *all* uids, not just 0).
	useUser := !realRoot && haveUser
	if !useUser {
		spec.NoUser = true
	}

	uts := !spec.NoUTS && probeNS("uts", !realRoot)
	if !spec.NoUTS && !uts {
		warn("UTS namespace unavailable; keeping the host hostname")
		spec.NoUTS = true
	}
	ipc := !spec.NoIPC && probeNS("ipc", !realRoot)
	if !spec.NoIPC && !ipc {
		warn("IPC namespace unavailable; continuing without it")
		spec.NoIPC = true
	}
	pid := !spec.NoPID && probeNS("pid", !realRoot)
	if !spec.NoPID && !pid {
		warn("PID namespace unavailable; the host /proc will be visible")
		spec.NoPID = true
	}
	net := !spec.ShareNet && probeNS("net", !realRoot)
	if !spec.ShareNet && !net {
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
	tty := isTerminal(os.Stdin.Fd())
	if tty {
		attr.Setpgid = true
		attr.Foreground = true
		attr.Ctty = 0
	}

	cmd := exec.Command(selfPath(), "__init")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "GOROOT_SPEC="+marshalSpec(spec))
	cmd.SysProcAttr = attr

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

func warn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "goroot: warning: "+format+"\n", a...)
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

func info(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "goroot: "+format+"\n", a...)
}
