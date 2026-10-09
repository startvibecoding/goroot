package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// The daemon (`goroot server`) listens on a unix socket under ~/.goroot and
// manages detached container tasks. `goroot client` talks to it with a simple
// newline-delimited JSON request/response protocol.

// gorootHome returns the state directory (~/.goroot), honoring $GOROOT_HOME.
func gorootHome() string {
	if h := os.Getenv("GOROOT_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".goroot")
}

func socketPath() string { return filepath.Join(gorootHome(), "goroot.sock") }
func logsDir() string    { return filepath.Join(gorootHome(), "logs") }
func taskLogPath(id string) string {
	return filepath.Join(logsDir(), id+".log")
}

// Request is what a client sends to the server.
type Request struct {
	Op     string `json:"op"`               // run|ps|logs|stop|rm|status|shutdown
	ID     string `json:"id,omitempty"`     // task id
	Spec   *Spec  `json:"spec,omitempty"`   // for op=run
	Follow bool   `json:"follow,omitempty"` // for op=logs
}

// Response is the server's reply. For op=logs with Follow set, the JSON header
// line is followed by the raw log stream on the same connection.
type Response struct {
	OK      bool       `json:"ok"`
	Error   string     `json:"error,omitempty"`
	ID      string     `json:"id,omitempty"`
	Path    string     `json:"path,omitempty"`
	Message string     `json:"message,omitempty"`
	Tasks   []TaskInfo `json:"tasks,omitempty"`
	Follow  bool       `json:"follow,omitempty"`
}

// TaskInfo describes a background task to clients.
type TaskInfo struct {
	ID      string   `json:"id"`
	Argv    []string `json:"argv"`
	Rootfs  string   `json:"rootfs"`
	Net     string   `json:"net"`
	PID     int      `json:"pid"`
	State   string   `json:"state"` // running|exited
	Exit    int      `json:"exit"`
	Started string   `json:"started"`
	LogPath string   `json:"logPath"`
}

const (
	opRun      = "run"
	opPs       = "ps"
	opLogs     = "logs"
	opStop     = "stop"
	opRm       = "rm"
	opStatus   = "status"
	opShutdown = "shutdown"
)

// dialServer connects to the daemon, optionally starting it first.
func dialServer() (net.Conn, error) {
	conn, err := net.DialTimeout("unix", socketPath(), 2*time.Second)
	if err == nil {
		return conn, nil
	}
	if os.Getenv("GOROOT_NO_AUTOSTART") != "" {
		return nil, err
	}
	if aerr := startDaemon(); aerr != nil {
		return nil, fmt.Errorf("%v (and could not start server: %v)", err, aerr)
	}
	// Wait for the socket to come up.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, derr := net.DialTimeout("unix", socketPath(), time.Second); derr == nil {
			return c, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("server did not come up on %s", socketPath())
}

// call sends a single request and decodes the JSON response header.
func call(c net.Conn, req Request) (*Response, error) {
	resp, _, err := callRaw(c, req)
	return resp, err
}

// callRaw is like call but also returns the reader positioned right after the
// JSON header line, so a following raw stream (op=logs follow) can be read.
func callRaw(c net.Conn, req Request) (*Response, *bufio.Reader, error) {
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return nil, nil, err
	}
	br := bufio.NewReader(c)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return nil, nil, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, nil, err
	}
	if !resp.OK {
		return &resp, br, fmt.Errorf("%s", resp.Error)
	}
	return &resp, br, nil
}
