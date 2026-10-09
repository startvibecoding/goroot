// Command sdk is a tiny example of embedding goroot's sandbox package.
//
//	go build -o /tmp/goroot-sdk ./examples/sdk
//	/tmp/goroot-sdk                                   # uses the embedded rootfs
//	ROOTFS=/path/to/rootfs /tmp/goroot-sdk            # uses your own rootfs
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/startvibecoding/goroot/assets"
	"github.com/startvibecoding/goroot/sandbox"
)

func main() {
	// Required: handles the container-init re-exec. Returns immediately in a
	// normal run; never returns in the child.
	sandbox.Init()

	rootfs := os.Getenv("ROOTFS")
	if rootfs == "" {
		rootfs = extractEmbedded()
	}

	// Example 1: run a command inside the sandbox and capture its output.
	var out bytes.Buffer
	code, err := sandbox.Run(context.Background(), &sandbox.Spec{
		Rootfs:   rootfs,
		Hostname: "demo",
		Argv:     []string{"/bin/sh", "-c", "echo hello from $(hostname); id -u; cat /etc/os-release | head -1; exit 7"},
	}, sandbox.Stdio{Stdout: &out, Stderr: &out})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox error:", err)
		os.Exit(1)
	}
	fmt.Printf("--- captured output (exit code %d) ---\n%s", code, out.String())

	// Example 2: start detached and signal it.
	sb, err := sandbox.Start(&sandbox.Spec{
		Rootfs:  rootfs,
		Argv:    []string{"/bin/sh", "-c", "echo started; sleep 30"},
		UseInit: true,
	}, sandbox.Stdio{Stdout: os.Stdout})
	if err != nil {
		fmt.Fprintln(os.Stderr, "start error:", err)
		os.Exit(1)
	}
	fmt.Println("started sandbox pid", sb.Pid())
	_ = sb.Kill()
	code, _ = sb.Wait()
	fmt.Println("killed, exit code", code)
}

// extractEmbedded unpacks the built-in Alpine minirootfs to a cache directory.
func extractEmbedded() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "goroot", assets.RootDir)
	if _, err := os.Stat(filepath.Join(dir, ".goroot-ok")); err == nil {
		return dir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}
	if err := sandbox.Extract(bytes.NewReader(assets.Tarball), dir, assets.TarballName, 0); err != nil {
		panic(err)
	}
	_ = os.WriteFile(filepath.Join(dir, ".goroot-ok"), []byte("1\n"), 0o644)
	fmt.Fprintln(os.Stderr, "sdk: extracted embedded rootfs to", dir)
	return dir
}
