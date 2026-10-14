package buildkit

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Builder materialises a Dockerfile into a goroot local image.
type Builder struct {
	ContextDir string
	OutputDir  string
	Stdout     io.Writer
	Stderr     io.Writer
	Base       func(ref string, cfg *ImageConfig) (string, error)
	Run        func(contextDir, rootfs string, cfg ImageConfig, argv []string, stdout, stderr io.Writer) error
}

// Build parses and executes df. The returned path is OutputDir.
func (b *Builder) Build(df *Dockerfile) (string, error) {
	if b.ContextDir == "" {
		return "", fmt.Errorf("build context is required")
	}
	if b.OutputDir == "" {
		return "", fmt.Errorf("output image directory is required")
	}
	absCtx, err := filepath.Abs(b.ContextDir)
	if err != nil {
		return "", err
	}
	absOut, err := filepath.Abs(b.OutputDir)
	if err != nil {
		return "", err
	}
	b.ContextDir = absCtx
	b.OutputDir = absOut
	steps, vars, err := Plan(df, absCtx)
	if err != nil {
		return "", err
	}
	baseRef, _, err := df.Base()
	cfg := steps[0].Config
	rootSrc, err := b.resolveBase(baseRef, &cfg)
	if err != nil {
		return "", err
	}
	staged, outRoot, err := b.stageOutput(rootSrc)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staged)
	for _, step := range steps[1:] {
		if err := b.apply(outRoot, step, vars); err != nil {
			return "", fmt.Errorf("%s at line %d: %w", strings.ToLower(step.Instruction.Name), step.Instruction.Line, err)
		}
	}
	cfg = steps[len(steps)-1].Config
	if err := WriteConfig(staged, cfg); err != nil {
		return "", err
	}
	if err := b.publish(staged); err != nil {
		return "", err
	}
	return b.OutputDir, nil
}

func (b *Builder) resolveBase(ref string, cfg *ImageConfig) (string, error) {
	if b.Base == nil {
		return "", fmt.Errorf("base resolver is required")
	}
	p, err := b.Base(ref, cfg)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("base rootfs %q is not a directory", p)
	}
	return p, nil
}

// stageOutput copies the base into a temporary image directory. Every build is
// rebuilt from its declared base because this implementation does not yet have
// content-addressed layer caching.
func (b *Builder) stageOutput(rootSrc string) (string, string, error) {
	if rootSrc == b.OutputDir || strings.HasPrefix(rootSrc, b.OutputDir+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("base rootfs cannot be inside output image")
	}
	staged, err := os.MkdirTemp(filepath.Dir(b.OutputDir), filepath.Base(b.OutputDir)+".goroot-build-*")
	if err != nil {
		return "", "", err
	}
	root := filepath.Join(staged, RootfsDir)
	if err := copyTree(rootSrc, root); err != nil {
		os.RemoveAll(staged)
		return "", "", fmt.Errorf("copy base rootfs: %w", err)
	}
	return staged, root, nil
}

// publish atomically replaces the destination image with the completed staging dir.
func (b *Builder) publish(staged string) error {
	backup := b.OutputDir + ".goroot-old-" + hashShort(time.Now().String())
	hasOld := false
	if _, err := os.Lstat(b.OutputDir); err == nil {
		if err := os.Rename(b.OutputDir, backup); err != nil {
			return err
		}
		hasOld = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(staged, b.OutputDir); err != nil {
		if hasOld {
			_ = os.Rename(backup, b.OutputDir)
		}
		return err
	}
	if hasOld {
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("replace image while preserving old copy failed: %w", err)
		}
	}
	return nil
}

func (b *Builder) apply(root string, step Step, vars map[string]string) error {
	instr := step.Instruction
	switch instr.Name {
	case "RUN":
		argv, err := ExecArgs(instr, Expand(vars, instr.RawArgs))
		if err != nil {
			return err
		}
		if b.Run == nil {
			return fmt.Errorf("runner is required")
		}
		logf(b.Stdout, "RUN %s", displayArgs(argv))
		if err := b.Run(b.ContextDir, root, step.Config, argv, b.Stdout, b.Stderr); err != nil {
			return err
		}
	case "COPY", "ADD":
		sources, dest, allAtOnce, err := b.parseCopyArgs(instr, Expand(vars, step.Instruction.RawArgs))
		if err != nil {
			return err
		}
		for _, s := range sources {
			if !strings.Contains(s, "://") {
				if _, err := ValidateCopySource(b.ContextDir, s); err != nil {
					return err
				}
			}
		}
		resolved, err := b.resolveCopySources(sources)
		if err != nil {
			return err
		}
		if err := b.copySources(root, resolved, dest, allAtOnce, instr.Name == "ADD"); err != nil {
			return err
		}
	case "WORKDIR":
		dir := filepath.Join(root, strings.TrimPrefix(step.Config.Cwd, "/"))
		return os.MkdirAll(dir, 0o755)
	default:
		// metadata-only changes are already represented in step.Config.
	}
	return nil
}

