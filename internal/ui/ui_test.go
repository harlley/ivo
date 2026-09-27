package ui

import (
	"bytes"
	"strings"
	"testing"
)

func TestShellQuoteIsDisplayOnlyAndFaithful(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"ls", "-la", "."}, "ls -la ."},
		{[]string{"find", ".", "-name", "*.go"}, "find . -name '*.go'"},
		{[]string{"rg", "-e", "duas palavras", "."}, "rg -e 'duas palavras' ."},
		{[]string{"echo", "it's"}, `echo 'it'\''s'`},
		{[]string{"echo", ""}, "echo ''"},
	}
	for _, tc := range cases {
		if got := ShellQuote(tc.argv); got != tc.want {
			t.Errorf("ShellQuote(%q) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

func TestConfirmReadsTheAnswer(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"enter confirms", "\n", true},
		{"y confirms", "y\n", true},
		{"yes confirms", "YES\n", true},
		{"n declines", "n\n", false},
		{"no declines", "no\n", false},
		{"an unrecognised answer is not consent", "maybe\n", false},
		{"a closed input is not consent", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			p := &Printer{Out: &out, Err: &out}
			if got := p.Confirm(strings.NewReader(tc.input), "run it? [Y/n]"); got != tc.want {
				t.Errorf("Confirm(%q) = %v, want %v", tc.input, got, tc.want)
			}
			if !strings.Contains(out.String(), "run it?") {
				t.Errorf("the question should be printed, got %q", out.String())
			}
		})
	}
}
