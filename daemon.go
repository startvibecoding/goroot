package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/startvibecoding/goroot/sandbox"
)

// --- server command ---------------------------------------------------------

func cmdServer(args []string) int {
	daemon := false
	for _, a := range args {
		switch a {
		case "-d", "--daemon":
			daemon = true
		case "-h", "--help":
			fmt.Fprintln(os.Stderr, "usage: goroot server [-d|--daemon]")
			return 0
		default:
			fatal("server: unknown argument %q", a)
		}
	}
	if daemon {
		if err := startDaemon(); err != nil {
			fatal("start daemon: %v", err)
		}
		fmt.Printf("goroot server started (socket: %s)\n", socketPath())
		return 0
	}
	return runServer()
}

// startDaemon launches a detached `goroot server` and returns immediately.
func startDaemon() error {
	if err := os.MkdirAll(gorootHome(), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(gorootHome(), "server.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()

	cmd := exec.Command(selfPath(), "server")
	cmd.Stdin = devnull
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// The daemon must not inherit stray descriptors from the caller (e.g. a
	// control pipe the invoking tool is waiting on), otherwise it keeps that
	// pipe open forever and the caller never sees EOF. Go's exec preserves
	// inherited fds, so mark them close-on-exec. The caller exits right away.
	setCloexecOnStrayFds()

	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// setCloexecOnStrayFds marks every open fd above 2 as close-on-exec.
func setCloexecOnStrayFds() {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return
	}
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil || fd <= 2 {
			continue
		}
		unix.CloseOnExec(fd)
	}
}

func runServer() int {
	home := gorootHome()
	if err := os.MkdirAll(home, 0o700); err != nil {
		fatal("create %s: %v", home, err)
	}
	if err := os.MkdirAll(logsDir(), 0o700); err != nil {
		fatal("create %s: %v", logsDir(), err)
	}

	sp := socketPath()
	if _, err := os.Stat(sp); err == nil {
		if c, derr := net.DialTimeout("unix", sp, time.Second); derr == nil {
			c.Close()
			fatal("server already running on %s", sp)
		}
		_ = os.Remove(sp) // stale socket
	}

	ln, err := net.Listen("unix", sp)
	if err != nil {
		fatal("listen on %s: %v", sp, err)
	}
	defer os.Remove(sp)
	_ = os.Chmod(sp, 0o600)

	m := newTaskManager()
	m.ln = ln
	info("server listening on %s (pid %d)", sp, os.Getpid())

	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigc
		info("shutting down")
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			break
		}
		go m.serve(conn)
	}
	m.stopAll()
	return 0
}

// --- task manager -----------------------------------------------------------

type task struct {
	sb   *sandbox.Sandbox
	info TaskInfo
}

type taskManager struct {
	mu    sync.Mutex
	tasks map[string]*task
	order []string
	ln    net.Listener
}

func newTaskManager() *taskManager {
	return &taskManager{tasks: map[string]*task{}}
}

func newID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func netMode(spec *sandbox.Spec) string {
	if spec.ShareNet {
		return "shared"
	}
	return "isolated"
}

// run starts a detached container task and returns its id.
func (m *taskManager) run(spec *sandbox.Spec) (string, error) {
	if err := resolveRootfs(spec); err != nil {
		return "", err
	}
	// Detached tasks get the tiny init as PID 1: it reaps orphans and turns
	// SIGTERM into a graceful stop for the payload.
	spec.UseInit = true

	id := newID()
	logPath := taskLogPath(id)
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		lf.Close()
		return "", err
	}

	sb, err := sandbox.Start(spec, sandbox.Stdio{Stdin: devnull, Stdout: lf, Stderr: lf})
	lf.Close()
	devnull.Close()
	if err != nil {
		return "", err
	}

	t := &task{sb: sb, info: TaskInfo{
		ID:      id,
		Argv:    append([]string(nil), spec.Argv...),
		Rootfs:  spec.Rootfs,
		Net:     netMode(spec),
		PID:     sb.Pid(),
		State:   "running",
		Started: time.Now().Format(time.RFC3339),
		LogPath: logPath,
	}}

	m.mu.Lock()
	m.tasks[id] = t
	m.order = append(m.order, id)
	m.mu.Unlock()

	go func() {
		code, _ := sb.Wait()
		m.mu.Lock()
		t.info.State = "exited"
		t.info.Exit = code
		m.mu.Unlock()
	}()
	return id, nil
}

