//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// cgroupMount is the standard unified (v2) hierarchy mount point.
const cgroupMount = "/sys/fs/cgroup"

// cgroup is a leaf cgroup created for one container. Limits are written before
// the container starts and, when possible, the container is placed into it with
// CLONE_INTO_CGROUP so the whole process tree is contained from birth.
type cgroup struct {
	path string
}

// CgroupSupport describes rootless cgroup v2 availability on this host.
type CgroupSupport struct {
	// V2 is true when a unified cgroup v2 hierarchy is mounted.
	V2 bool
	// Delegated is true when a writable cgroup subtree exists that rootless
	// resource limits can be created under.
	Delegated bool
	// Path is the delegated cgroup directory (empty when not delegated).
	Path string
	// Controllers lists the controllers usable under Path.
	Controllers []string
}

// DetectCgroup reports whether rootless cgroup limits are usable.
func DetectCgroup() CgroupSupport {
	s := CgroupSupport{V2: hasCgroupV2()}
	if !s.V2 {
		return s
	}
	own, err := ownCgroupDir()
	if err != nil {
		return s
	}
	for _, dir := range cgroupCandidates(own) {
		if !canMkdir(dir) {
			continue
		}
		s.Delegated = true
		s.Path = dir
		s.Controllers = strings.Fields(readTrim(filepath.Join(dir, "cgroup.controllers")))
		return s
	}
	return s
}

func hasCgroupV2() bool {
	_, err := os.Stat(filepath.Join(cgroupMount, "cgroup.controllers"))
	return err == nil
}

// ownCgroupDir returns the cgroup v2 directory of the current process.
func ownCgroupDir() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join(cgroupMount, filepath.Clean("/"+rest)), nil
		}
	}
	return "", errors.New("no cgroup v2 entry in /proc/self/cgroup")
}

// cgroupCandidates lists the current cgroup and its ancestors (deepest first),
// stopping at the cgroup v2 mount root.
func cgroupCandidates(own string) []string {
	var out []string
	for d := own; strings.HasPrefix(d, cgroupMount); d = filepath.Dir(d) {
		out = append(out, d)
	}
	return out
}

// canMkdir reports whether we can create a child cgroup under dir.
func canMkdir(dir string) bool {
	t, err := os.MkdirTemp(dir, ".goroot-probe-")
	if err != nil {
		return false
	}
	_ = os.Remove(t)
	return true
}

// newCgroup creates a leaf cgroup for spec's limits under the deepest writable
// delegated ancestor. It returns an error when no usable subtree exists.
func newCgroup(spec *Spec) (*cgroup, error) {
	if !hasCgroupV2() {
		return nil, errors.New("cgroup v2 is not mounted")
	}
	own, err := ownCgroupDir()
	if err != nil {
		return nil, err
	}

	var needed []string
	if spec.MemoryMax > 0 || spec.MemoryHigh > 0 {
		needed = append(needed, "memory")
	}
	if spec.CPUQuota > 0 {
		needed = append(needed, "cpu")
	}
	if spec.PidsMax > 0 {
		needed = append(needed, "pids")
	}

	base := fmt.Sprintf("goroot-%d", os.Getpid())
	for _, dir := range cgroupCandidates(own) {
		if !enableControllers(dir, needed) {
			continue
		}
		sub := filepath.Join(dir, base)
		if err := os.Mkdir(sub, 0o755); err != nil {
			if !errors.Is(err, fs.ErrExist) {
				continue
			}
			// A stale leftover with our pid is effectively impossible, but be
			// safe and pick a unique name.
			sub = filepath.Join(dir, fmt.Sprintf("%s-%d", base, time.Now().UnixNano()))
			if err := os.Mkdir(sub, 0o755); err != nil {
				continue
			}
		}
		c := &cgroup{path: sub}
		if err := c.apply(spec); err != nil {
			_ = os.Remove(sub)
			return nil, err
		}
		return c, nil
	}
	return nil, errors.New("no writable delegated cgroup (systemd delegation disabled?)")
}

// enableControllers ensures dir's cgroup.subtree_control contains every
// controller in needed, returning false when that is not possible here
// (not permitted, controller unavailable, or busy with internal processes).
func enableControllers(dir string, needed []string) bool {
	if len(needed) == 0 {
		return canMkdir(dir)
	}
	cur := readSet(filepath.Join(dir, "cgroup.subtree_control"))
	avail := readSet(filepath.Join(dir, "cgroup.controllers"))

	var toEnable []string
	for _, c := range needed {
		if cur[c] {
			continue
		}
		if !avail[c] {
			return false // controller cannot be enabled here
		}
		toEnable = append(toEnable, "+"+c)
	}
	if len(toEnable) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"),
			[]byte(strings.Join(toEnable, " ")), 0o644); err != nil {
			return false
		}
		cur = readSet(filepath.Join(dir, "cgroup.subtree_control"))
		for _, c := range needed {
			if !cur[c] {
				return false
			}
		}
	}
	return canMkdir(dir)
}

// apply writes the requested limits into the leaf cgroup.
func (c *cgroup) apply(spec *Spec) error {
	set := 0
	if spec.MemoryMax > 0 {
		if err := c.write("memory.max", strconv.FormatInt(spec.MemoryMax, 10)); err != nil {
			return err
		}
		set++
	}
	if spec.MemoryHigh > 0 {
		if err := c.write("memory.high", strconv.FormatInt(spec.MemoryHigh, 10)); err != nil {
			return err
		}
		set++
	}
	if spec.CPUQuota > 0 {
		const period = 100000 // 100ms, the cgroup v2 default
		quota := int64(spec.CPUQuota * float64(period))
		if quota < 1000 {
			quota = 1000 // kernel minimum: 1ms
		}
		if err := c.write("cpu.max", fmt.Sprintf("%d %d", quota, period)); err != nil {
			return err
		}
		set++
	}
	if spec.PidsMax > 0 {
		if err := c.write("pids.max", strconv.FormatInt(spec.PidsMax, 10)); err != nil {
			return err
		}
		set++
	}
	if set == 0 {
		return errors.New("no cgroup limits requested")
	}
	return nil
}

func (c *cgroup) write(file, val string) error {
	p := filepath.Join(c.path, file)
	if _, err := os.Stat(p); err != nil {
		return fmt.Errorf("cgroup controller %q unavailable: %w", file, err)
	}
	return os.WriteFile(p, []byte(val), 0o644)
}

// openFD opens the cgroup directory for CLONE_INTO_CGROUP.
func (c *cgroup) openFD() (int, error) {
	return unix.Open(c.path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
}

// addPID moves an already-running process into the cgroup (fallback attach).
func (c *cgroup) addPID(pid int) error {
	return os.WriteFile(filepath.Join(c.path, "cgroup.procs"),
		[]byte(strconv.Itoa(pid)), 0o644)
}

// close removes the leaf cgroup. Best-effort: it may fail if processes remain.
func (c *cgroup) close() {
	if c == nil || c.path == "" {
		return
	}
	// Reclaim disk/memory accounting before the final rmdir.
	_ = os.Remove(c.path)
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readSet(path string) map[string]bool {
	m := map[string]bool{}
	for _, f := range strings.Fields(readTrim(path)) {
		f = strings.TrimPrefix(f, "+")
		if f != "" {
			m[f] = true
		}
	}
	return m
}
