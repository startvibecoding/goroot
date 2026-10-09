//go:build linux

package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const oldRootName = ".goroot_oldroot"

// initMain runs inside the freshly created namespaces. It sets up the
// root filesystem, pivots into it and finally execs the target command.
func initMain() int {
	spec := loadSpecFromEnv()
	if err := setupContainer(spec); err != nil {
		fmt.Fprintf(os.Stderr, "goroot: %v\n", err)
		return 125
	}
	if spec.UseInit {
		return runTinyInit(spec)
	}
	// Everything is in place: replace ourselves with the target process.
	argv0, argv, env, err := buildExec(spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "goroot: %v\n", err)
		return 127
	}
	if err := unix.Exec(argv0, argv, env); err != nil {
		fmt.Fprintf(os.Stderr, "goroot: exec %s: %v\n", argv0, err)
		return 127
	}
	return 0
}

// runTinyInit keeps this process as PID 1 inside the namespace and runs the
// target as its child. PID 1 reaps orphaned processes and forwards signals,
// which makes running long-lived services more robust.
func runTinyInit(spec *Spec) int {
	argv0, argv, env, err := buildExec(spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "goroot: %v\n", err)
		return 127
	}
	attr := &os.ProcAttr{
		Dir:   spec.Cwd,
		Env:   env,
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	}
	proc, err := os.StartProcess(argv0, argv, attr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "goroot: start %s: %v\n", argv0, err)
		return 127
	}

	sigc := make(chan os.Signal, 16)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP,
		syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2,
		syscall.SIGWINCH, syscall.SIGCONT)
	go func() {
		for s := range sigc {
			_ = proc.Signal(s)
		}
	}()

	state, err := proc.Wait()
	if err != nil {
		fmt.Fprintf(os.Stderr, "goroot: wait: %v\n", err)
		return 127
	}

	signal.Stop(sigc)
	// Reap any orphaned grandchildren that got reparented to PID 1.
	for {
		var ws syscall.WaitStatus
		pid, _ := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if pid <= 0 {
			break
		}
	}

	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return state.ExitCode()
}

func setupContainer(spec *Spec) error {
	// 0. Make all mount events private so nothing propagates back to the
	// host mount namespace.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}

	root := spec.Rootfs

	// 1. Bind the rootfs onto itself so that pivot_root has a mount
	// point to work with. This must happen BEFORE we add submounts.
	if err := unix.Mount(root, root, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind rootfs onto itself: %w", err)
	}

	// 2. Prepare the standard directory tree inside the rootfs.
	for _, d := range []string{
		"/proc", "/sys", "/dev", "/tmp", "/run",
	} {
		_ = os.MkdirAll(filepath.Join(root, d), 0o755)
	}

	// 3. /proc. Mounting a fresh procfs is only permitted when we own a
	// PID namespace; otherwise fall back to a bind of the host's /proc.
	procDst := filepath.Join(root, "proc")
	if spec.NoPID {
		if err := unix.Mount("/proc", procDst, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("bind /proc: %w", err)
		}
	} else if err := unix.Mount("proc", procDst, "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}

	// 4. /sys: a fresh sysfs is mountable because we own a private
	// network namespace; fall back to a read-only bind of the host's.
	if err := unix.Mount("sysfs", filepath.Join(root, "sys"), "sysfs", unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		sysDst := filepath.Join(root, "sys")
		if berr := unix.Mount("/sys", sysDst, "", unix.MS_BIND|unix.MS_REC, ""); berr == nil {
			_ = unix.Mount("", sysDst, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_REC, "")
		} else {
			// sysfs is tied to the network namespace: when sharing the
			// host netns it simply cannot be exposed. Warn and continue.
			fmt.Fprintln(os.Stderr, "goroot: warning: /sys unavailable (drop --share-net for a private netns)")
		}
	}

	// 5. /dev: fresh tmpfs plus bind-mounted device nodes.
	if err := setupDev(root); err != nil {
		return err
	}

	// 6. /tmp as tmpfs.
	_ = unix.Mount("tmpfs", filepath.Join(root, "tmp"), "tmpfs", 0, "mode=1777")

	// 7. User supplied tmpfs mounts.
	for _, t := range spec.Tmpfs {
		dst := filepath.Join(root, strings.TrimPrefix(t, "/"))
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return fmt.Errorf("tmpfs mkdir %s: %w", t, err)
		}
		if err := unix.Mount("tmpfs", dst, "tmpfs", 0, "mode=1777"); err != nil {
			return fmt.Errorf("mount tmpfs %s: %w", t, err)
		}
	}

	// 8. User supplied bind mounts.
	for _, b := range spec.Binds {
		if err := applyBind(root, b); err != nil {
			return err
		}
	}

	// 9. pivot_root into the new root.
	if err := pivotRoot(root); err != nil {
		return err
	}

	// 10. Hostname (UTS namespace).
	if spec.Hostname != "" && !spec.NoUTS {
		if err := unix.Sethostname([]byte(spec.Hostname)); err != nil {
			fmt.Fprintf(os.Stderr, "goroot: sethostname: %v\n", err)
		}
	}

	// 11. Bring up loopback when we own a private network namespace.
	if !spec.ShareNet {
		if err := setLoopbackUp(); err != nil {
			fmt.Fprintf(os.Stderr, "goroot: bring up lo: %v\n", err)
		}
	}

	// 12. Working directory.
	cwd := spec.Cwd
	if cwd == "" {
		cwd = "/"
	}
	if err := os.Chdir(cwd); err != nil {
		return fmt.Errorf("chdir %s: %w", cwd, err)
	}

	// 13. Drop to the requested uid/gid (still inside the userns).
	// Only uid/gid 0 is mapped in this single-id user namespace.
	if spec.GID != 0 || spec.UID != 0 {
		if err := unix.Setgid(spec.GID); err != nil {
			return fmt.Errorf("setgid %d: %w (only uid/gid 0 is mapped in this rootless userns)", spec.GID, err)
		}
		if err := unix.Setuid(spec.UID); err != nil {
			return fmt.Errorf("setuid %d: %w (only uid/gid 0 is mapped in this rootless userns)", spec.UID, err)
		}
	}

	return nil
}

