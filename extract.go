package main

import (
	"archive/tar"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cmdExtract unpacks a (possibly compressed) tar archive into dest,
// discarding ownership so the tree is owned by the invoking user. That
// is exactly what the single-id user namespace expects.
func cmdExtract(args []string) int {
	var (
		src   string
		dest  string
		strip int
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "--strip":
			i++
			fmt.Sscanf(args[i], "%d", &strip)
		case strings.HasPrefix(a, "-c"):
			fmt.Sscanf(strings.TrimPrefix(a, "-c"), "%d", &strip)
		case src == "":
			src = a
		case dest == "":
			dest = a
		default:
			fatal("extract: unexpected argument %q", a)
		}
	}
	if src == "" || dest == "" {
		fmt.Fprintln(os.Stderr, "usage: goroot extract [-c N] <tarball|url> <dest>")
		return 2
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		fatal("extract: %v", err)
	}

	r, err := openArchive(src)
	if err != nil {
		fatal("extract: %v", err)
	}
	defer r.Close()

	if err := untar(r, dest, src, strip); err != nil {
		fatal("extract: %v", err)
	}
	return 0
}

func openArchive(src string) (io.ReadCloser, error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		c := &http.Client{Timeout: 5 * time.Minute}
		resp, err := c.Get(src)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("GET %s: %s", src, resp.Status)
		}
		return resp.Body, nil
	}
	return os.Open(src)
}

func decompress(src string, r io.Reader) (io.Reader, error) {
	switch {
	case strings.HasSuffix(src, ".gz") || strings.HasSuffix(src, ".tgz"):
		return gzip.NewReader(r)
	case strings.HasSuffix(src, ".bz2"):
		return bzip2.NewReader(r), nil
	default:
		return r, nil
	}
}

func untar(r io.Reader, dest, name string, strip int) error {
	dr, err := decompress(name, r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(dr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		rel := stripComponents(hdr.Name, strip)
		if rel == "" || rel == "." {
			continue
		}
		target := filepath.Join(dest, rel)
		// Guard against path traversal.
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) && target != filepath.Clean(dest) {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFile(tr, target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Remove(target)
			if err := os.Link(filepath.Join(dest, stripComponents(hdr.Linkname, strip)), target); err != nil {
				// Fall back to a copy if the hard link target is unavailable.
				if data, rerr := os.ReadFile(filepath.Join(dest, stripComponents(hdr.Linkname, strip))); rerr == nil {
					_ = os.WriteFile(target, data, os.FileMode(hdr.Mode))
				}
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			// Device nodes cannot be created without privileges; skip.
			continue
		}
	}
	return nil
}

func writeFile(r io.Reader, target string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode&0o777)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return err
	}
	return nil
}

func stripComponents(name string, n int) string {
	name = filepath.Clean(name)
	parts := strings.Split(name, string(os.PathSeparator))
	if n >= len(parts) {
		return ""
	}
	return filepath.Join(parts[n:]...)
}
