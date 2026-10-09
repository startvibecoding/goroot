//go:build !linux

package sandbox

import (
	"context"
	"errors"
	"os"
	"syscall"
)

// Caps is empty on non-Linux platforms.
type Caps struct {
	RealRoot, User, Mount, Pid, Uts, Ipc, Net bool
}

// Sandbox is a stub on non-Linux platforms.
type Sandbox struct{}

// Detect is a stub on non-Linux platforms.
func Detect() Caps { return Caps{} }

var errNotLinux = errors.New("sandbox: only supported on Linux")

// Start is a stub on non-Linux platforms.
func Start(*Spec, Stdio) (*Sandbox, error) { return nil, errNotLinux }

// Run is a stub on non-Linux platforms.
func Run(context.Context, *Spec, Stdio) (int, error) { return -1, errNotLinux }

// Wait is a stub on non-Linux platforms.
func (s *Sandbox) Wait() (int, error) { return -1, errNotLinux }

// Pid is a stub on non-Linux platforms.
func (s *Sandbox) Pid() int { return 0 }

// Signal is a stub on non-Linux platforms.
func (s *Sandbox) Signal(syscall.Signal) error { return errNotLinux }

// Kill is a stub on non-Linux platforms.
func (s *Sandbox) Kill() error { return errNotLinux }

// Process is a stub on non-Linux platforms.
func (s *Sandbox) Process() *os.Process { return nil }

// Init is a no-op on non-Linux platforms.
func Init() {}
