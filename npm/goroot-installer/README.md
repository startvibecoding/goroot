# @startvibecoding/goroot-installer

Installs the [`goroot`](https://github.com/startvibecoding/goroot) CLI — a
rootless, lightweight, single-process container runtime written in Go.

Published to the **GitHub Packages** npm registry (not npmjs.org).

```sh
# One-time: point the scope at GitHub Packages and authenticate.
# Your token needs at least read:packages.
echo "@startvibecoding:registry=https://npm.pkg.github.com" >> ~/.npmrc
echo "//npm.pkg.github.com/:_authToken=$GITHUB_TOKEN"        >> ~/.npmrc

npm install -g @startvibecoding/goroot-installer
goroot run -- /bin/sh        # drops into a shell in the built-in Alpine rootfs
```

The postinstall step downloads the matching Linux binary from GitHub Releases.

- **Linux only** (uses user namespaces; `kernel.unprivileged_userns_clone=1`).
- Supported: `linux-x64`, `linux-arm64`.
- Skip the download with `GOROOT_SKIP_DOWNLOAD=1`; point at a mirror with
  `GOROOT_DOWNLOAD_BASE=https://...`.

See the full documentation at
<https://github.com/startvibecoding/goroot>.