// Step combines an instruction with the config visible after it.
type Step struct {
	Instruction Instruction
	Config      ImageConfig
}

// Plan validates instructions and resolves ARG/ENV variables without touching
// the filesystem.
func Plan(df *Dockerfile, contextDir string) ([]Step, map[string]string, error) {
	vars := map[string]string{}
	cfg := ImageConfig{Cwd: "/", Env: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}}
	steps := make([]Step, 0, len(df.Instructions))
	for _, instr := range df.Instructions {
		args := Expand(vars, instr.RawArgs)
		if instr.Name == "ARG" {
			name, value, err := parseArg(args)
			if err != nil {
				return nil, nil, fmt.Errorf("line %d: ARG: %w", instr.Line, err)
			}
			if v, ok := os.LookupEnv(name); ok {
				value = v
			}
			vars[name] = value
			steps = append(steps, Step{Instruction: instr, Config: cloneConfig(cfg)})
			continue
		}
		if err := ApplyConfig(&cfg, instr, vars); err != nil {
			return nil, nil, err
		}
		switch instr.Name {
		case "COPY", "ADD":
			sources, dest, allAtOnce, err := parseStaticCopyArgs(args)
			if err != nil {
				return nil, nil, fmt.Errorf("line %d: %s: %w", instr.Line, instr.Name, err)
			}
			for _, s := range sources {
				if allAtOnce {
					break
				}
				if strings.Contains(s, "://") {
					if instr.Name != "ADD" {
						return nil, nil, fmt.Errorf("line %d: COPY URLs are not supported; use ADD", instr.Line)
					}
					continue
				}
				if _, err := ValidateCopySource(contextDir, s); err != nil {
					return nil, nil, err
				}
			}
			if !allAtOnce && len(sources) > 1 && !strings.HasSuffix(dest, "/") {
				return nil, nil, fmt.Errorf("line %d: destination must be a directory when more than one source is specified", instr.Line)
			}
		case "MAINTAINER":
			if cfg.Labels == nil {
				cfg.Labels = map[string]string{}
			}
			cfg.Labels["maintainer"] = args
		case "EXPOSE", "STOPSIGNAL", "HEALTHCHECK", "SHELL", "ONBUILD":
			// Accepted for compatibility; runtime translation is deliberately scoped later.
		}
		steps = append(steps, Step{Instruction: instr, Config: cloneConfig(cfg)})
	}
	return steps, vars, nil
}

func (b *Builder) resolveCopySources(sources []string) ([]string, error) {
	var out []string
	for _, source := range sources {
		if strings.Contains(source, "://") || (!strings.ContainsAny(source, "*?[") && !strings.HasPrefix(source, "./")) {
			out = append(out, source)
			continue
		}
		abs, err := ValidateCopySource(b.ContextDir, source)
		if err != nil {
			return nil, err
		}
		matches, err := filepath.Glob(abs)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("no source files were specified for %q", source)
		}
		for _, m := range matches {
			rel, err := filepath.Rel(b.ContextDir, m)
			if err != nil {
				return nil, err
			}
			out = append(out, rel)
		}
	}
	return out, nil
}

