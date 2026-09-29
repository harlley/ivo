package discover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePage puts a manual page where describePage can read it, which is what the
// index does thousands of times.
func writePage(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestDescribePageReadsMdoc is the shape macOS writes: the command is named by
// .Nm and described by .Nd, which is why a reader that only looks for text lines
// finds nothing at all.
func TestDescribePageReadsMdoc(t *testing.T) {
	path := writePage(t, "vm_stat.1", `.Dd January 1, 2026
.Dt VM_STAT 1
.Os
.Sh NAME
.Nm vm_stat
.Nd show Mach virtual memory statistics
.Sh SYNOPSIS
`)
	if got := describePage(path); got != "show Mach virtual memory statistics" {
		t.Errorf("description = %q", got)
	}
}

// TestDescribePageReadsTheManMacros is the other shape, and the escaped dash is
// the part that has to survive: the text reads "name - description", and a
// reader that keeps the escape gets a description that starts with a backslash.
func TestDescribePageReadsTheManMacros(t *testing.T) {
	path := writePage(t, "ls.1", `.TH LS 1
.SH NAME
ls \- list directory contents
.SH SYNOPSIS
`)
	if got := describePage(path); got != "list directory contents" {
		t.Errorf("description = %q", got)
	}
}

// TestDescribePageReadsAGeneratedPage covers the third shape: docbook output
// writes the section as .SH "NAME" and the description over several lines.
func TestDescribePageReadsAGeneratedPage(t *testing.T) {
	path := writePage(t, "git.1", `.SH "NAME"
git \- the stupid content tracker
.SH "SYNOPSIS"
`)
	if got := describePage(path); got != "the stupid content tracker" {
		t.Errorf("description = %q", got)
	}
}

// TestShortlistRanksByTheMachinesOwnWords is the tier that makes the priority
// list's blind spots survivable: a command nobody put on a list is still found
// when the machine's own summary of it matches the request.
func TestShortlistRanksByTheMachinesOwnWords(t *testing.T) {
	index := NameIndex{
		"df":        "display free disk space",
		"vm_stat":   "show Mach virtual memory statistics",
		"ls":        "list directory contents",
		"frobnicat": "frobnicate the frobnicator",
	}
	exists := func(string) bool { return true }

	disk := index.Shortlist("how much disk space is available?", exists, 10)
	if len(disk) == 0 || disk[0] != "df" {
		t.Errorf("shortlist for a disk question = %v, want df first", disk)
	}
	memory := index.Shortlist("how much memory does this machine have?", exists, 10)
	if len(memory) == 0 || memory[0] != "vm_stat" {
		t.Errorf("shortlist for a memory question = %v, want vm_stat first", memory)
	}
	if got := index.Shortlist("the and of", exists, 10); got != nil {
		t.Errorf("a request with no content words shortlisted %v", got)
	}
	// The filter is about what is installed, and the ranking is about the
	// request: a listing question with only ls installed returns ls and nothing
	// the machine does not have.
	only := index.Shortlist("list the files in this directory", func(name string) bool { return name == "ls" }, 10)
	if len(only) != 1 || only[0] != "ls" {
		t.Errorf("shortlist with only ls installed = %v, want [ls]", only)
	}
}

// TestCommandNameOfReadsPageNames covers the difference between a command page
// and everything else in a man directory.
func TestCommandNameOfReadsPageNames(t *testing.T) {
	for _, tc := range []struct {
		file, section, want string
	}{
		{"ls.1", "man1", "ls"},
		{"vm_stat.1.gz", "man1", "vm_stat"},
		{"sysctl.8", "man8", "sysctl"},
		{"foo.3", "man1", ""},
		{"foo.1m", "man1", ""},
		{"sub.dir.1", "man1", "sub.dir"},
	} {
		if got := commandNameOf(tc.file, tc.section); got != tc.want {
			t.Errorf("commandNameOf(%q, %q) = %q, want %q", tc.file, tc.section, got, tc.want)
		}
	}
}

// TestIndexWordsKeepsTheWordsThatSayWhatACommandIsFor is the trap this tier fell
// into first: the filter used to pick a search term drops the domain nouns
// themselves, so "list the files in this directory" matched nothing at all.
func TestIndexWordsKeepsTheWordsThatSayWhatACommandIsFor(t *testing.T) {
	words := indexWords("list the files in this directory")
	joined := strings.Join(words, " ")
	for _, want := range []string{"list", "files", "directory"} {
		if !strings.Contains(joined, want) {
			t.Errorf("indexWords dropped %q: %v", want, words)
		}
	}
	for _, unwanted := range []string{"the", "in", "this"} {
		for _, word := range words {
			if word == unwanted {
				t.Errorf("indexWords kept the function word %q", unwanted)
			}
		}
	}
	// And the difference is the point: the search term filter drops all of them.
	if got := ContentWords("list the files in this directory"); len(got) != 0 {
		t.Errorf("ContentWords kept %v, which is why this tier cannot use it", got)
	}
}
