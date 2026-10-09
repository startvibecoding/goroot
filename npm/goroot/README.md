# goroot (npm)

Installs the [`goroot`](https://github.com/startvibecoding/goroot) CLI — a
rootless, lightweight, single-process container runtime written in Go.

```sh
npm install -g goroot
goroot run -- /bin/sh        # drops into a shell in the built-in Alpine rootfs
```

The postinstall step downloads the matching Linux binary from GitHub Releases.

- **Linux only** (uses user namespaces; `kernel.unprivileged_userns_clone=1`).
- Supported: `linux-x64`, `linux-arm64`.
- Skip the download with `GOROOT_SKIP_DOWNLOAD=1`; point at a mirror with
  `GOROOT_DOWNLOAD_BASE=https://...`.

See the full documentation at
<https://github.com/startvibecoding/goroot>.
