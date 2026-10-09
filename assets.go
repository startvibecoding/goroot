//go:build linux

package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// The built-in default rootfs: an Alpine minirootfs shipped inside the binary
// (see assets/). This is what `goroot run` uses when no -r/--root is given, so
// the tool works out of the box on a fresh machine.
const (
	embeddedTarball = "alpine-minirootfs-3.20.0-x86_64.tar.gz"
	embeddedRootDir = "alpine-3.20.0-x86_64"
)

//go:embed assets/alpine-minirootfs-3.20.0-x86_64.tar.gz
var embeddedRootfs []byte

// ensureEmbeddedRootfs extracts the built-in rootfs to a per-user cache on first
// use and returns its path. Extraction is done into a temporary directory that
// is atomically renamed, so concurrent invocations are safe.
func ensureEmbeddedRootfs() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "goroot", embeddedRootDir)
	marker := filepath.Join(dir, ".goroot-ok")
	if _, err := os.Stat(marker); err == nil {
		return dir, nil
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".extract-*")
	if err != nil {
		return "", err
	}
	if err := untar(bytes.NewReader(embeddedRootfs), tmp, embeddedTarball, 0); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("extract built-in rootfs: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".goroot-ok"), []byte("1\n"), 0o644); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}

	if err := os.Rename(tmp, dir); err != nil {
		os.RemoveAll(tmp)
		// Another process may have won the race and populated dir already.
		if _, e := os.Stat(marker); e == nil {
			return dir, nil
		}
		return "", err
	}
	return dir, nil
}
