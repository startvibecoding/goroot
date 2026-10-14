package buildkit

import (
	"compress/bzip2"
	"compress/gzip"
	"io"
	"strings"
)

func decompressArchive(name string, r io.Reader) (io.Reader, error) {
	switch {
	case strings.HasSuffix(strings.ToLower(name), ".gz"), strings.HasSuffix(strings.ToLower(name), ".tgz"):
		return gzip.NewReader(r)
	case strings.HasSuffix(strings.ToLower(name), ".bz2"):
		return bzip2.NewReader(r), nil
	default:
		return r, nil
	}
}

func stripComponents(name string, n int) string {
	parts := strings.Split(strings.TrimPrefix(strings.ReplaceAll(name, "\\", "/"), "/"), "/")
	if n >= len(parts) {
		return ""
	}
	return strings.Join(parts[n:], "/")
}
