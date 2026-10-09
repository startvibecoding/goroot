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

One binary dispatches on `os.Args[1]` in `main.go`:

| arg | role |
| --- | --- |
| `run` | **parent** stage: probe capabilities, open namespaces, re-exec self as `__init` |
| `__init` | **child** stage: runs *inside* the namespaces; sets up mounts, `pivot_root`, execs the target |
| `__probeok` | hidden trivial child used to test whether a namespace can be created |
| `extract` / `pull` | unpack a (possibly compressed) tar rootfs, discarding ownership |
| `doctor` / `check` | capability report |

The parent builds a `Spec` (`spec.go`), serialises it to JSON and passes it to the
child via the **`GOROOT_SPEC`** environment variable. Namespaces are created by
`os/exec` + `syscall.SysProcAttr.Cloneflags`; uid/gid mapping is set via
`UidMappings`/`GidMappings`.

| file | responsibility |
| --- | --- |
| `main.go` | CLI parsing / command dispatch |
| `spec.go` | `Spec` struct (JSON to child), add fields here for new options |
| `run.go` | parent: capability probing, degradation, clone flags, uid maps, tty, exit code |
| `probe_linux.go` | `probeNS`, `doctor`, `GOROOT_DISABLE_NS` test hook |
| `init_linux.go` | child: mounts, `pivot_root`, exec, tiny init (`--init`) |
| `net_linux.go` | bring up `lo`, `isTerminal` |
| `extract.go` | tar/.gz/.bz2 unpacking (local file or http(s) URL) |

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

## Environment notes

- Requires unprivileged user namespaces: `kernel.unprivileged_userns_clone = 1`.
- `goroot doctor` is the fastest way to see what a host supports and what will be
  degraded.
- To exercise the real-root code path as an unprivileged user, run inside a mapped
  user namespace, e.g.
  `unshare --user --map-root-user --mount --pid --fork bash`.
