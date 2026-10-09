package main

import (
	"encoding/json"
	"os"
)

// Bind describes a single bind mount from Src (on the host) to Dst
// (inside the container rootfs). Dst may be empty, in which case it
// defaults to Src.
type Bind struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
	RO  bool   `json:"ro"`
}

// Spec is the full description of the container to start. It is
// serialised to JSON and handed to the child (__init) process through
// the GOROOT_SPEC environment variable.
type Spec struct {
	Rootfs   string   `json:"rootfs"`
	Hostname string   `json:"hostname"`
	Binds    []Bind   `json:"binds"`
	Env      []string `json:"env"`
	KeepEnv  bool     `json:"keepEnv"`
	Cwd      string   `json:"cwd"`
	Tmpfs    []string `json:"tmpfs"`
	ShareNet bool     `json:"shareNet"`
	NoResolv bool     `json:"noResolv"`
	NoPID    bool     `json:"noPid"`
	NoIPC    bool     `json:"noIpc"`
	NoUTS    bool     `json:"noUts"`
	NoUser   bool     `json:"noUser"`
	UID      int      `json:"uid"`
	GID      int      `json:"gid"`
	UseInit  bool     `json:"useInit"`
	Argv     []string `json:"argv"`

	// Filled in by the parent at runtime.
	HostUID int `json:"hostUid"`
	HostGID int `json:"hostGid"`
}

func marshalSpec(s *Spec) string {
	b, err := json.Marshal(s)
	if err != nil {
		fatal("encode spec: %v", err)
	}
	return string(b)
}

func loadSpecFromEnv() *Spec {
	raw := os.Getenv("GOROOT_SPEC")
	if raw == "" {
		fatal("missing GOROOT_SPEC (internal error)")
	}
	var s Spec
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		fatal("decode spec: %v", err)
	}
	return &s
}
