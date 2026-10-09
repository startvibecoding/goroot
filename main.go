package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/startvibecoding/goroot/sandbox"
)

const version = "0.1.0"

func main() {
	// Init must run before anything else: when this process is the container
	// init (or a capability probe) re-executed by the sandbox package, Init
	// never returns. Otherwise it returns immediately.
	sandbox.Init()

	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	switch args[0] {
	case "run":
		os.Exit(cmdRun(args[1:]))
	case "extract", "pull":
		os.Exit(cmdExtract(args[1:]))
	case "doctor", "check":
		os.Exit(cmdDoctor(args[1:]))
	case "server", "daemon":
		os.Exit(cmdServer(args[1:]))
	case "client":
		os.Exit(cmdClient(args[1:]))
	case "version", "-v", "--version":
		fmt.Printf("goroot %s\n", version)
		os.Exit(0)
	case "help", "-h", "--help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "goroot: unknown command %q\n\n", args[0])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `goroot `+version+` - rootless lightweight container (user namespace based)

Usage:
  goroot run [options] [--] <command> [args...]
  goroot extract [-c N] <tarball|url> <dest>
  goroot server [-d|--daemon]      start the background task daemon
  goroot client <subcommand>       talk to the daemon (run/ps/logs/stop/rm/status/shutdown)
  goroot doctor
  goroot version

run options:
  -r, --root DIR        rootfs directory (default: built-in Alpine minirootfs)
      --hostname NAME   container hostname (default: goroot)
  -b, --bind SRC[:DST][,ro]   bind mount host path into the container (repeatable)
      --ro-bind SRC[:DST]     read-only bind mount (repeatable)
      --tmpfs DST             mount a tmpfs at DST (repeatable)
  -e, --env KEY=VAL     set an environment variable (repeatable)
  -w, --cwd DIR         working directory inside the container (default: /)
      --share-net       share the host network namespace (default)
      --isolate-net     use a private network namespace (only lo)
      --no-resolv       do not auto-bind the host /etc/resolv.conf
      --no-pid          do not create a PID namespace
      --no-ipc          do not create an IPC namespace
      --no-uts          do not create a UTS namespace
  -u, --uid N           uid to run as inside the container (default: 0)
  -g, --gid N           gid to run as inside the container (default: 0)
      --keep-env        keep the host environment
  -h, --help            show this help

Examples:
  goroot run -- /bin/sh                     # use the built-in Alpine rootfs
  goroot run -r myrootfs -- /bin/sh          # use your own rootfs
  goroot extract alpine-3.20.tar.gz rootfs   # unpack a custom rootfs

Notes:
  * Files in the rootfs should be owned by your host user (extract as
    yourself). Inside the container that user maps to root (uid 0).
  * Needs unprivileged user namespaces when run as a normal user
    (kernel.unprivileged_userns_clone=1); namespaces that a given kernel
    cannot provide are dropped automatically with a warning. Run
    'goroot doctor' to see exactly what is supported here.
  * A Go SDK for embedding sandboxes is available as the 'goroot/sandbox'
    package (call sandbox.Init() first in your main).
`)
}

func cmdRun(args []string) int {
	spec := buildRunSpec(args)
	if err := resolveRootfs(spec); err != nil {
		fatal("%v", err)
	}
	return runSpec(spec)
}

// buildRunSpec parses the options shared by `goroot run` and `goroot client run`.
func buildRunSpec(args []string) *sandbox.Spec {
	spec := &sandbox.Spec{
		Rootfs:   "", // empty => use the built-in Alpine rootfs
		Hostname: "goroot",
		Cwd:      "/",
		ShareNet: true, // share the host network by default
	}

	binds := []sandbox.Bind{}
	var cmdArgs []string

	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			cmdArgs = args[i+1:]
			break
		}
		if !strings.HasPrefix(a, "-") {
			cmdArgs = args[i:]
			break
		}

		val := ""
		hasVal := false
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			val, hasVal = a[eq+1:], true
			a = a[:eq]
		}
		next := func() string {
			if hasVal {
				return val
			}
			i++
			if i >= len(args) {
				fatal("run: flag %s needs a value", a)
			}
			return args[i]
		}

		switch a {
		case "-r", "--root":
			spec.Rootfs = next()
		case "--hostname":
			spec.Hostname = next()
		case "-b", "--bind":
			binds = append(binds, parseBind(next(), false))
		case "--ro-bind":
			binds = append(binds, parseBind(next(), true))
		case "--tmpfs":
			spec.Tmpfs = append(spec.Tmpfs, next())
		case "-e", "--env":
			spec.Env = append(spec.Env, next())
		case "-w", "--cwd":
			spec.Cwd = next()
		case "--share-net":
			spec.ShareNet = true
		case "--isolate-net":
			spec.ShareNet = false
		case "--no-resolv", "--no-resolv-conf":
			spec.NoResolv = true
		case "--no-pid":
			spec.NoPID = true
		case "--no-ipc":
			spec.NoIPC = true
		case "--no-uts":
			spec.NoUTS = true
		case "--keep-env":
			spec.KeepEnv = true
		case "--init":
			spec.UseInit = true
		case "-u", "--uid":
			spec.UID = atoiOr(next(), a)
		case "-g", "--gid":
			spec.GID = atoiOr(next(), a)
		case "-h", "--help":
			usage()
			os.Exit(0)
		default:
			fatal("run: unknown flag %q", a)
		}
	}

	spec.Binds = binds
	spec.Argv = cmdArgs
	return spec
}

func parseBind(s string, forceRO bool) sandbox.Bind {
	ro := forceRO
	parts := strings.Split(s, ",")
	specPart := parts[0]
	for _, o := range parts[1:] {
		if o == "ro" {
			ro = true
		}
	}

	fields := strings.Split(specPart, ":")
	if n := len(fields); n >= 2 {
		switch fields[n-1] {
		case "ro":
			ro = true
			fields = fields[:n-1]
		case "rw":
			fields = fields[:n-1]
		}
	}

	src := fields[0]
	dst := ""
	if len(fields) > 1 {
		dst = strings.Join(fields[1:], ":")
	}
	return sandbox.Bind{Src: src, Dst: dst, RO: ro}
}

func atoiOr(s, flag string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		fatal("run: flag %s: invalid number %q", flag, s)
	}
	return n
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "goroot: "+format+"\n", a...)
	os.Exit(1)
}

func info(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "goroot: "+format+"\n", a...)
}

// selfPath returns the path to re-exec for the daemon child.
func selfPath() string {
	if _, err := os.Stat("/proc/self/exe"); err == nil {
		return "/proc/self/exe"
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "goroot"
}
