package sandbox

import (
	"archive/tar"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Extract unpacks a (possibly compressed) tar archive into dest, discarding
// ownership so the tree is owned by the invoking user — which is exactly what
// the single-id user namespace expects. The name is used only to pick the
// decompressor by suffix (.gz/.tgz/.bz2).
func Extract(r io.Reader, dest, name string, strip int) error {
	dr, err := decompress(name, r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(dr)
	dest = filepath.Clean(dest)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
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
		if !strings.HasPrefix(target, dest+string(os.PathSeparator)) && target != dest {
			continue // path traversal guard
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
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Remove(target)
			if err := os.Link(filepath.Join(dest, stripComponents(hdr.Linkname, strip)), target); err != nil {
				if data, rerr := os.ReadFile(filepath.Join(dest, stripComponents(hdr.Linkname, strip))); rerr == nil {
					_ = os.WriteFile(target, data, os.FileMode(hdr.Mode))
				}
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			continue // device nodes cannot be created unprivileged
		}
	}
	return nil
}

func decompress(name string, r io.Reader) (io.Reader, error) {
	switch {
	case strings.HasSuffix(name, ".gz") || strings.HasSuffix(name, ".tgz"):
		return gzip.NewReader(r)
	case strings.HasSuffix(name, ".bz2"):
		return bzip2.NewReader(r), nil
	default:
		return r, nil
	}
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
	_, err = io.Copy(f, r)
	return err
}

func stripComponents(name string, n int) string {
	name = filepath.Clean(name)
	parts := strings.Split(name, string(os.PathSeparator))
	if n >= len(parts) {
		return ""
	}
	return filepath.Join(parts[n:]...)
}
