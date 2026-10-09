package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/startvibecoding/goroot/assets"
	"github.com/startvibecoding/goroot/sandbox"
)

// resolveRootfs fills in spec.Rootfs with the extracted built-in rootfs when the
// caller did not provide one.
func resolveRootfs(spec *sandbox.Spec) error {
	if spec.Rootfs != "" {
		return nil
	}
	dir, err := ensureEmbeddedRootfs()
	if err != nil {
		return fmt.Errorf("prepare built-in rootfs: %w", err)
	}
	info("using built-in Alpine minirootfs (cache: %s)", dir)
	spec.Rootfs = dir
	return nil
}

// ensureEmbeddedRootfs extracts the embedded Alpine minirootfs to a per-user
// cache on first use and returns its path. Extraction is race-safe (temporary
// directory + atomic rename).
func ensureEmbeddedRootfs() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "goroot", assets.RootDir)
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
	if err := sandbox.Extract(bytes.NewReader(assets.Tarball), tmp, assets.TarballName, 0); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, ".goroot-ok"), []byte("1\n"), 0o644); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}

	if err := os.Rename(tmp, dir); err != nil {
		os.RemoveAll(tmp)
		if _, e := os.Stat(marker); e == nil {
			return dir, nil
		}
		return "", err
	}
	return dir, nil
}
