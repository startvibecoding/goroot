package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/startvibecoding/goroot/assets"
	"github.com/startvibecoding/goroot/buildkit"
	"github.com/startvibecoding/goroot/sandbox"
)

// resolveRootfs fills in spec.Rootfs with the extracted built-in rootfs when the
// caller did not provide one.
func resolveRootfs(spec *sandbox.Spec) error {
	if spec.Rootfs != "" {
		return applyImageConfig(spec)
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

// applyImageConfig loads the Dockerfile-derived configuration stored beside a
// local goroot image. A plain rootfs directory has no metadata and is left as-is.
func applyImageConfig(spec *sandbox.Spec) error {
	abs, err := filepath.Abs(spec.Rootfs)
	if err != nil {
		return err
	}
	cfg, err := buildkit.ReadConfig(abs)
	if err != nil {
		return nil
	}
	spec.Rootfs = filepath.Join(abs, buildkit.RootfsDir)
	if fi, err := os.Stat(spec.Rootfs); err != nil || !fi.IsDir() {
		return fmt.Errorf("image %s has no %s directory", abs, buildkit.RootfsDir)
	}
	if cfg.Cwd != "" {
		spec.Cwd = cfg.Cwd
	}
	spec.Env = append(append([]string(nil), cfg.Env...), spec.Env...)
	if len(spec.Argv) == 0 {
		spec.Argv = append(append([]string(nil), cfg.Entrypoint...), cfg.Cmd...)
	} else if len(cfg.Entrypoint) > 0 && !equalArgs(spec.Argv[:1], cfg.Entrypoint) {
		// Mimic docker run: positional args replace CMD but retain ENTRYPOINT.
		spec.Argv = append(append([]string(nil), cfg.Entrypoint...), spec.Argv...)
	}
	if cfg.User != "" && spec.UID == 0 && spec.GID == 0 {
		uid, gid, perr := parseUserSpec(cfg.User)
		if perr != nil {
			return fmt.Errorf("image %s: %w", abs, perr)
		}
		spec.UID, spec.GID = uid, gid
	}
	return nil
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func parseUserSpec(s string) (int, int, error) {
	name, group, _ := strings.Cut(s, ":")
	uid, err := strconv.Atoi(name)
	if err != nil {
		return 0, 0, fmt.Errorf("USER %q is not supported rootlessly; only numeric uid 0 is mapped", s)
	}
	gid := uid
	if group != "" {
		gid, err = strconv.Atoi(group)
		if err != nil {
			return 0, 0, fmt.Errorf("USER group %q is not supported rootlessly", group)
		}
	}
	return uid, gid, nil
}
