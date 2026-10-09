package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/startvibecoding/goroot/sandbox"
)

// runSpec runs one container in the foreground, attached to the current
// terminal, and returns its exit code.
func runSpec(spec *sandbox.Spec) int {
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()

	code, err := sandbox.Run(ctx, spec, sandbox.Stdio{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if err != nil {
		fatal("%v", err)
	}
	return code
}

func warn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "goroot: warning: "+format+"\n", a...)
}
