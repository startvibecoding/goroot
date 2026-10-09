# goroot

**English** | [中文](README_zh.md)

`goroot` is a **rootless, lightweight, single-process container runtime** written in
Go. It boots a rootfs (such as an Alpine minirootfs) directly, with no root
privileges required. It is meant as a modern alternative to
[proot](https://proot-me.github.io/): instead of intercepting syscalls with
`ptrace`, it uses the Linux **user namespace** to acquire a "fake root" as an
ordinary user — which is faster and more robust than proot.

```
$ go build -o goroot .
$ ./goroot run -r rootfs -- /bin/sh
/ # id
uid=0(root) gid=0(root) groups=65534(nobody),0(root)
```

## Features

- **Works out of the box** — an Alpine minirootfs is embedded in the binary and
  used when no `--root` is given.
- **Sane defaults** — shares the host network, auto-binds the host
  `/etc/resolv.conf` read-only and auto-selects a shell, so `goroot run` just works
  (and `apk`/`curl` work immediately).
- **No root required** — an unprivileged user namespace maps your user to root
  inside the container.
- **Real isolation** — user / mount / pid / uts / ipc / net namespaces (each can be
  turned off individually).
- **Background tasks** — a `server`/`client` daemon pair (unix socket under
  `~/.goroot`) runs detached containers and can stream their logs.
- **Go SDK** — import the `github.com/startvibecoding/goroot/sandbox` package to
  run sandboxes from your own
  Go program (see [Go SDK](#go-sdk)).
- **Single process** — by default the target command is `exec`'d as PID 1 of the
  container; there is no per-container supervisor.
- **Complete mount view** — `/proc`, read-only `/sys`, `/dev` (device nodes,
  `/dev/pts`, `/dev/shm`) and `/tmp`.
- **pivot_root** to switch the root filesystem (falls back to `chroot`).
- **Bind mounts** (read-only or read-write), extra tmpfs, environment variables and
  working directory.
- **Optional tiny init** (`--init`): a PID 1 that reaps zombies and forwards
  signals, suited to running services.
- Built-in **rootfs unpacking** (local file or `http(s)://` URL; `.tar`, `.tar.gz`,
  `.tar.bz2`).
- Interactive terminal: automatically takes over the foreground process group, so
  Ctrl-C and job control work.
- **Kernel capability self-check** (`goroot doctor`) and **graceful degradation**:
  on an older kernel or a restricted host, missing namespaces are dropped with a
  warning instead of crashing.

## Requirements

- Linux kernel ≥ 3.8 (user namespaces), with
  `/proc/sys/kernel/unprivileged_userns_clone = 1`;
- a private network namespace must be creatable to mount `sysfs` (goroot creates one
  by default);
- Go ≥ 1.21.

Missing capabilities are detected and degraded automatically (see
[Compatibility & graceful degradation](#compatibility--graceful-degradation)).
Start with a self-check:

```sh
goroot doctor
```

## Build

```sh
make build          # produces ./goroot
make install        # installs to /usr/local/bin
```

The built-in rootfs lives in `assets/` (`alpine-minirootfs-3.20.0-x86_64.tar.gz`)
and is compiled into the binary with `go:embed`. To refresh it, drop a new tarball
in `assets/` and update the constants in `assets.go`.

## Quick start

A minimal Alpine rootfs is embedded in the binary, so you can start immediately —
with no arguments you drop straight into a shell:

```sh
./goroot run -- /bin/sh        # or just: ./goroot run
```

By default the container shares the host network and the host resolver is
available, so this works out of the box:

```sh
./goroot run -- /bin/sh -c 'apk add --no-cache curl && curl -s https://example.com | head -1'
```

On first use the built-in rootfs is unpacked to
`~/.cache/goroot/alpine-3.20.0-x86_64` and reused from then on. To use your own
rootfs instead, pass `-r DIR`:

```sh
# unpack a rootfs you downloaded (no root; files end up owned by you)
./goroot extract \
  https://dl-cdn.alpinelinux.org/alpine/v3.21/releases/x86_64/alpine-minirootfs-3.21.3-x86_64.tar.gz \
  rootfs

./goroot run -r rootfs -- /bin/sh
```

### Networking

By default the container **shares the host network** and the host's
`/etc/resolv.conf` is bind-mounted read-only, so package managers and `curl` work
with no extra flags:

```sh
./goroot run -r rootfs -- /bin/sh -c 'apk add --no-cache curl && curl -s https://example.com | head -1'
```

For a private network namespace (only `lo`), pass `--isolate-net`. Use
`--no-resolv` to stop binding the host resolver, and `--share-net` (redundant with
the default) to force sharing.

## Usage

```
goroot run [options] [--] <command> [args...]
goroot extract [-c N] <tarball|url> <dest>
goroot server [-d|--daemon]
goroot client <subcommand>       # run | ps | logs | stop | rm | status | shutdown
goroot doctor
goroot version
```

`run` options:

| Option | Description |
| --- | --- |
| `-r, --root DIR` | rootfs directory (default: built-in Alpine minirootfs) |
| `--hostname NAME` | container hostname (default `goroot`) |
| `-b, --bind SRC[:DST][,ro]` | bind-mount a host path (repeatable) |
| `--ro-bind SRC[:DST]` | read-only bind mount (repeatable) |
| `--tmpfs DST` | mount a tmpfs at DST (repeatable) |
| `-e, --env KEY=VAL` | set an environment variable (repeatable) |
| `-w, --cwd DIR` | working directory inside the container (default `/`) |
| `--share-net` | share the host network namespace (default) |
| `--isolate-net` | use a private network namespace (only `lo`) |
| `--no-resolv` | do not auto-bind the host `/etc/resolv.conf` |
| `--no-pid` / `--no-ipc` / `--no-uts` | do not create that namespace |
| `-u, --uid N` / `-g, --gid N` | run as that identity inside (see limitations) |
| `--memory SIZE` | hard memory ceiling (e.g. `512m`, `1g`) |
| `--memory-high SIZE` | soft memory ceiling that only throttles reclaim (cgroup only) |
| `--cpus N` | CPU bandwidth in cores (e.g. `0.5`) |
| `--cpu-time N` | kill after N seconds of CPU time (`RLIMIT_CPU`) |
| `--pids N` | max number of tasks/threads |
| `--no-cgroup` | force the rlimit fallback |
| `--init` | run a tiny init as PID 1 (reaps zombies, forwards signals) |
| `--keep-env` | keep the host environment variables |

`extract` options: `-c N` / `--strip N` removes N leading path components.

Defaults: `--root` = built-in Alpine minirootfs, shared host network, host
`/etc/resolv.conf` bound read-only, and a shell auto-selected from
`/bin/sh`, `/bin/bash`, `/bin/zsh`, `/bin/fish` (in that order).

## Resource limits

Limits are enforced with **cgroup v2** when the host delegates a writable cgroup
subtree to the user (systemd's `user@.service` does this by default via
`Delegate=`) — the precise path: `memory.max`, `memory.high`, `cpu.max`,
`pids.max`, with the container started inside its own cgroup via
`CLONE_INTO_CGROUP` so the whole process tree is contained from birth.

When no delegated cgroup is available (e.g. inside many CI containers), the
runtime degrades to **rlimits** and warns: `--memory` becomes `RLIMIT_AS` and
`--cpu-time` is `RLIMIT_CPU`. `--cpus` (a bandwidth quota) and `--pids` cannot
be expressed as rlimits and are skipped with a warning; `--memory-high` is
cgroup-only. Pass `--no-cgroup` to force the rlimit path. `goroot doctor`
reports which path this host uses.

## How it works

The parent process creates the namespaces in one `clone(2)` (via Go's
`SysProcAttr`) and maps the current user/group to uid/gid 0 inside the container:

```
uid_map: 0 -> <your host uid>     (size 1)
gid_map: 0 -> <your host gid>     (size 1)
```

It then `fork/exec`s itself, entering the hidden `__init` stage, which inside the
namespaces:

1. `mount --make-rprivate /` so mount events never leak back to the host;
2. bind-mounts the rootfs onto itself to give `pivot_root` a mount point;
3. mounts `/proc`, read-only `sysfs`, `/dev` (tmpfs + device nodes +
   devpts/`/dev/shm`) and `/tmp`;
4. `pivot_root`s into the new root and unmounts the old one;
5. sets the hostname, brings up `lo`, applies bind/tmpfs mounts, switches
   uid/gid, `chdir`s;
6. finally `execve`s the target command (or enters the tiny init).

proot normally does all of this by intercepting every syscall with ptrace and
rewriting paths. goroot lets the kernel do it natively, so there is no
interpreter overhead and behaviour is much closer to a real container.

## Background tasks (daemon)

`goroot server` runs a small daemon (listening on `~/.goroot/goroot.sock`) that can
launch **detached** containers; `goroot client` talks to it. The daemon is started
automatically the first time a client needs it (set `GOROOT_NO_AUTOSTART=1` to
forbid that).

```sh
goroot server -d                          # start the daemon in the background
goroot client status                      # is it up?

id=$(goroot client run -- /bin/sh -c 'while :; do date; sleep 5; done')
goroot client ps                          # list tasks
goroot client logs -f $id                 # follow the task's logs
goroot client stop $id                    # SIGTERM, escalating to SIGKILL
goroot client rm $id                      # drop a finished task
goroot client shutdown                    # stop the daemon and its tasks
```

`client run` accepts the same options as `run`. Detached tasks are launched with
the tiny init as PID 1, so `stop` delivers a graceful SIGTERM (exit code 143) and
orphaned children are reaped. Task state is in-memory: if the daemon stops, its
## Go SDK

The container engine is a reusable package, `github.com/startvibecoding/goroot/sandbox`, so other Go programs
can embed container sandboxing:

```go
package main

import (
	"context"
	"os"

	"github.com/startvibecoding/goroot/sandbox"
)

func main() {
	sandbox.Init() // MUST be the first statement of main (see below)

	code, err := sandbox.Run(context.Background(), &sandbox.Spec{
		Rootfs:   "/path/to/rootfs",
		Hostname: "demo",
		Argv:     []string{"/bin/sh", "-c", "id; echo hi"},
	}, sandbox.Stdio{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
	if err != nil {
		panic(err)
	}
	os.Exit(code)
}
```

API: `Spec`, `Stdio`, `Start` (returns a `*Sandbox`), `Run`, `Sandbox.Wait`,
`Sandbox.Signal`, `Sandbox.Kill`, `Sandbox.Pid`, `Extract`, `Detect`.

- **`sandbox.Init()` must be the very first statement of your `main`.** The
  container init runs as a re-exec of *your* binary with `GOROOT_INIT=1` set;
  `Init` detects that and runs the init stage (it never returns in that case, and
  returns immediately otherwise). Because your binary is re-executed, keep your
  package-level `init()` functions free of side effects.
- `Stdio` accepts any `io.Reader`/`io.Writer`, so you can capture output into a
  buffer or a pipe. TTY is auto-detected for terminal `*os.File`s.
- The embedded Alpine rootfs lives in a separate package, `github.com/startvibecoding/goroot/assets` (opt-in,
  ~3.5 MB); combined with `sandbox.Extract` you can unpack it as your default
  rootfs. See `examples/sdk`.

Build the example: `go build -o /tmp/goroot-sdk ./examples/sdk && /tmp/goroot-sdk`.

tasks are killed and the list is lost. Logs live in `~/.goroot/logs/<id>.log`.

## Compatibility & graceful degradation

`goroot` does not assume every namespace exists. Before launching it **probes**
each capability with `clone(2)` (note: it cannot call `unshare()` directly from the
Go process — Go is multithreaded and the kernel refuses it; it must probe via
`fork+exec`, exactly like the real container does), then degrades item by item in
priority order, warning on stderr rather than failing outright.

```sh
goroot doctor   # print what this host supports and what will be degraded
```

```
kernel          : 7.0.10-1-liquorix-amd64 (x86_64)
euid            : 1000 (unprivileged)
namespaces      :
  user   ok
  mount  ok
  pid    ok
  uts    ok
  ipc    ok
  net    ok
verdict         :
  can run: full isolation
```

| Capability | Minimum kernel | Behaviour when missing |
| --- | --- | --- |
| user namespace | 3.8 (and not disabled by the distro) | unprivileged: error out with guidance; root: skip it, run as a full real-root container |
| mount ns + pivot_root | 2.6.x | required (no container without it) |
| pid namespace | 2.6.24 | dropped; bind the host `/proc` |
| uts / ipc / net ns | 2.6.19 / 3.0 / 2.6.24 | dropped one by one with a warning (never touches the host hostname / host `lo`) |
| mounting procfs in userns | 3.8 | fall back to binding the host `/proc` |
| mounting sysfs in userns | 3.8–4.x | fall back to a read-only bind of the host `/sys`; under `--share-net` only the host `/sys` can be bound |
| devpts `newinstance` | 4.7 | fall back to binding the host `/dev/pts` |
| creating regular files in userns/tmpfs | 3.8 | required (`/dev` device nodes are bind-mounted, never `mknod`ed) |

Key safety point: when a namespace probe fails and it is dropped, the flags passed
to the child are updated in lockstep so that no side effect lands on the host — for
example, if UTS is unavailable goroot **does not** call `sethostname` (which would
change the *host* hostname), and if the network namespace is unavailable it **does
not** bring up the host `lo`.

If the kernel is too old, with neither user namespaces nor root, real isolation is
simply impossible — this is exactly the niche proot fills (faking it in userspace
with ptrace). goroot then tells you to enable userns or use proot/bwrap, instead of
surfacing an obscure `EPERM`.

Debugging / forcing degradation: set `GOROOT_DISABLE_NS=pid,uts,net`
(comma-separated) to make goroot pretend those namespaces are unavailable, which is
handy for exercising the degradation paths.

## Limitations & known issues

- **Single-id mapping**: an unprivileged user namespace can only map
  `current user ↔ container root` one-to-one. Therefore only uid/gid 0 exists
  inside; passing a non-zero `-u`/`--gid` is an error. Multi-user support would
  need `/etc/subuid` + `newuidmap` (not implemented yet).
- **Supplementary groups**: because `setgroups` is denied inside a user namespace,
  the host's supplementary groups show up as `nobody(65534)` (cosmetic only; file
  permissions are unaffected).
- **`/sys` under `--share-net`**: sysfs is tied to the network namespace, so when
  sharing the host netns only the host `/sys` can be bind-mounted (read-only),
  bringing along the host's submounts.
- **No seccomp filtering yet**: CPU, memory and pids limits are supported (see
  *Resource limits*), but syscall filtering is not.
- Files in the rootfs are best owned by your user (unpacking with `goroot extract`
  does this); otherwise files that aren't yours appear as `nobody` and may be
  unreadable inside.

## Development

```sh
make fmt vet        # format and static checks
make test           # smoke test (prepares an alpine rootfs; needs network)
ROOTFS=./myrootfs ./scripts/smoke.sh   # reuse an existing rootfs
```

### CI & releasing

`.github/workflows/ci.yml` builds and runs the smoke test on every push and pull
request.

`.github/workflows/release.yml` triggers on **`v*` tags**: it cross-compiles
Linux binaries, creates the GitHub Release and publishes the npm package to the
**GitHub Packages** registry.

```sh
git tag v0.1.0 && git push origin v0.1.0
```

Artifacts (also buildable locally with `make dist`):
`goroot_<ver>_linux_{amd64,arm64}.tar.gz`, matching raw binaries (downloaded by
the npm package on install) and `checksums.txt`.

```sh
# GitHub Packages requires a token even to install; export GITHUB_TOKEN first.
echo "@startvibecoding:registry=https://npm.pkg.github.com" >> ~/.npmrc
echo "//npm.pkg.github.com/:_authToken=$GITHUB_TOKEN"        >> ~/.npmrc
npm install -g @startvibecoding/goroot-installer   # installs the `goroot` command
```

Publishing uses the built-in `GITHUB_TOKEN` (`packages: write`) — no extra secret,
and it skips a version that is already published.

Code layout:

Three packages: the root `github.com/startvibecoding/goroot` (CLI + daemon),
`.../sandbox` (engine + SDK), `.../assets` (embedded rootfs).

| File | Responsibility |
| --- | --- |
| `main.go` | CLI parsing and dispatch; `sandbox.Init()`, `buildRunSpec` |
| `run.go` | foreground `run` (wires stdio + signals into `sandbox.Run`) |
| `doctor.go` | `doctor` self-check (uses `sandbox.Detect`) |
| `extract.go` | `extract` command (URL/local fetch) |
| `assets.go` | resolve the built-in rootfs into `~/.cache/goroot` |
| `protocol.go` | daemon wire protocol + `~/.goroot` path helpers |
| `daemon.go` | `server`: task manager, unix-socket listener, log streaming |
| `client.go` | `client`: run/ps/logs/stop/rm/status/shutdown |
| `sandbox/*.go` | the engine / SDK (Spec, Start/Run/Wait/Signal, Init, Extract, Detect) |
| `assets/assets.go` | `go:embed` of the Alpine tarball |