func (b *Builder) copySources(root string, sources []string, dest string, allAtOnce, allowAdd bool) error {
	target := dest
	if !path.IsAbs(target) {
		return fmt.Errorf("destination must be absolute")
	}
	destPath := filepath.Join(root, strings.TrimPrefix(filepath.Clean(target), "/"))
	copyAsDir := allAtOnce || len(sources) > 1 || strings.HasSuffix(target, "/")
	if !copyAsDir {
		src := sources[0]
		data, name, err := b.readSource(src)
		if err != nil {
			return err
		}
		if isArchiveName(name) {
			if err := os.MkdirAll(destPath, 0o755); err != nil {
				return err
			}
			return extractTar(bytes.NewReader(data), name, destPath, 0)
		}
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return err
		}
		return writeMaybeExecutable(destPath, data, name)
	}
	if err := os.MkdirAll(destPath, 0o755); err != nil {
		return err
	}
	for _, src := range sources {
		data, name, err := b.readSource(src)
		if err != nil {
			return err
		}
		if isArchiveName(name) && allowAdd && !allAtOnce {
			if err := extractTar(bytes.NewReader(data), name, destPath, 0); err != nil {
				return err
			}
			continue
		}
		if err := writeMaybeExecutable(filepath.Join(destPath, filepath.Base(name)), data, name); err != nil {
			return err
		}
	}
	return nil
}

func (b *Builder) readSource(src string) ([]byte, string, error) {
	if strings.Contains(src, "://") {
		if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
			return nil, "", fmt.Errorf("unsupported URL scheme in ADD %q", src)
		}
		client := &http.Client{Timeout: 10 * time.Minute}
		req, err := http.NewRequest(http.MethodGet, src, nil)
		if err != nil {
			return nil, "", err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("GET %s: %s", src, resp.Status)
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, "", err
		}
		name := path.Base(resp.Request.URL.Path)
		if name == "" || name == "." || name == "/" {
			name = "download"
		}
		return data, name, nil
	}
	abs, err := ValidateCopySource(b.ContextDir, src)
	if err != nil {
		return nil, "", err
	}
	st, err := os.Lstat(abs)
	if err != nil {
		return nil, "", err
	}
	if st.IsDir() {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		err := filepath.Walk(abs, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, relErr := filepath.Rel(abs, p)
			if relErr != nil {
				return relErr
			}
			if rel == "." {
				return nil
			}
			hdr, hdrErr := tar.FileInfoHeader(info, "")
			if hdrErr != nil {
				return hdrErr
			}
			hdr.Name = filepath.ToSlash(rel)
			if info.Mode()&os.ModeSymlink != 0 {
				hdr.Linkname, _ = os.Readlink(p)
			}
			if wrErr := tw.WriteHeader(hdr); wrErr != nil {
				return wrErr
			}
			if info.Mode().IsRegular() {
				f, openErr := os.Open(p)
				if openErr != nil {
					return openErr
				}
				defer f.Close()
				if _, cpErr := io.Copy(tw, f); cpErr != nil {
					return cpErr
				}
			}
			return nil
		})
		if err != nil {
			return nil, "", err
		}
		if err := tw.Close(); err != nil {
			return nil, "", err
		}
		if err := zw.Close(); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), abs + ".tar.gz", nil
	}
	data, err := os.ReadFile(abs)
	return data, abs, err
}

func (b *Builder) parseCopyArgs(instr Instruction, args string) ([]string, string, bool, error) {
	return parseCopyArgsWithSource(b.ContextDir, instr, args)
}

// Plan intentionally validates only static syntax; files produced by RUN are not
// required to exist yet.
func parseStaticCopyArgs(args string) ([]string, string, bool, error) {
	fields, err := shellFields(args)
	if err != nil {
		return nil, "", false, err
	}
	for len(fields) > 1 && strings.HasPrefix(fields[0], "--") {
		name, _, hasValue := strings.Cut(fields[0], "=")
		switch name {
		case "--from", "--chown", "--chmod", "--link", "--parents":
			if !hasValue && (name == "--from" || name == "--chown" || name == "--chmod") {
				if len(fields) < 2 {
					return nil, "", false, fmt.Errorf("flag %s needs a value", name)
				}
				fields = fields[2:]
			} else {
				fields = fields[1:]
			}
		case "--files-from":
			if !hasValue && len(fields) < 2 {
				return nil, "", false, fmt.Errorf("%s needs a value", name)
			}
			if !hasValue {
				fields = fields[2:]
			} else {
				fields = fields[1:]
			}
			if len(fields) == 0 {
				return nil, "", false, fmt.Errorf("requires a source and destination")
			}
			return nil, fields[len(fields)-1], true, nil
		default:
			return nil, "", false, fmt.Errorf("unknown flag %q", fields[0])
		}
	}
	if len(fields) < 2 {
		return nil, "", false, fmt.Errorf("requires a source and destination")
	}
	return fields[:len(fields)-1], fields[len(fields)-1], false, nil
}

