package buildkit

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ExtractLayer safely unpacks a tar layer into dest. Ownership is discarded so
// the result remains usable by goroot's single-id rootless user namespace.
func ExtractLayer(r io.Reader, name, dest string, strip int) error {
	dr, err := decompress(name, r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(dr)
	dest = filepath.Clean(dest)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		rel := stripComponents(hdr.Name, strip)
		if rel == "" || rel == "." {
			continue
		}
		target, err := safeJoin(dest, rel)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFileFromReader(target, tr, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if _, err := validateLinkTarget(dest, target, hdr.Linkname); err != nil {
				return err
			}
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			source, err := safeJoin(dest, stripComponents(hdr.Linkname, strip))
			if err != nil {
				return err
			}
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Remove(target)
			if err := os.Link(source, target); err != nil {
				data, readErr := os.ReadFile(source)
				if readErr != nil {
					return err
				}
				if writeErr := writeFileFromReader(target, strings.NewReader(string(data)), os.FileMode(hdr.Mode)); writeErr != nil {
					return writeErr
				}
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			// Device nodes cannot be created in a user namespace; skip them.
		}
	}
}

func decompress(name string, r io.Reader) (io.Reader, error) { return decompressArchive(name, r) }

func safeJoin(root, rel string) (string, error) {
	root = filepath.Clean(root)
	cleanRel := filepath.Clean(strings.TrimPrefix(filepath.ToSlash(rel), "/"))
	if cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(os.PathSeparator)) {
		return "", errors.New("tar entry escapes destination")
	}
	p := filepath.Join(root, cleanRel)
	if p != root && !strings.HasPrefix(p, root+string(os.PathSeparator)) {
		return "", errors.New("tar entry escapes destination")
	}
	return p, nil
}

func validateLinkTarget(root, linkPath, linkTarget string) (bool, error) {
	if filepath.IsAbs(linkTarget) {
		return false, nil
	}
	resolved := filepath.Dir(linkPath)
	if resolved == linkPath {
		resolved = root
	}
	full := filepath.Clean(filepath.Join(resolved, linkTarget))
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return false, errors.New("symlink escapes destination")
	}
	return true, nil
}

func writeFileFromReader(target string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	tmp := target + ".goroot-layer-tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode&0o777|0o600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode & 0o777); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}
