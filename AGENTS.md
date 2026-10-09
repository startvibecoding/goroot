# AGENTS.md

Guidance for AI agents (and humans) working on this repository.

## What this is

`goroot` is a **rootless, lightweight, single-process container runtime** written in
Go. It boots a rootfs (e.g. Alpine minirootfs) as an **unprivileged** user by using
Linux **user namespaces** — it is a modern alternative to `proot`: instead of
intercepting syscalls with `ptrace`, it lets the kernel do namespace isolation
natively (faster, and behaves like a real container).

Single static-ish binary, no daemon. One process per container by default.

## Build / test / run

```sh
make build          # or: go build -o goroot .
make fmt vet        # gofmt + go vet, must be clean
make test           # runs scripts/smoke.sh (downloads alpine rootfs if missing)
./goroot doctor     # print kernel capability report for this machine
```

Most work is verified with `./scripts/smoke.sh`. **Add a case there for every new
behavior.** It returns non-zero on any failure.

## Architecture: two-stage re-exec

The container init is a **re-exec of the binary itself** (for the SDK, the
importing binary), selected by an environment marker, not by a subcommand:

| marker | stage |
| --- | --- |
| `GOROOT_INIT=1` | **child**: runs *inside* the namespaces; sets up mounts, `pivot_root`, execs the target |
| `GOROOT_PROBE=1` | trivial child used to test whether a namespace can be created |

The parent (`sandbox.Start`) builds a `Spec` (`sandbox/spec.go`), serialises it to
JSON and passes it via **`GOROOT_SPEC`**. `sandbox.Init()` (called first thing in
`main`) checks the markers and runs the child stage. Namespaces are created by
`os/exec` + `syscall.SysProcAttr.Cloneflags`; uid/gid mapping via
`UidMappings`/`GidMappings`.

Top-level CLI subcommands (`main.go`): `run`, `extract`/`pull`, `doctor`/`check`,
`server`/`daemon`, `client`, `version`, `help`.

## Layout

Three packages:

- **`goroot`** (root, `package main`) — the CLI + daemon.
- **`github.com/startvibecoding/goroot/sandbox`** — the reusable container engine (also the Go SDK).
- **`github.com/startvibecoding/goroot/assets`** — the embedded default rootfs (opt-in; ~3.5 MB).

| file | responsibility |
| --- | --- |
| `main.go` | CLI parsing / dispatch; calls `sandbox.Init()` first; `buildRunSpec` |
| `run.go` | foreground `run`: wires stdio + signal-derived context into `sandbox.Run` |
| `doctor.go` | `doctor` command (uses `sandbox.Detect`) |
| `extract.go` | `extract` command (URL/local fetch) + `sandbox.Extract` |
| `assets.go` | resolve built-in rootfs from `assets` pkg into `~/.cache/goroot` |
| `protocol.go` | daemon wire protocol + `~/.goroot` paths |
| `daemon.go` | `server`: task manager, unix-socket listener, log streaming |
| `client.go` | `client` subcommands |
| `sandbox/spec.go` | exported `Spec`, `Bind`, `Stdio`, JSON to child |
| `sandbox/sandbox.go` | parent side: `Start`/`Run`/`Wait`/`Signal`/`Kill`, degrade, clone flags |
| `sandbox/init_linux.go` | child side: `Init` (re-exec entry), mounts, `pivot_root`, exec, tiny init |
| `sandbox/probe_linux.go` | `probeNS`, `Detect` (`Caps`), `GOROOT_DISABLE_NS` hook |
| `sandbox/net_linux.go` | bring up `lo`, `isTerminal` |
| `sandbox/extract.go` | `Extract` tar/.gz/.bz2 unpacking |
| `assets/assets.go` | `go:embed` of the Alpine tarball |

## Go SDK (`github.com/startvibecoding/goroot/sandbox`)

The `sandbox` package is the public SDK: other programs import it to run
sandboxes. API: `Spec`, `Stdio`, `Start`, `Run`, `Wait`, `Signal`, `Kill`,
`Pid`, `Extract`, `Detect`, and `Init`.

- **`sandbox.Init()` MUST be the first statement of the importing program's
  `main`.** The child stage is a re-exec of *the importing binary* with
  `GOROOT_INIT=1` (init) or `GOROOT_PROBE=1` (capability probe). `Init` inspects
  those env vars and either runs the container init (never returning) or returns
  immediately. See `examples/sdk`.
- The re-exec target is `os.Executable()`. Package-level `init()` funcs of the
  host program therefore run in the child too — keep them side-effect free.
