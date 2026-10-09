//go:build linux

package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"goroot/assets"
	"goroot/sandbox"
)

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
	fmt.Printf("built-in rootfs : %s (%d bytes, %s)\n",
		assets.TarballName, len(assets.Tarball), assets.RootDir)

	caps := sandbox.Detect()
	fmt.Println("namespaces      :")
	status := []struct {
		name string
		ok   bool
	}{
		{"user", caps.User},
		{"mount", caps.Mount},
		{"pid", caps.Pid},
		{"uts", caps.Uts},
		{"ipc", caps.Ipc},
		{"net", caps.Net},
	}
	for _, s := range status {
		switch {
		case s.name == "user" && caps.RealRoot:
			fmt.Printf("  %-6s n/a (running as real root, not needed)\n", s.name)
		case !caps.RealRoot && !caps.User && s.name != "user":
			fmt.Printf("  %-6s n/a (requires a user namespace)\n", s.name)
		case s.ok:
			fmt.Printf("  %-6s ok\n", s.name)
		default:
			fmt.Printf("  %-6s UNSUPPORTED\n", s.name)
		}
	}

	fmt.Printf("setuid helpers  : newuidmap=%v newgidmap=%v\n",
		fileExists("/usr/bin/newuidmap") || fileExists("/bin/newuidmap"),
		fileExists("/usr/bin/newgidmap") || fileExists("/bin/newgidmap"))
	fmt.Printf("subuid entry    : %s\n", subidLine("/etc/subuid"))
	fmt.Printf("subgid entry    : %s\n", subidLine("/etc/subgid"))

	fmt.Println("verdict         :")
	switch {
	case caps.RealRoot:
		fmt.Println("  can run (root): full namespace isolation, all uids available")
	case !caps.User:
		fmt.Println("  CANNOT run as unprivileged user: user namespaces are unavailable.")
		fmt.Println("  -> enable them (sysctl kernel.unprivileged_userns_clone=1) or use proot/bwrap.")
	default:
		var degraded []string
		if !caps.Pid {
			degraded = append(degraded, "pid")
		}
		if !caps.Net {
			degraded = append(degraded, "net")
		}
		if !caps.Uts {
			degraded = append(degraded, "uts")
		}
		if !caps.Ipc {
			degraded = append(degraded, "ipc")
		}
		if len(degraded) == 0 {
			fmt.Println("  can run: full isolation")
		} else {
			fmt.Printf("  can run with degradation (dropped: %s)\n", strings.Join(degraded, ", "))
		}
	}
	return 0
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

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
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
