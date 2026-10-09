//go:build linux

package sandbox

import (
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Caps reports which namespaces this host/environment can provide.
type Caps struct {
	RealRoot bool
	User     bool
	Mount    bool
	Pid      bool
	Uts      bool
	Ipc      bool
	Net      bool
}

type nsSpec struct {
	name string
	flag uintptr
}

func nsList() []nsSpec {
	return []nsSpec{
		{"user", unix.CLONE_NEWUSER},
		{"mount", unix.CLONE_NEWNS},
		{"pid", unix.CLONE_NEWPID},
		{"uts", unix.CLONE_NEWUTS},
		{"ipc", unix.CLONE_NEWIPC},
		{"net", unix.CLONE_NEWNET},
	}
}

// execPath is the binary to re-exec for the init/probe child stages. For the
// importing program this is its own executable.
func execPath() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "/proc/self/exe"
}

// probeNS reports whether the named namespace can be created. Probing must go
// through clone(2) via fork+exec: a Go process is multithreaded and the kernel
// refuses unshare(CLONE_NEWUSER) from a process sharing its address space.
// For an unprivileged user the namespace is requested together with CLONE_NEWUSER.
func probeNS(name string, nonRoot bool) bool {
	if nsDisabledByEnv(name) {
		return false
	}
	flag, ok := nsFlag(name)
	if !ok {
		return false
	}
	flags := flag
	attr := &syscall.SysProcAttr{}
	env := append(os.Environ(), "GOROOT_PROBE=1")
	if nonRoot {
		flags |= unix.CLONE_NEWUSER
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
		attr.GidMappingsEnableSetgroups = false
	}
	attr.Cloneflags = flags

	cmd := exec.Command(execPath())
	cmd.Env = env
	cmd.SysProcAttr = attr
	if devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
		defer devnull.Close()
	}
	return cmd.Run() == nil
}

func nsFlag(name string) (uintptr, bool) {
	for _, n := range nsList() {
		if n.name == name {
			return n.flag, true
		}
	}
	return 0, false
}

// Detect probes every namespace and returns the result.
func Detect() Caps {
	c := Caps{RealRoot: os.Geteuid() == 0}
	c.User = probeNS("user", false)
	nonRoot := !c.RealRoot
	if c.RealRoot || c.User {
		c.Mount = probeNS("mount", nonRoot)
		c.Pid = probeNS("pid", nonRoot)
		c.Uts = probeNS("uts", nonRoot)
		c.Ipc = probeNS("ipc", nonRoot)
		c.Net = probeNS("net", nonRoot)
	}
	return c
}

func nsDisabledByEnv(name string) bool {
	v := os.Getenv("GOROOT_DISABLE_NS")
	if v == "" {
		return false
	}
	for _, n := range strings.Split(v, ",") {
		if strings.TrimSpace(n) == name {
			return true
		}
	}
	return false
}
