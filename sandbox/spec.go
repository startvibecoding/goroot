// Package sandbox provides rootless container ("sandbox") primitives: it boots
// a rootfs as an unprivileged user using Linux user namespaces.
//
// Typical use from another Go program:
//
//	func main() {
//		sandbox.Init() // MUST be the first statement; handles the re-exec child
//		code, err := sandbox.Run(context.Background(), &sandbox.Spec{
//			Rootfs: "/path/to/rootfs",
//			Argv:   []string{"/bin/sh", "-c", "echo hello"},
//			Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin,
//		}, sandbox.Stdio{Stdout: os.Stdout, Stderr: os.Stderr})
//		_ = code
//		_ = err
//	}
//
// sandbox.Init is required because the container init stage runs as a re-exec
// of the *importing* program with a marker environment variable.
package sandbox

import (
	"encoding/json"
	"io"
	"os"
)

// Bind describes a bind mount from Src (on the host) to Dst (inside the
// container). Dst defaults to Src when empty.
type Bind struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
	RO  bool   `json:"ro"`
}

// Spec fully describes the container to start.
type Spec struct {
	// Rootfs is the directory to use as the container's root filesystem.
	Rootfs string `json:"rootfs"`
	// Argv is the command to run inside the container. When empty a shell is
	// auto-selected from /bin/sh, /bin/bash, /bin/zsh, /bin/fish.
	Argv []string `json:"argv"`

	Hostname string   `json:"hostname"`
	Binds    []Bind   `json:"binds"`
	Tmpfs    []string `json:"tmpfs"`
	Env      []string `json:"env"`
	KeepEnv  bool     `json:"keepEnv"`
	Cwd      string   `json:"cwd"`

	ShareNet bool `json:"shareNet"` // share the host network namespace
	NoResolv bool `json:"noResolv"` // do not bind the host /etc/resolv.conf
	NoPID    bool `json:"noPid"`
	NoIPC    bool `json:"noIpc"`
	NoUTS    bool `json:"noUts"`
	NoUser   bool `json:"noUser"` // do not create/enter a user namespace

	UID int `json:"uid"`
	GID int `json:"gid"`

	// UseInit runs a tiny PID 1 that reaps orphans and forwards signals.
	UseInit bool `json:"useInit"`

	// --- resource limits ---------------------------------------------------
	//
	// Limits are enforced with cgroup v2 when the host delegates a writable
	// cgroup subtree to the user (the preferred path, precise). When cgroups
	// are unavailable the runtime degrades to rlimits (coarser), and warns for
	// limits that cannot be expressed that way. Zero means "no limit".

	// MemoryMax is a hard memory ceiling in bytes (cgroup memory.max;
	// RLIMIT_AS as fallback).
	MemoryMax int64 `json:"memoryMax,omitempty"`
	// MemoryHigh is a soft memory ceiling in bytes that only throttles reclaim
	// (cgroup memory.high). cgroup-only; ignored with a warning on fallback.
	MemoryHigh int64 `json:"memoryHigh,omitempty"`
	// CPUQuota is the CPU bandwidth in cores (e.g. 0.5 = half a core).
	// Enforced as cgroup cpu.max; not expressible with rlimits.
	CPUQuota float64 `json:"cpuQuota,omitempty"`
	// CPUTime is a cumulative CPU-time budget in seconds (RLIMIT_CPU); the
	// process is killed with SIGXCPU/SIGKILL once it burns through it. Always
	// applied via rlimit, with or without cgroups.
	CPUTime int64 `json:"cpuTime,omitempty"`
	// PidsMax caps the number of tasks/threads (cgroup pids.max; RLIMIT_NPROC
	// as fallback, only meaningful for real root since nproc is per host uid).
	PidsMax int64 `json:"pidsMax,omitempty"`
	// NoCgroup forces the rlimit fallback even when cgroup v2 is available.
	NoCgroup bool `json:"noCgroup,omitempty"`

	// The following are filled in by the runtime: the rlimit values the child
	// stage must apply when cgroups are unavailable. Not set by callers.
	RlimitAS    int64 `json:"rlimitAS,omitempty"`
	RlimitCPU   int64 `json:"rlimitCPU,omitempty"`
	RlimitNPROC int64 `json:"rlimitNproc,omitempty"`

	// HostUID/HostGID are filled in by the runtime.
	HostUID int `json:"hostUid"`
	HostGID int `json:"hostGid"`
}

// needsCgroup reports whether any requested limit requires cgroup v2.
func (s *Spec) needsCgroup() bool {
	return s.MemoryMax > 0 || s.MemoryHigh > 0 || s.CPUQuota > 0 || s.PidsMax > 0
}

// hasLimits reports whether any resource limit was requested at all.
func (s *Spec) hasLimits() bool {
	return s.needsCgroup() || s.CPUTime > 0
}

// Stdio wires the container's standard streams. Any io.Reader/Writer works;
// when nil, stdin is empty and output is discarded. TTY requests interactive
// terminal handling (foreground process group); it is auto-detected when Stdin
// or Stdout is a terminal *os.File.
type Stdio struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	TTY    bool
}

func (s *Spec) netMode() string {
	if s.ShareNet {
		return "shared"
	}
	return "isolated"
}

func marshalSpec(s *Spec) string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(b)
}

func loadSpecFromEnv() *Spec {
	raw := os.Getenv("GOROOT_SPEC")
	if raw == "" {
		return &Spec{}
	}
	var s Spec
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return &Spec{}
	}
	return &s
}
