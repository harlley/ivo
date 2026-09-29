package discover

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInspectDoesNotExecuteTheCandidate(t *testing.T) {
	dir := t.TempDir()
	name := "ivo_inspection_fixture"
	marker := filepath.Join(dir, "ran")
	// If inspection tries --help, this otherwise harmless fixture records it.
	script := "#!/bin/sh\ntouch '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	docs := Inspect(name)
	if docs.Program != name {
		t.Fatalf("docs=%+v", docs)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("inspection ran the candidate: %v", err)
	}
}

func TestDescriptionExcerptRetainsPurposeAndIsBounded(t *testing.T) {
	got := descriptionOf("NAME\nfoo - sample\nDESCRIPTION\n Reports current measurements.\n More details.\n")
	if got != "Reports current measurements. More details." {
		t.Fatal(got)
	}
	long := make([]rune, 2000)
	for i := range long {
		long[i] = '界'
	}
	if len([]rune(descriptionOf(string(long)))) != 1500 {
		t.Fatal("description must be bounded without splitting unicode")
	}
}
