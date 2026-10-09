//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Namespace capabilities must be probed with clone(2), not unshare(2): a Go
// process is multithreaded and the kernel refuses unshare(CLONE_NEWUSER) from
// a process that shares its address space with other threads (EINVAL). The
// real container creates namespaces through fork+exec, so we probe the same
// way -- by launching a trivial copy of ourselves inside the namespace.

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

func nsFlag(name string) (uintptr, bool) {
	for _, n := range nsList() {
		if n.name == name {
			return n.flag, true
		}
	}
	return 0, false
}

// probeOK implements the hidden `__probeok` stage: it does nothing and exits,
// merely proving that the namespace setup performed by the parent succeeded.
func probeOK(_ []string) int { return 0 }

func selfPath() string {
	if _, err := os.Stat("/proc/self/exe"); err == nil {
		return "/proc/self/exe"
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "goroot"
}

// probeNS reports whether the given namespace can be created. For an
// unprivileged user the namespace must be requested together with (and is
// thus owned by) a user namespace, so we add CLONE_NEWUSER + id mappings.
//
// GOROOT_DISABLE_NS (comma separated namespace names) forces namespaces to be
// treated as unavailable; it exists to exercise the degradation paths.
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
	if nonRoot {
		flags |= unix.CLONE_NEWUSER
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
		attr.GidMappingsEnableSetgroups = false
	}
	attr.Cloneflags = flags

	cmd := exec.Command(selfPath(), "__probeok")
	cmd.SysProcAttr = attr
	if devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
		defer devnull.Close()
	}
	return cmd.Run() == nil
}

func cstr(b []byte) string {
	if i := indexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func readSysctl(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(b))
}

// cmdDoctor probes the environment and prints what goroot can and cannot do
// here. It is the answer to "what happens on an older kernel?".
func cmdDoctor(_ []string) int {
	var u unix.Utsname
	if err := unix.Uname(&u); err == nil {
		fmt.Printf("kernel          : %s (%s)\n", cstr(u.Release[:]), cstr(u.Machine[:]))
	}

	euid := os.Geteuid()
	who := "unprivileged"
	if euid == 0 {
		who = "root"
	}
	fmt.Printf("euid            : %d (%s)\n", euid, who)
	fmt.Printf("userns sysctls  : unprivileged_userns_clone=%s max_user_namespaces=%s\n",
		readSysctl("/proc/sys/kernel/unprivileged_userns_clone"),
		readSysctl("/proc/sys/user/max_user_namespaces"))

	realRoot := euid == 0
	haveUser := probeNS("user", false)

	fmt.Println("namespaces      :")
	if !haveUser {
		if realRoot {
			fmt.Println("  user   n/a (running as real root, not needed)")
		} else {
			fmt.Println("  user   UNSUPPORTED  <-- blocking")
		}
	} else {
		fmt.Println("  user   ok")
	}
	for _, n := range nsList() {
		if n.name == "user" {
			continue
		}
		if !realRoot && !haveUser {
			fmt.Printf("  %-6s n/a (requires a user namespace)\n", n.name)
			continue
		}
		if probeNS(n.name, !realRoot) {
			fmt.Printf("  %-6s ok\n", n.name)
		} else {
			fmt.Printf("  %-6s UNSUPPORTED\n", n.name)
		}
	}

	fmt.Printf("setuid helpers  : newuidmap=%v newgidmap=%v\n",
		fileExists("/usr/bin/newuidmap") || fileExists("/bin/newuidmap"),
		fileExists("/usr/bin/newgidmap") || fileExists("/bin/newgidmap"))
	fmt.Printf("subuid entry    : %s\n", subidLine("/etc/subuid"))
	fmt.Printf("subgid entry    : %s\n", subidLine("/etc/subgid"))

	fmt.Println("verdict         :")
	switch {
	case realRoot:
		fmt.Println("  can run (root): full namespace isolation, all uids available")
	case !haveUser:
		fmt.Println("  CANNOT run as unprivileged user: user namespaces are unavailable.")
		fmt.Println("  -> enable them (sysctl kernel.unprivileged_userns_clone=1) or use proot/bwrap.")
	default:
		var degraded []string
		for _, n := range nsList() {
			if n.name == "user" {
				continue
			}
			if !probeNS(n.name, true) {
				degraded = append(degraded, n.name)
			}
		}
		if len(degraded) == 0 {
			fmt.Println("  can run: full isolation")
		} else {
			fmt.Printf("  can run with degradation (dropped: %s)\n", strings.Join(degraded, ", "))
		}
	}
	return 0
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
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

func subidLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(none)"
	}
	names := []string{fmt.Sprintf("%d", os.Getuid())}
	if u := os.Getenv("USER"); u != "" {
		names = append(names, u)
	}
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		for _, n := range names {
			if strings.HasPrefix(trimmed, n+":") {
				return trimmed
			}
		}
	}
	return "(none)"
}
