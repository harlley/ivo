package ui

import "testing"

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