func setupDev(root string) error {
	dev := filepath.Join(root, "dev")
	if err := unix.Mount("tmpfs", dev, "tmpfs", 0, "mode=0755"); err != nil {
		return fmt.Errorf("mount /dev: %w", err)
	}

	// These directories must be created *after* the tmpfs is mounted,
	// otherwise they are hidden by it.
	for _, d := range []string{"pts", "shm"} {
		if err := os.MkdirAll(filepath.Join(dev, d), 0o755); err != nil {
			return err
		}
	}
	_ = unix.Mount("tmpfs", filepath.Join(dev, "shm"), "tmpfs", 0, "mode=1777")

	// Bind the host device nodes (mknod is not permitted in a userns).
	for _, n := range []string{"null", "zero", "full", "random", "urandom", "tty"} {
		src := "/dev/" + n
		dst := filepath.Join(dev, n)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		f, err := os.OpenFile(dst, os.O_CREATE, 0o666)
		if err == nil {
			f.Close()
		}
		if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
			fmt.Fprintf(os.Stderr, "goroot: bind /dev/%s: %v\n", n, err)
		}
	}

	// A dedicated devpts instance for the container's pseudoterminals.
	pts := filepath.Join(dev, "pts")
	if err := unix.Mount("devpts", pts, "devpts", 0, "newinstance,mode=0620,ptmxmode=0666"); err != nil {
		// Fall back to the host's devpts.
		_ = unix.Mount("/dev/pts", pts, "", unix.MS_BIND, "")
	}

	// Handy symlinks expected by many programs.
	links := map[string]string{
		"fd":     "/proc/self/fd",
		"stdin":  "/proc/self/fd/0",
		"stdout": "/proc/self/fd/1",
		"stderr": "/proc/self/fd/2",
		"ptmx":   "pts/ptmx",
	}
	for name, target := range links {
		p := filepath.Join(dev, name)
		_ = os.Remove(p)
		_ = os.Symlink(target, p)
	}
	return nil
}

func applyBind(root string, b Bind) error {
	src := b.Src
	dst := b.Dst
	if dst == "" {
		dst = src
	}
	target := filepath.Join(root, strings.TrimPrefix(dst, "/"))

	// Create the mount point matching the source type.
	if st, err := os.Stat(src); err == nil {
		if st.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("bind mkdir %s: %w", dst, err)
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE, 0o644)
			if err == nil {
				f.Close()
			}
		}
	}

	flags := uintptr(unix.MS_BIND | unix.MS_REC)
	if err := unix.Mount(src, target, "", flags, ""); err != nil {
		return fmt.Errorf("bind %s -> %s: %w", src, dst, err)
	}
	if b.RO {
		if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("remount ro %s: %w", dst, err)
		}
	}
	return nil
}

func pivotRoot(root string) error {
	oldRoot := filepath.Join(root, oldRootName)
	if err := os.MkdirAll(oldRoot, 0o700); err != nil {
		return fmt.Errorf("create oldroot: %w", err)
	}
	if err := unix.PivotRoot(root, oldRoot); err != nil {
		// pivot_root can fail on exotic setups; fall back to chroot.
		if cerr := unix.Chroot(root); cerr != nil {
			return fmt.Errorf("pivot_root: %v (chroot fallback: %v)", err, cerr)
		}
		return os.Chdir("/")
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	_ = unix.Unmount("/"+oldRootName, unix.MNT_DETACH)
	_ = os.Remove("/" + oldRootName)
	return nil
}

// buildExec resolves argv[0] against the container PATH and assembles the
// final argv/env for the target process.
func buildExec(spec *Spec) (string, []string, []string, error) {
	if len(spec.Argv) == 0 {
		spec.Argv = []string{"/bin/sh"}
	}

	env := buildEnv(spec)
	pathVal := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			pathVal = strings.TrimPrefix(kv, "PATH=")
			break
		}
	}

	argv0 := spec.Argv[0]
	if !strings.Contains(argv0, "/") {
		resolved, err := lookPath(argv0, pathVal)
		if err != nil {
			return "", nil, nil, err
		}
		argv0 = resolved
	}
	return argv0, spec.Argv, env, nil
}

func lookPath(file, path string) (string, error) {
	for _, dir := range strings.Split(path, ":") {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, file)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in PATH", file)
}

func buildEnv(spec *Spec) []string {
	var env []string
	if spec.KeepEnv {
		env = append(env, os.Environ()...)
	}
	env = append(env,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"TERM="+firstNonEmpty(os.Getenv("TERM"), "xterm"),
		"GOROOT_CONTAINER=1",
	)
	// Spec env overrides/extends.
	env = append(env, spec.Env...)
	return dedupEnv(env)
}

func dedupEnv(env []string) []string {
	seen := map[string]int{}
	var out []string
	for _, kv := range env {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if idx, ok := seen[key]; ok {
			out[idx] = kv
			continue
		}
		seen[key] = len(out)
		out = append(out, kv)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
