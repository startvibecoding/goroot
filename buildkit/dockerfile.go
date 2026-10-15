// Package buildkit implements the Dockerfile parser and local image metadata
// used by goroot build.
package buildkit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Directive is a parser directive appearing before comments/instructions.
type Directive struct {
	Name  string
	Value string
}

// Instruction is one logical Dockerfile instruction.
type Instruction struct {
	Name       string
	RawArgs    string
	JSONArgs   []string
	Line       int
	HasJSONArg bool
}

// Dockerfile is a parsed, single-stage Dockerfile.
type Dockerfile struct {
	Directives   []Directive
	Instructions []Instruction
	Escape       byte
}

// ImageConfig is the portable runtime configuration stored with an image.
type ImageConfig struct {
	Env        []string          `json:"env,omitempty"`
	User       string            `json:"user,omitempty"`
	Cwd        string            `json:"cwd,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Cmd        []string          `json:"cmd,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Volumes    []string          `json:"volumes,omitempty"`
}

const (
	// ImageConfigFile is the metadata filename inside a goroot image directory.
	ImageConfigFile = "goroot-image.json"
	// RootfsDir is the filesystem directory inside a goroot image directory.
	RootfsDir = "rootfs"
)

// Parse reads a Dockerfile. It intentionally supports a single stage; multi-
// stage files fail with a clear error instead of silently ignoring FROM lines.
func Parse(r io.Reader) (*Dockerfile, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	df := &Dockerfile{Escape: '\\'}
	lineNo := 0
	stageSeen := false
	for sc.Scan() {
		lineNo++
		raw := strings.TrimRight(sc.Text(), "\r")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}

		if len(df.Instructions) == 0 && strings.HasPrefix(trimmed, "#") {
			name, value, ok := parseParserDirective(strings.TrimSpace(trimmed[1:]))
			if ok {
				switch strings.ToLower(name) {
				case "syntax", "escape":
					if strings.ToLower(name) == "escape" && len(value) != 1 {
						return nil, fmt.Errorf("line %d: escape directive requires one character", lineNo)
					}
					if strings.ToLower(name) == "escape" {
						df.Escape = value[0]
					}
				default:
					return nil, fmt.Errorf("line %d: unknown or unsupported parser directive %q", lineNo, name)
				}
				df.Directives = append(df.Directives, Directive{Name: strings.ToLower(name), Value: value})
				continue
			}
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}

		var logical strings.Builder
		for {
			if !endsWithContinuation(trimmed, df.Escape) {
				logical.WriteString(trimmed)
				break
			}
			logical.WriteString(strings.TrimRight(trimmed[:len(trimmed)-1], " \t"))
			logical.WriteByte(' ')
			if !sc.Scan() {
				return nil, fmt.Errorf("line %d: unterminated line continuation", lineNo)
			}
			lineNo++
			next := strings.TrimRight(sc.Text(), "\r")
			trimmed = strings.TrimSpace(next)
			// Docker permits comment lines inside a continued instruction.
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
		}
		text := logical.String()
		name, rest, err := splitInstruction(text)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", instrLine(text, lineNo), err)
		}
		instr := Instruction{Name: strings.ToUpper(name), RawArgs: strings.TrimSpace(rest), Line: instrLine(text, lineNo)}
		if strings.HasPrefix(instr.RawArgs, "[") {
			if err := json.Unmarshal([]byte(instr.RawArgs), &instr.JSONArgs); err != nil {
				return nil, fmt.Errorf("line %d: invalid JSON arguments: %w", instr.Line, err)
			}
			for _, a := range instr.JSONArgs {
				if strings.Contains(a, "\n") {
					return nil, fmt.Errorf("line %d: JSON argument contains newline", instr.Line)
				}
			}
			instr.HasJSONArg = true
		}
		if instr.Name == "FROM" {
			if stageSeen {
				return nil, fmt.Errorf("line %d: multi-stage builds are not supported yet", instr.Line)
			}
			stageSeen = true
		}
		switch instr.Name {
		case "FROM", "RUN", "CMD", "LABEL", "MAINTAINER", "EXPOSE", "ENV", "ARG", "ADD", "COPY", "ENTRYPOINT", "VOLUME", "USER", "WORKDIR", "ONBUILD", "STOPSIGNAL", "HEALTHCHECK", "SHELL":
		default:
			return nil, fmt.Errorf("line %d: unknown instruction %q", instr.Line, instr.Name)
		}
		df.Instructions = append(df.Instructions, instr)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(df.Instructions) == 0 || df.Instructions[0].Name != "FROM" {
		return nil, fmt.Errorf("Dockerfile must begin with FROM")
	}
	return df, nil
}

