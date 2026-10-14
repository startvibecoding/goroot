package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/startvibecoding/goroot/buildkit"
	"github.com/startvibecoding/goroot/sandbox"
)

func cmdBuild(args []string) int {
	var (
		file     string
		context  string
		image    string
		showHelp bool
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-f" || a == "--file":
			i++
			if i >= len(args) {
				fatal("build: flag %s needs a value", a)
			}
			file = args[i]
		case strings.HasPrefix(a, "--file="):
			file = strings.TrimPrefix(a, "--file=")
		case a == "-t" || a == "--tag" || a == "--image":
			i++
			if i >= len(args) {
				fatal("build: flag %s needs a value", a)
			}
			image = args[i]
		case strings.HasPrefix(a, "--tag="):
			image = strings.TrimPrefix(a, "--tag=")
		case a == "-h" || a == "--help":
			showHelp = true
		case context == "":
			context = a
		default:
			fatal("build: unexpected argument %q", a)
		}
	}
	if showHelp {
		fmt.Fprint(os.Stderr, `Usage: goroot build [-f Dockerfile] [context]

Build a local goroot image from a compatible Dockerfile. The output is written
to ~/.goroot/images/<name> (or the directory named by --tag/--image).

Options:
  -f, --file FILE    Dockerfile (default: <context>/Dockerfile)
  -t, --tag NAME     image name or output directory
  -h, --help         show this help
`)
		return 0
	}
	if context == "" {
		context = "."
	}
	if file == "" {
		file = filepath.Join(context, "Dockerfile")
	}
	df, err := buildkit.ParseFile(file)
	if err != nil {
		fatal("build: %v", err)
	}
	output, err := buildOutputDir(image)
	if err != nil {
		fatal("build: %v", err)
	}
	b := &buildkit.Builder{
		ContextDir: context,
		OutputDir:  output,
		Stdout:     os.Stderr,
		Stderr:     os.Stderr,
		Base:       resolveBuildContext,
		Run:        runBuildStep,
	}
	done, err := b.Build(df)
	if err != nil {
		fatal("build: %v", err)
	}
	info("built image %s", done)
	return 0
}

func buildOutputDir(image string) (string, error) {
	if image != "" && (strings.ContainsRune(image, os.PathSeparator) || strings.HasPrefix(image, ".")) {
		return filepath.Abs(image)
	}
	base := gorootHome()
	name := image
	if name == "" {
		name = "latest"
	}
	name = strings.NewReplacer("/", "_", ":", "_").Replace(name)
	return filepath.Join(base, "images", name), nil
}

func resolveBuildContext(ref string, cfg *buildkit.ImageConfig) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("FROM requires a base image")
	}
	if strings.EqualFold(ref, "scratch") {
		dir, err := tempScratchRootfs()
		if err != nil {
			return "", err
		}
		*cfg = buildkit.ImageConfig{Cwd: "/"}
		return dir, nil
	}
	if strings.ContainsRune(ref, os.PathSeparator) || strings.HasPrefix(ref, ".") {
		abs, err := filepath.Abs(ref)
		if err != nil {
			return "", err
		}
		if c, err := buildkit.ReadConfig(abs); err == nil {
			*cfg = mergeBaseConfig(c)
			return filepath.Join(abs, buildkit.RootfsDir), nil
		}
		*cfg = buildkit.ImageConfig{Cwd: "/"}
		return abs, nil
	}
	home := gorootHome()
	p := filepath.Join(home, "images", strings.NewReplacer("/", "_", ":", "_").Replace(ref))
	if c, cerr := buildkit.ReadConfig(p); cerr == nil {
		*cfg = mergeBaseConfig(c)
		return filepath.Join(p, buildkit.RootfsDir), nil
	}
	// Fall back to a built-in/rootfs shorthand for common Alpine tags. Registry
	// pulls remain intentionally out of scope.
	if strings.HasPrefix(strings.ToLower(ref), "alpine:") || strings.EqualFold(ref, "alpine") {
		dir, derr := ensureEmbeddedRootfs()
		if derr != nil {
			return "", fmt.Errorf("base image %q is not available locally: registry pulls are not implemented; use goroot extract or a local rootfs/image path", ref)
		}
		*cfg = buildkit.ImageConfig{Cwd: "/", Env: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}}
		return dir, nil
	}
	return "", fmt.Errorf("base image %q is not available locally: registry pulls are not implemented; use goroot extract or a local rootfs/image path", ref)
}

func mergeBaseConfig(in buildkit.ImageConfig) buildkit.ImageConfig {
	out := in
	if out.Cwd == "" {
		out.Cwd = "/"
	}
	if len(out.Env) == 0 {
		out.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}
	}
	return out
}

func tempScratchRootfs() (string, error) {
	dir, err := os.MkdirTemp("", "goroot-scratch-*")
	if err != nil {
		return "", err
	}
	for _, d := range []string{"bin", "dev", "etc", "proc", "sys", "tmp", "usr/bin", "var"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
	}
	return dir, nil
}

func runBuildStep(contextDir, rootfs string, cfg buildkit.ImageConfig, argv []string, stdout, stderr io.Writer) error {
	spec := &sandbox.Spec{
		Rootfs:   rootfs,
		Argv:     argv,
		Cwd:      cfg.Cwd,
		Env:      append([]string(nil), cfg.Env...),
		Hostname: "goroot-build",
		ShareNet: true,
		UseInit:  true,
		Binds:    []sandbox.Bind{{Src: contextDir, Dst: "/build", RO: true}},
	}
	if err := os.MkdirAll(contextDir, 0o755); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()
	code, err := sandbox.Run(ctx, spec, sandbox.Stdio{Stdin: bytes.NewReader(nil), Stdout: stdout, Stderr: stderr})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("command exited with status %d", code)
	}
	return nil
}
