package run

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRefusesAnythingOutsideTheAllowlist(t *testing.T) {
	allow := []string{"ls", "cat"}
	if err := Validate([]string{"ls", "-l"}, allow); err != nil {
		t.Errorf("ls should be allowed: %v", err)
	}
	err := Validate([]string{"rm", "-rf", "/"}, allow)
	if err == nil || !strings.Contains(err.Error(), "is not in the allowed binary list") {
		t.Errorf("rm should be refused, got %v", err)
	}
	if err := Validate(nil, allow); err == nil {
		t.Error("an empty command should be refused")
	}
	if err := Validate([]string{"ls", "a\x00b"}, allow); err == nil {
		t.Error("a NUL byte should be refused")
	}
}

func TestDryRunStartsNothing(t *testing.T) {
	var stdout bytes.Buffer
	outcome, err := Do(context.Background(), []string{"echo", "hi"}, Options{
		Execute: false,
		Allow:   []string{"echo"},
		Capture: true,
		Stdout:  &stdout,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if outcome.Ran {
		t.Error("a dry run must not run anything")
	}
	if stdout.Len() != 0 {
		t.Errorf("a dry run wrote %q", stdout.String())
	}
}

func TestExecuteCapturesOutput(t *testing.T) {
	outcome, err := Do(context.Background(), []string{"echo", "hello"}, Options{
		Execute: true,
		Allow:   []string{"echo"},
		Capture: true,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !outcome.Ran || outcome.ExitCode != 0 {
		t.Fatalf("outcome = %+v", outcome)
	}
	if strings.TrimSpace(outcome.Stdout) != "hello" {
		t.Errorf("stdout = %q", outcome.Stdout)
	}
}

func TestChildExitCodeIsPropagatedAsAnOutcomeNotAnError(t *testing.T) {
	outcome, err := Do(context.Background(), []string{"ls", "/definitely-not-a-real-path-jev"}, Options{
		Execute: true,
		Allow:   []string{"ls"},
		Capture: true,
	})
	if err != nil {
		t.Fatalf("a failing child is not a wrapper error: %v", err)
	}
	if outcome.ExitCode == 0 {
		t.Error("expected a non-zero exit code from ls")
	}
	if outcome.Stderr == "" {
		t.Error("expected stderr to be captured")
	}
}

func TestCapturedOutputIsTruncated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 5000)), 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := Do(context.Background(), []string{"cat", path}, Options{
		Execute:        true,
		Allow:          []string{"cat"},
		Capture:        true,
		MaxOutputBytes: 100,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(outcome.Stdout) != 100 {
		t.Errorf("captured %d bytes, want the 100-byte cap", len(outcome.Stdout))
	}
	if !outcome.Truncated {
		t.Error("truncation must be reported")
	}
}

func TestMissingProgramIsAnError(t *testing.T) {
	_, err := Do(context.Background(), []string{"jev-no-such-binary"}, Options{
		Execute: true,
		Allow:   []string{"jev-no-such-binary"},
		Capture: true,
	})
	if err == nil {
		t.Fatal("expected an error for a missing program")
	}
}

func TestStripOverstrike(t *testing.T) {
	// What a terminal formatter writes for bold: each character twice, with a
	// backspace between. Underline does the same with an underscore first.
	bold := "N\bNA\bAM\bME\bE"
	doubled := "NNAAMMEE"
	underlined := "_\bx_\by"
	mixed := "plain " + bold + " tail\n"

	cases := []struct{ in, want string }{
		{bold, "NAME"},
		{doubled, "NNAAMMEE"}, // the doubled form is not overstrike, leave it alone
		{underlined, "xy"},
		{mixed, "plain NAME tail\n"},
		{"", ""},
		{"\b", ""},                // an erase with nothing to erase
		{"trailing\b", "trailin"}, // an erase at the end takes the last character
		{"a\bb", "b"},
	}
	for _, tc := range cases {
		if got := string(StripOverstrike([]byte(tc.in))); got != tc.want {
			t.Errorf("StripOverstrike(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFilterAdaptsTheOutput(t *testing.T) {
	var stdout bytes.Buffer
	outcome, err := Do(context.Background(), []string{"echo", "-n", "N\bNA\bAM\bME\bE"}, Options{
		Execute: true,
		Allow:   []string{"echo"},
		Filter:  StripOverstrike,
		Stdout:  &stdout,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !outcome.Ran {
		t.Fatal("the command should have run")
	}
	if got := stdout.String(); got != "NAME" {
		t.Errorf("filtered output = %q, want %q", got, "NAME")
	}
}
