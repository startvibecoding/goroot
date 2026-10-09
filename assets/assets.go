// Package assets embeds the default Alpine minirootfs shipped with goroot.
//
// Import it only if you want the built-in rootfs — it adds roughly 3.5 MB to the
// importing binary. Importers of github.com/.../sandbox alone stay small.
package assets

import _ "embed"

// Tarball is the embedded Alpine minirootfs (a gzip-compressed tar archive).
//
//go:embed alpine-minirootfs-3.20.0-x86_64.tar.gz
var Tarball []byte

// TarballName is the file name of Tarball, used to pick the decompressor.
const TarballName = "alpine-minirootfs-3.20.0-x86_64.tar.gz"

// RootDir is the cache directory name for the extracted rootfs.
const RootDir = "alpine-3.20.0-x86_64"