- The SDK never depends on the embedded rootfs; import `github.com/startvibecoding/goroot/assets` only if you
  want it.
- `Stdio` takes any `io.Reader`/`io.Writer` (nil = empty stdin / discarded out).
  TTY is auto-detected for terminal `*os.File`s.
- The module path is `github.com/startvibecoding/goroot`; import packages as
  `github.com/startvibecoding/goroot/{sandbox,assets}`.


## Critical invariants — do not break these

These were each a real bug. Read before touching namespaces/mounts.

1. **Never call `unix.Unshare()` for a namespace from the Go process.** Go is
   multithreaded and the kernel refuses `unshare(CLONE_NEWUSER)` when the address
   space is shared with other threads (`EINVAL`). Always create namespaces via
   `clone` (`SysProcAttr.Cloneflags`) in a child process. This is exactly why
   `probeNS` spawns a `__probeok` child instead of unsharing in-process.

2. **Mount ordering in `setupContainer` is load-bearing:**
   - `mount("", "/", "", MS_REC|MS_PRIVATE, "")` first, or mounts leak to the host.
   - **Bind the rootfs onto itself *before* adding submounts.** `pivot_root` needs
     the new root to be a mount point; if you bind after mounting children, the
     self-bind shadows them.
   - Create `/dev/pts` and `/dev/shm` **after** mounting the tmpfs on `/dev`,
     otherwise the tmpfs hides them.
   - A fresh `proc` needs your own PID namespace; a fresh `sysfs` needs your own
     network namespace. When either is absent, **fall back to binding the host's**
     `/proc` / `/sys`.

3. **TTY: do not `setsid()` + `Setctty` on the inherited terminal.** Claiming an
   already-owned tty as the controlling terminal of a new session fails
   (`ioctl(TIOCSCTTY) = EPERM`). Instead use `Setpgid + Foreground + Ctty=0` so the
   container inherits the tty and takes the foreground process group.

4. **`mknod` is forbidden in a user namespace.** Populate `/dev` by bind-mounting the
   host device nodes, not by creating them.

5. **Degradation must sync the Spec so the child never touches the host.** When a
   namespace is dropped in `run.go`, set the matching `Spec` field:
   - UTS gone → `NoUTS = true` (child must **not** call `sethostname`, or it changes
     the *host* hostname).
   - net gone → `ShareNet = true` (child must **not** bring up `lo`).
   - pid gone → `NoPID = true` (child binds host `/proc`).
   - user ns not used → `NoUser = true` (no id maps; child may use any uid).

6. **Single-id user namespace.** An unprivileged process can only map
   `container 0 ↔ host uid`. So inside, only uid/gid 0 exists — `-u 0x1000` must
   fail with a clear message. The real-root path (`euid == 0`) skips the userns
   (`NoUser=true`) and can use any uid.

7. **Exit codes must propagate**: signal death → `128 + signum`.

## Built-in default rootfs

A minimal Alpine minirootfs is committed under `assets/` and compiled into the
binary with `go:embed` (see `assets.go`). When `run` is called without `-r/--root`,
`ensureEmbeddedRootfs` extracts it (atomically, race-safe) to
`$XDG_CACHE_HOME/goroot/alpine-<ver>` on first use and reuses it afterwards.

- The tarball is **intentionally tracked**; `.gitignore` ignores `*.tar.gz` but
  re-includes `!assets/*.tar.gz`.
- To bump the embedded version: drop a new tarball in `assets/`, update the
  `embeddedTarball` / `embeddedRootDir` constants and the `go:embed` path in
  `assets.go`, and commit the tarball.
- Keep the binary buildable offline: never make the build depend on downloading the
  rootfs.

## Default UX (keep these defaults sane)

`run` is tuned to work with zero flags. Do not regress these:

- **Shared host network by default**: `cmdRun` sets `Spec.ShareNet = true`
  (also `--share-net`); `--isolate-net` opts back into a private netns. Because
  the default shares the host netns, a fresh `sysfs` cannot be mounted and the
  `/sys` fallback (read-only bind of the host) kicks in — expected.
- **Auto `/etc/resolv.conf`**: `addDefaultResolv` (run.go) appends a read-only bind
  of the host resolver unless `--no-resolv` is set or the user already mounted
  something at `/etc/resolv.conf`.
- **Auto shell**: with no command, `pickDefaultShell` (init_linux.go) picks the
  first available of `/bin/sh`, `/bin/bash`, `/bin/zsh`, `/bin/fish` (both `/bin`
  and `/usr/bin`). It runs after `pivot_root`, so it inspects the *container*.

