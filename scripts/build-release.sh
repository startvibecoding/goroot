#!/usr/bin/env bash
# Build cross-compiled release artifacts into dist/.
# Shared by `make dist` and .github/workflows/release.yml.
set -euo pipefail

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
VERSION="${VERSION#v}"
OUT="${OUT:-dist}"
PLATFORMS="${PLATFORMS:-linux/amd64 linux/arm64}"

rm -rf "$OUT"
mkdir -p "$OUT"

echo "building goroot ${VERSION} -> ${OUT}/"

for p in $PLATFORMS; do
  os="${p%%/*}"
  arch="${p##*/}"
  bin="goroot_${VERSION}_${os}_${arch}"

  echo "  ${os}/${arch}: ${bin}"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o "${OUT}/${bin}" .

  # Tarball ships the binary as a plain `goroot` entry. The raw binary
  # (${bin}) is kept alongside it for the npm package to download.
  tar -C "$OUT" -czf "${OUT}/${bin}.tar.gz" \
    --transform "s|^${bin}$|goroot|" "${bin}"
done

( cd "$OUT" && sha256sum goroot_* > checksums.txt )

echo
echo "artifacts in ${OUT}/:"
ls -l "$OUT"