// ParseFile parses a Dockerfile from disk.
func ParseFile(path string) (*Dockerfile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

// Syntax returns the # syntax= directive, if present.
func (d *Dockerfile) Syntax() string {
	for _, x := range d.Directives {
		if x.Name == "syntax" {
			return x.Value
		}
	}
	return ""
}

// Base resolves the FROM operand to a base reference and optional tag.
func (d *Dockerfile) Base() (ref, tag string, err error) {
	for _, i := range d.Instructions {
		if i.Name != "FROM" {
			continue
		}
		args := strings.Fields(i.RawArgs)
		if len(args) == 0 {
			return "", "", fmt.Errorf("line %d: FROM requires a base image", i.Line)
		}
		ref = args[0]
		for j := 1; j < len(args); j++ {
			if strings.EqualFold(args[j], "AS") && j+1 < len(args) {
				tag = args[j+1]
				j++
				continue
			}
			return "", "", fmt.Errorf("line %d: unexpected FROM argument %q", i.Line, args[j])
		}
		return ref, tag, nil
	}
	return "", "", fmt.Errorf("missing FROM")
}

// LoadConfig applies supported instructions to cfg in source order. RUN/COPY/
// ADD are represented as BuildStep values by Builder; this helper updates only
// metadata that affects later instructions or the resulting image.
func ApplyConfig(cfg *ImageConfig, instr Instruction, vars map[string]string) error {
	defer syncEnvVars(cfg, vars)
	args, err := ExpandErr(vars, instr.RawArgs)
	if err != nil {
		return fmt.Errorf("line %d: %s: %w", instr.Line, instr.Name, err)
	}
	switch instr.Name {
	case "ENV":
		values, err := parseKeyValuePairs(args)
		if err != nil {
			return fmt.Errorf("line %d: ENV: %w", instr.Line, err)
		}
		cfg.Env = mergeEnv(cfg.Env, values)
	case "ARG":
		// handled by builder before variable expansion
	case "WORKDIR":
		if args == "" {
			return fmt.Errorf("line %d: WORKDIR requires a path", instr.Line)
		}
		p, err := absoluteInRoot(cfg.Cwd, args)
		if err != nil {
			return fmt.Errorf("line %d: WORKDIR: %w", instr.Line, err)
		}
		cfg.Cwd = p
	case "USER":
		cfg.User = strings.TrimSpace(args)
		if cfg.User == "" {
			return fmt.Errorf("line %d: USER requires a value", instr.Line)
		}
	case "LABEL":
		values, err := parseKeyValuePairs(args)
		if err != nil {
			return fmt.Errorf("line %d: LABEL: %w", instr.Line, err)
		}
		if cfg.Labels == nil {
			cfg.Labels = map[string]string{}
		}
		for _, kv := range values {
			k, v, _ := strings.Cut(kv, "=")
			cfg.Labels[k] = v
		}
	case "VOLUME":
		var names []string
		if instr.HasJSONArg {
			names = instr.JSONArgs
		} else {
			names = strings.Fields(args)
		}
		if len(names) == 0 {
			return fmt.Errorf("line %d: VOLUME requires at least one path", instr.Line)
		}
		for _, n := range names {
			p, err := absoluteInRoot("/", Expand(vars, n))
			if err != nil {
				return fmt.Errorf("line %d: VOLUME: %w", instr.Line, err)
			}
			cfg.Volumes = appendUnique(cfg.Volumes, p)
		}
	case "EXPOSE", "MAINTAINER", "STOPSIGNAL", "HEALTHCHECK", "SHELL", "ONBUILD", "CMD", "ENTRYPOINT":
		// CMD/ENTRYPOINT handled below; remaining directives are validated but
		// some have no effect in this runtime-compatible subset.
	}
	switch instr.Name {
	case "CMD":
		cmd, err := execForm(instr, args)
		if err != nil {
			return fmt.Errorf("line %d: CMD: %w", instr.Line, err)
		}
		cfg.Cmd = cmd
	case "ENTRYPOINT":
		ep, err := execForm(instr, args)
		if err != nil {
			return fmt.Errorf("line %d: ENTRYPOINT: %w", instr.Line, err)
		}
		cfg.Entrypoint = ep
	}
	return nil
}

// ExecArgs returns argv for CMD/ENTRYPOINT/RUN after applying Docker's shell
// form semantics. RUN always uses /bin/sh -c.
func ExecArgs(instr Instruction, args string) ([]string, error) {
	if instr.Name == "RUN" {
		if instr.HasJSONArg {
			return instr.JSONArgs, nil
		}
		return []string{"/bin/sh", "-c", args}, nil
	}
	return execForm(instr, args)
}

func execForm(instr Instruction, args string) ([]string, error) {
	if instr.HasJSONArg {
		return instr.JSONArgs, nil
	}
	if args == "" {
		return nil, nil
	}
	return []string{"/bin/sh", "-c", args}, nil
}

// Expand performs Docker-style ${VAR}/$VAR expansion. Command/process
// substitutions ($(...) and backticks) and shell parameter forms are left
// untouched for
// /bin/sh.
func Expand(vars map[string]string, s string) string {
	out, _ := ExpandErr(vars, s)
	return out
}

// ExpandErr is Expand with error reporting: ${VAR?...} on an unset variable
// aborts the expansion, matching Docker's build behaviour.
func ExpandErr(vars map[string]string, s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+1 < len(s) && (s[i+1] == '(' || s[i+1] == '`') {
			b.WriteByte('$')
			i++
			continue
		}
		i++
		if i >= len(s) {
			b.WriteByte('$')
			break
		}
		if s[i] == '{' {
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				b.WriteString("${")
				i++
				continue
			}
			out, err := expandBraced(vars, s[i+1:i+end])
			if err != nil {
				return "", err
			}
			b.WriteString(out)
			i += end + 1
			continue
		}
		start := i
		for i < len(s) && isVarChar(s[i]) {
			i++
		}
		name := s[start:i]
		if name == "" {
			b.WriteString("$")
			continue
		}
		b.WriteString(valueFor(vars, name, ""))
	}
	return b.String(), nil
}

