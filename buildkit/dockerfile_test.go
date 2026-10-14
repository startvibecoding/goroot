package buildkit

import (
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