func parseCopyArgsWithSource(contextDir string, instr Instruction, args string) (sources []string, dest string, allAtOnce bool, err error) {
	fields, err := shellFields(args)
	if err != nil {
		return nil, "", false, err
	}
	for len(fields) > 1 && strings.HasPrefix(fields[0], "--") {
		name, value, hasValue := strings.Cut(fields[0], "=")
		switch name {
		case "--from", "--chown", "--chmod", "--link", "--parents":
			if !hasValue && (name == "--from" || name == "--chown" || name == "--chmod") {
				if len(fields) < 2 {
					return nil, "", false, fmt.Errorf("flag %s needs a value", name)
				}
				fields = fields[2:]
			} else {
				fields = fields[1:]
			}
		case "--files-from":
			allAtOnce = true
			if !hasValue {
				if len(fields) < 2 {
					return nil, "", false, fmt.Errorf("%s needs a value", name)
				}
				value = fields[1]
				fields = fields[2:]
			} else {
				fields = fields[1:]
			}
			absList, verr := ValidateCopySource(contextDir, value)
			if verr != nil {
				return nil, "", false, verr
			}
			listData, _, rerr := readFileAt(absList, instr.Line)
			if rerr != nil {
				return nil, "", false, rerr
			}
			for _, l := range strings.Split(string(listData), "\n") {
				l = strings.TrimSpace(l)
				if l != "" && !strings.HasPrefix(l, "#") {
					sources = append(sources, l)
				}
			}
			dest = fields[len(fields)-1]
			return sources, dest, allAtOnce, nil
		default:
			return nil, "", false, fmt.Errorf("unknown flag %q", fields[0])
		}
	}
	if len(fields) < 2 {
		return nil, "", false, fmt.Errorf("requires a source and destination")
	}
	dest = fields[len(fields)-1]
	sources = fields[:len(fields)-1]
	if len(sources) == 0 {
		return nil, "", false, fmt.Errorf("no source files")
	}
	return sources, dest, allAtOnce, nil
}

func readFileAt(abs string, line int) ([]byte, string, error) {
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, "", fmt.Errorf("line %d: %w", line, err)
	}
	return b, abs, nil
}

func parseArg(s string) (name, value string, err error) {
	name, value, _ = strings.Cut(strings.TrimSpace(s), "=")
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, " \t=\"'") {
		return "", "", fmt.Errorf("invalid argument name %q", s)
	}
	return name, strings.TrimSpace(value), nil
}

func displayArgs(argv []string) string {
	if len(argv) == 2 && argv[0] == "/bin/sh" && argv[1] == "-c" {
		return argv[2]
	}
	return strings.Join(argv, " ")
}

func logf(w io.Writer, format string, a ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, "goroot build: "+format+"\n", a...)
}

func cloneConfig(c ImageConfig) ImageConfig {
	out := c
	out.Env = append([]string(nil), c.Env...)
	out.Entrypoint = append([]string(nil), c.Entrypoint...)
	out.Cmd = append([]string(nil), c.Cmd...)
	out.Volumes = append([]string(nil), c.Volumes...)
	if c.Labels != nil {
		out.Labels = map[string]string{}
		for k, v := range c.Labels {
			out.Labels[k] = v
		}
	}
	return out
}

func isArchiveName(n string) bool {
	l := strings.ToLower(n)
	return strings.HasSuffix(l, ".tar") || strings.HasSuffix(l, ".tar.gz") || strings.HasSuffix(l, ".tgz") || strings.HasSuffix(l, ".tar.bz2")
}

func extractTar(r io.Reader, name, dest string, strip int) error {
	return ExtractLayer(r, name, dest, strip)
}

func writeMaybeExecutable(dest string, data []byte, src string) error {
	mode := os.FileMode(0o644)
	if st, err := os.Stat(src); err == nil && st.Mode()&0o111 != 0 {
		mode = 0o755
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".goroot-tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

func copyTree(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp-" + hashShort(time.Now().String())
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	if err := walkCopy(src, tmp); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	return nil
}

func walkCopy(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Remove(target)
			return os.Symlink(link, target)
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode().IsRegular():
			return copyFile(p, target, info.Mode().Perm())
		default:
			return nil
		}
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm|0o200)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func hashShort(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