func expandBraced(vars map[string]string, expr string) (string, error) {
	if validVarName(expr) {
		return valueFor(vars, expr, ""), nil
	}
	for _, op := range []string{":-", "?", "+", "-", "="} {
		idx := strings.Index(expr, op)
		if idx <= 0 || !validVarName(expr[:idx]) {
			continue
		}
		name, replacement := expr[:idx], expr[idx+len(op):]
		v := valueFor(vars, name, "")
		switch op {
		case ":-", "-", "=":
			if v == "" {
				return replacement, nil
			}
			return v, nil
		case "?":
			if v == "" {
				// Docker aborts the build on an unset required variable.
				msg := replacement
				if msg == "" {
					msg = "is not set"
				}
				return "", fmt.Errorf("required variable %s %s", name, msg)
			}
			return v, nil
		case "+":
			if v != "" {
				return replacement, nil
			}
			return "", nil
		}
	}
	return "${" + expr + "}", nil
}

func validVarName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isVarChar(s[i]) {
			return false
		}
	}
	return true
}

func valueFor(vars map[string]string, name, def string) string {
	if v, ok := vars[name]; ok {
		return v
	}
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func isVarChar(c byte) bool {
	return c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}

// ReadConfig loads image metadata from dir.
func ReadConfig(dir string) (ImageConfig, error) {
	var cfg ImageConfig
	b, err := os.ReadFile(filepath.Join(dir, ImageConfigFile))
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(b, &cfg)
	return cfg, err
}

// WriteConfig atomically writes image metadata to dir.
func WriteConfig(dir string, cfg ImageConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+".config-*")
	f, err := os.CreateTemp(filepath.Dir(tmp), filepath.Base(tmp))
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, ImageConfigFile))
}

