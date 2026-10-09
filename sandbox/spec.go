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

	// HostUID/HostGID are filled in by the runtime.
	HostUID int `json:"hostUid"`
	HostGID int `json:"hostGid"`
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
