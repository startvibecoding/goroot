//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFake(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEnableControllers(t *testing.T) {
	dir := t.TempDir()
	writeFake(t, dir, "cgroup.controllers", "cpu memory pids\n")
	writeFake(t, dir, "cgroup.subtree_control", "cpu\n")

	if !enableControllers(dir, []string{"memory", "pids"}) {
		t.Fatal("enableControllers returned false for an available controller")
	}
	// Real cgroupfs accumulates writes to subtree_control; a plain file does
	// not, so only assert on the controllers we asked to enable.
	got := readSet(filepath.Join(dir, "cgroup.subtree_control"))
	for _, c := range []string{"memory", "pids"} {
		if !got[c] {
			t.Errorf("subtree_control missing %q (got %v)", c, got)
		}
	}
}

func TestEnableControllersUnavailable(t *testing.T) {
	dir := t.TempDir()
	writeFake(t, dir, "cgroup.controllers", "cpu\n")
	writeFake(t, dir, "cgroup.subtree_control", "\n")
	if enableControllers(dir, []string{"memory"}) {
		t.Fatal("enableControllers should fail when the controller is unavailable")
	}
}

func TestCgroupApply(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"memory.max", "memory.high", "cpu.max", "pids.max"} {
		writeFake(t, dir, f, "max")
	}
	c := &cgroup{path: dir}
	spec := &Spec{MemoryMax: 1 << 20, MemoryHigh: 512 << 10, CPUQuota: 0.5, PidsMax: 64}
	if err := c.apply(spec); err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := map[string]string{
		"memory.max":  "1048576",
		"memory.high": "524288",
		"cpu.max":     "50000 100000",
		"pids.max":    "64",
	}
	for f, exp := range want {
		if got := readTrim(filepath.Join(dir, f)); got != exp {
			t.Errorf("%s = %q, want %q", f, got, exp)
		}
	}
}

func TestCgroupApplyMissingController(t *testing.T) {
	c := &cgroup{path: t.TempDir()}
	if err := c.apply(&Spec{MemoryMax: 1 << 20}); err == nil {
		t.Fatal("apply should error when memory.max is absent")
	}
}