func parseParserDirective(s string) (name, value string, ok bool) {
	eq := strings.IndexByte(s, '=')
	if eq < 0 {
		return "", "", false
	}
	name = strings.TrimSpace(s[:eq])
	value = strings.TrimSpace(s[eq+1:])
	return name, value, name != "" && value != ""
}

func splitInstruction(s string) (string, string, error) {
	sp := strings.IndexFunc(s, func(r rune) bool { return r == ' ' || r == '\t' })
	if sp < 0 {
		return s, "", nil
	}
	return s[:sp], s[sp+1:], nil
}

func endsWithContinuation(s string, esc byte) bool {
	if len(s) == 0 || s[len(s)-1] != esc {
		return false
	}
	n := 0
	for i := len(s) - 2; i >= 0 && s[i] == esc; i-- {
		n++
	}
	return n%2 == 0
}

func instrLine(_ string, fallback int) int { return fallback }

func parseKeyValuePairs(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("empty key-value list")
	}
	// Docker legacy ENV accepts arbitrary KEY=value text. Prefer quoted parsing.
	var out []string
	remaining := strings.TrimSpace(s)
	for remaining != "" {
		fields, err := shellFields(remaining)
		if err != nil {
			return nil, err
		}
		for _, f := range fields {
			k, v, ok := strings.Cut(f, "=")
			if !ok || k == "" || strings.ContainsAny(k, " \t\"'") {
				return nil, fmt.Errorf("invalid pair %q", f)
			}
			out = append(out, k+"="+v)
		}
		break
	}
	return out, nil
}

func shellFields(s string) ([]string, error) {
	var fields []string
	var cur strings.Builder
	inQuote := byte(0)
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			cur.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' && inQuote != '\'' {
			escaped = true
			continue
		}
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			} else {
				cur.WriteByte(c)
			}
			continue
		}
		switch c {
		case '"', '\'':
			inQuote = c
		case ' ', '\t':
			if cur.Len() > 0 {
				fields = append(fields, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if escaped {
		return nil, fmt.Errorf("trailing escape")
	}
	if inQuote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	if cur.Len() > 0 {
		fields = append(fields, cur.String())
	}
	return fields, nil
}

func syncEnvVars(cfg *ImageConfig, vars map[string]string) {
	if vars == nil {
		return
	}
	for _, kv := range cfg.Env {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
}

func mergeEnv(existing, updates []string) []string {
	out := append([]string(nil), existing...)
	index := map[string]int{}
	for i, kv := range out {
		k, _, _ := strings.Cut(kv, "=")
		index[k] = i
	}
	for _, kv := range updates {
		k, _, _ := strings.Cut(kv, "=")
		if pos, ok := index[k]; ok {
			out[pos] = kv
			continue
		}
		index[k] = len(out)
		out = append(out, kv)
	}
	return out
}

func absoluteInRoot(cwd, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if !filepath.IsAbs(p) {
		if cwd == "" {
			cwd = "/"
		}
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p), nil
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// ValidateCopySource rejects escaping the build context except via an explicit
// allowed context root supplied by the caller.
func ValidateCopySource(contextDir, src string) (string, error) {
	abs := src
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(contextDir, abs)
	}
	abs = filepath.Clean(abs)
	ctx := filepath.Clean(contextDir)
	rel, err := filepath.Rel(ctx, abs)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes build context", src)
	}
	return abs, nil
}
