package buildkit

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExpandPreservesShellSubstitution(t *testing.T) {
	got := Expand(map[string]string{"NAME": "world"}, `echo "$NAME $(whoami)" ${OTHER:-fallback} $UNSET`)
	want := `echo "world $(whoami)" fallback `
	if got != want {
		t.Fatalf("Expand() = %q, want %q", got, want)
	}
}

func TestParseEscapeDirectiveAndContinuation(t *testing.T) {
	df, err := Parse(strings.NewReader("# escape=!\nFROM base\nRUN echo a !\n  b\n"))
	if err != nil {
		t.Fatal(err)
	}
	if df.Escape != '!' {
		t.Fatalf("escape = %q", df.Escape)
	}
	if got := df.Instructions[1].RawArgs; got != "echo a b" {
		t.Fatalf("RUN args = %q", got)
	}
}

func TestValidateCopySourceRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	if _, err := ValidateCopySource(dir, "../secret"); err == nil {
		t.Fatal("expected context escape to be rejected")
	}
}

func TestPlanMakesEnvAvailableToLaterInstructions(t *testing.T) {
	df, err := Parse(strings.NewReader("FROM base\nENV NAME=world\nRUN echo $NAME\nCOPY ${NAME}.txt /target\n"))
	if err != nil {
		t.Fatal(err)
	}
	steps, vars, err := Plan(df, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if vars["NAME"] != "world" {
		t.Fatalf("vars = %#v", vars)
	}
	if len(steps) != 4 {
		t.Fatalf("steps = %d", len(steps))
	}
	if got := Expand(vars, steps[2].Instruction.RawArgs); got != "echo world" {
		t.Fatalf("RUN args = %q", got)
	}
	if got := Expand(vars, steps[3].Instruction.RawArgs); got != "world.txt /target" {
		t.Fatalf("COPY args = %q", got)
	}
}

func TestExpandErrAbortsOnUnsetRequiredVar(t *testing.T) {
	if _, err := ExpandErr(map[string]string{}, `${REQ?message for req}`); err == nil || !strings.Contains(err.Error(), "required variable REQ") {
		t.Fatalf("err = %v", err)
	}
	if _, err := ExpandErr(map[string]string{}, `${REQ?}`); err == nil || !strings.Contains(err.Error(), "is not set") {
		t.Fatalf("err = %v", err)
	}
	out, err := ExpandErr(map[string]string{"REQ": "v"}, `${REQ?message}`)
	if err != nil || out != "v" {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	// Set variables still expand; unset non-required vars stay empty.
	out, err = ExpandErr(map[string]string{"N": "x"}, "a${N:-d}b $UNSET")
	if err != nil || out != "axb " {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}

func TestPlanRejectsUnsetRequiredVar(t *testing.T) {
	df, err := Parse(strings.NewReader("FROM base\nRUN echo ${REQ?needed}"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Plan(df, t.TempDir()); err == nil {
		t.Fatal("expected Plan to fail on ${REQ?}")
	}
}

func TestParseCopyArgsStaticVsEagerList(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "list.txt"), []byte("# comment\n\na.txt\nb.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	instr := Instruction{Name: "COPY", RawArgs: "--files-from=list.txt /dest/", Line: 2}

	// Static (Plan): list file is recorded, sources stay empty.
	parsed, err := parseCopyArgs(dir, instr, "--files-from=list.txt /dest/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.allAtOnce || parsed.listFile != "list.txt" || parsed.dest != "/dest/" || len(parsed.sources) != 0 {
		t.Fatalf("static parsed = %#v", parsed)
	}

	// Eager (apply): list file is read and resolved.
	parsed, err = parseCopyArgs(dir, instr, "--files-from=list.txt /dest/", func(list string) ([]string, error) {
		b := &Builder{ContextDir: dir}
		return b.resolveListFile(instr, list)
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.txt", "b.txt"}; !reflect.DeepEqual(parsed.sources, want) {
		t.Fatalf("sources = %#v", parsed.sources)
	}

	// Missing list file fails at eager time, not static time.
	if _, err := parseCopyArgs(dir, instr, "--files-from=missing.txt /dest/", func(list string) ([]string, error) {
		b := &Builder{ContextDir: dir}
		return b.resolveListFile(instr, list)
	}); err == nil {
		t.Fatal("expected missing list file to fail")
	}
}

func TestCopySourcesStreamsFileAndDir(t *testing.T) {
	b := &Builder{ContextDir: t.TempDir()}
	ctx := t.TempDir()
	b.ContextDir = ctx
	if err := os.WriteFile(filepath.Join(ctx, "script.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(ctx, "data")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "v.txt"), []byte("value"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := b.copySources(root, []string{"script.sh"}, "/bin/script.sh", false, false); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(root, "bin/script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("perm = %v", fi.Mode().Perm())
	}
	// Directory source with allAtOnce keeps the existing semantic: the tar.gz
	// payload lands under dest/ (extraction happens only for explicit archives
	// with a single source).
	if err := b.copySources(root, []string{"data"}, "/opt/", false, true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "opt/v.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "value" {
		t.Fatalf("got = %q", got)
	}
}