func (m *taskManager) get(id string) *task {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks[id]
}

func (m *taskManager) state(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.tasks[id]; t != nil {
		return t.info.State
	}
	return "gone"
}

func (m *taskManager) list() []TaskInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TaskInfo, 0, len(m.order))
	for _, id := range m.order {
		if t := m.tasks[id]; t != nil {
			out = append(out, t.info)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started < out[j].Started })
	return out
}

// stop signals a task; it sends SIGTERM and escalates to SIGKILL after 5s.
func (m *taskManager) stop(id string, force bool) error {
	t := m.get(id)
	if t == nil {
		return fmt.Errorf("no such task: %s", id)
	}
	_ = t.sb.Signal(syscall.SIGTERM)
	if force {
		_ = t.sb.Kill()
		return nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.state(id) != "running" {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = t.sb.Kill()
	return nil
}

func (m *taskManager) remove(id string) error {
	m.mu.Lock()
	t := m.tasks[id]
	if t == nil {
		m.mu.Unlock()
		return fmt.Errorf("no such task: %s", id)
	}
	if t.info.State == "running" {
		m.mu.Unlock()
		return fmt.Errorf("task %s is still running (stop it first)", id)
	}
	delete(m.tasks, id)
	for i, x := range m.order {
		if x == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	path := t.info.LogPath
	m.mu.Unlock()
	_ = os.Remove(path)
	return nil
}

func (m *taskManager) stopAll() {
	m.mu.Lock()
	ids := append([]string(nil), m.order...)
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.stop(id, true)
	}
}

func (m *taskManager) counts() (running, exited int) {
	for _, t := range m.list() {
		if t.State == "running" {
			running++
		} else {
			exited++
		}
	}
	return
}

// --- request handling -------------------------------------------------------

func (m *taskManager) serve(conn net.Conn) {
	defer conn.Close()
	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	enc := json.NewEncoder(conn)

	switch req.Op {
	case opRun:
		if req.Spec == nil {
			enc.Encode(Response{Error: "run: missing spec"})
			return
		}
		id, err := m.run(req.Spec)
		if err != nil {
			enc.Encode(Response{Error: err.Error()})
			return
		}
		enc.Encode(Response{OK: true, ID: id, Path: taskLogPath(id)})

	case opPs:
		enc.Encode(Response{OK: true, Tasks: m.list()})

	case opLogs:
		t := m.get(req.ID)
		if t == nil {
			enc.Encode(Response{Error: "no such task: " + req.ID})
			return
		}
		enc.Encode(Response{OK: true, ID: req.ID, Path: t.info.LogPath, Follow: req.Follow})
		streamLog(conn, t.info.LogPath, m, req.ID, req.Follow)

	case opStop:
		if err := m.stop(req.ID, false); err != nil {
			enc.Encode(Response{Error: err.Error()})
			return
		}
		enc.Encode(Response{OK: true, ID: req.ID, Message: "stopped"})

	case opRm:
		if err := m.remove(req.ID); err != nil {
			enc.Encode(Response{Error: err.Error()})
			return
		}
		enc.Encode(Response{OK: true, ID: req.ID, Message: "removed"})

	case opStatus:
		running, exited := m.counts()
		enc.Encode(Response{OK: true, Message: fmt.Sprintf(
			"server pid %d, socket %s, %d running, %d exited",
			os.Getpid(), socketPath(), running, exited)})

	case opShutdown:
		enc.Encode(Response{OK: true, Message: "shutting down"})
		if m.ln != nil {
			go func() {
				time.Sleep(100 * time.Millisecond)
				_ = m.ln.Close()
			}()
		}

	default:
		enc.Encode(Response{Error: "unknown op: " + req.Op})
	}
}

// streamLog writes the task log to conn. With follow it keeps streaming until
// the task exits; otherwise it sends the current contents and stops.
func streamLog(conn net.Conn, path string, m *taskManager, id string, follow bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	buf := make([]byte, 32*1024)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if _, werr := conn.Write(buf[:n]); werr != nil {
				return
			}
		}
		if rerr == io.EOF {
			if !follow || m.state(id) != "running" {
				return
			}
			time.Sleep(150 * time.Millisecond)
			continue
		}
		if rerr != nil {
			return
		}
	}
}