## Daemon (`server` / `client`)

A pair of subcommands for running detached background tasks:

- `goroot server` listens on a unix socket at `~/.goroot/goroot.sock` (dir 0700,
  socket 0600). `-d/--daemon` re-execs a detached copy (Setsid) logging to
  `~/.goroot/server.log`.
- `goroot client <run|ps|logs|stop|rm|status|shutdown>` speaks a
  newline-delimited JSON protocol (`protocol.go`). The client **auto-starts** the
  daemon on connection failure (disable with `GOROOT_NO_AUTOSTART=1`).
- Tasks are launched by the daemon via `buildContainerCmd` with **stdio redirected
  to `~/.goroot/logs/<id>.log`** and no tty, and force `Spec.UseInit = true` so a
  `stop` (SIGTERM) is graceful and orphaned children are reaped. Logs are streamed
  over the socket (`op=logs`, `Follow` follows until the task exits).
- State is **in-memory only**; if the daemon dies its tasks die (Pdeathsig) and the
  list is lost.

Gotchas:
- **Never let the daemon inherit stray fds.** The invoking tool may hold a control
  pipe on an fd > 2; a long-lived daemon that inherits it makes the caller block on
  EOF forever. `startDaemon` calls `setCloexecOnStrayFds()` before `Start` for this
  reason. Keep that.
- When testing by hand, **do not** use `pkill -f 'goroot server'`: the pattern also
  matches the shell running your script and kills it (looks like a hang). Use the
  `[g]oroot` bracket trick or match the real cmdline `/proc/self/exe server`.

## Conventions

- New run options: add a field to `Spec`, parse it in `cmdRun` (`main.go`), and use it
  in the child.
- Syscalls: use `golang.org/x/sys/unix`.
- User-facing warnings: `warn(...)`; fatal errors: `fatal(...)` (exit 1). Warnings go
  to stderr prefixed with `goroot: warning:`.
- Linux-only files carry `//go:build linux`.
- Keep `gofmt` and `go vet` clean; the repo has no separate linter.
- Docs are bilingual: `README.md` is the **English** default, `README_zh.md` is the
  Chinese version. Keep them in sync and preserve the language-switcher line
  under the title (`[English](README.md) | [中文](README_zh.md)`).

## Testing

- `scripts/smoke.sh` is the source of truth (19 checks today). Run it before
  considering work done: `make test`.
- Force degradation paths without an old kernel: `GOROOT_DISABLE_NS=pid,uts,net`
  (comma-separated namespace names) makes `probeNS` report them as unsupported.
- `rootfs/`, the built binary and tarballs are gitignored; the smoke test fetches the
  Alpine minirootfs on demand (needs network).

## CI, releases & npm

- `.github/workflows/ci.yml` — runs on every push (all branches) and PRs:
  `make vet`, `make build`, `./goroot doctor`, then `./scripts/smoke.sh`. It first
  relaxes the runner's userns restriction (`sysctl
  kernel.apparmor_restrict_unprivileged_userns=0`), because Ubuntu 24.04 runners
  block unprivileged user namespaces by default.
- `.github/workflows/release.yml` — triggers on `v*` tags (and manual dispatch):
  builds `dist/` via `make dist` → `scripts/build-release.sh`, uploads artifacts,
  creates a GitHub Release (`softprops/action-gh-release`), then publishes npm.
- `scripts/build-release.sh` cross-compiles `linux/{amd64,arm64}` with
  `-X main.version=<tag>` (so `version` in `main.go` must stay a `var`), emits
  `goroot_<ver>_linux_<arch>` raw binaries + `.tar.gz` + `checksums.txt`.
- `npm/goroot-installer/` is the published package (name `goroot-installer`, but
  the installed bin/command is `goroot`). `postinstall.js` downloads the matching
  raw binary from the release into `vendor/goroot`; `bin/goroot.js` execs it. The
  release job sets the version from the tag before `npm publish`. Publishing needs
  the `NPM_TOKEN` secret; the step no-ops with a warning when it is absent.
- Bump the version by tagging (`git tag v0.1.0 && git push origin v0.1.0`); it is
  injected at build time, do not hand-edit `main.go`'s `version` for releases.

## Environment notes

- Requires unprivileged user namespaces: `kernel.unprivileged_userns_clone = 1`.
- `goroot doctor` is the fastest way to see what a host supports and what will be
  degraded.
- To exercise the real-root code path as an unprivileged user, run inside a mapped
  user namespace, e.g.
  `unshare --user --map-root-user --mount --pid --fork bash`.
