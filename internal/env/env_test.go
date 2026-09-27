package env

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree creates a small directory to probe.
func writeTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"main.go", "README.md", "notas.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("contents\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, sub := range []string{"src", ".git"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestProbeFiltersAndOrdersTheListing(t *testing.T) {
	dir := writeTree(t)
	t.Chdir(dir)

	e, err := Probe(ProbeOptions{Request: "list the files", Binaries: []string{"ls"}, MaxEntries: 100})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	if e.CWD != dir {
		t.Errorf("cwd = %q, want %q", e.CWD, dir)
	}
	if e.EntryCount != 5 {
		t.Errorf("entry_count = %d, want 5", e.EntryCount)
	}
	if e.Truncated {
		t.Error("nothing should have been truncated")
	}
	// Subdirectories first, then files, each alphabetically. The order is
	// ours so the option list is stable across invocations.
	got := EntryNames(e.Entries)
	want := []string{".git/", "src/", "README.md", "main.go", "notas.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("entries = %v, want %v", got, want)
	}
	if e.OS == "" {
		t.Error("the OS should be reported")
	}
}

func TestProbeTruncatesAndCountsInCode(t *testing.T) {
	dir := writeTree(t)
	t.Chdir(dir)

	e, err := Probe(ProbeOptions{Request: "list", Binaries: nil, MaxEntries: 2})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(e.Entries) != 2 {
		t.Errorf("entries = %d, want the cap of 2", len(e.Entries))
	}
	if !e.Truncated {
		t.Error("truncation must be recorded")
	}
	// The count is a fact computed here, never asked of the model.
	if e.EntryCount != 5 {
		t.Errorf("entry_count = %d, want the real total 5", e.EntryCount)
	}
}

func TestProbeFindsTheRepository(t *testing.T) {
	dir := writeTree(t)
	nested := filepath.Join(dir, "src", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)

	e, err := Probe(ProbeOptions{Request: "what changed", Binaries: []string{"git"}})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !e.Git.IsRepo {
		t.Fatal("a .git further up the tree should be found")
	}
	if e.Git.Root != dir {
		t.Errorf("root = %q, want %q", e.Git.Root, dir)
	}
}

func TestProbeWithoutARepositoryIsNotAnError(t *testing.T) {
	t.Chdir(t.TempDir())
	e, err := Probe(ProbeOptions{Request: "list"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if e.Git.IsRepo {
		t.Error("a bare temp directory is not a repository")
	}
}

func TestCandidatesOnlyOfferPathsThatExist(t *testing.T) {
	dir := writeTree(t)
	t.Chdir(dir)

	e, err := Probe(ProbeOptions{
		Request: `show "main.go" and compare it with ./does-not-exist.go`,
	})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	if !contains(e.Candidates.Paths, "main.go") {
		t.Errorf("paths = %v, want main.go", e.Candidates.Paths)
	}
	for _, p := range e.Candidates.Paths {
		if strings.Contains(p, "does-not-exist") {
			t.Errorf("a path that does not exist was offered: %q", p)
		}
	}
	if !contains(e.Candidates.Terms, "main.go") {
		t.Errorf("terms = %v, want the quoted span as a search term too", e.Candidates.Terms)
	}
}

func TestCandidatesFindGlobsAndExtensions(t *testing.T) {
	t.Chdir(writeTree(t))
	e, err := Probe(ProbeOptions{Request: "find the *.md files and the .go ones too"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	for _, want := range []string{"*.md", "*.go"} {
		if !contains(e.Candidates.Patterns, want) {
			t.Errorf("patterns = %v, want %q", e.Candidates.Patterns, want)
		}
	}
}

func TestCandidatesFindShoutedWords(t *testing.T) {
	t.Chdir(writeTree(t))
	e, err := Probe(ProbeOptions{Request: "search for TODO and FIXME in the sources"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	for _, want := range []string{"TODO", "FIXME"} {
		if !contains(e.Candidates.Terms, want) {
			t.Errorf("terms = %v, want %q", e.Candidates.Terms, want)
		}
	}
}

func TestCandidatesFallBackToContentWords(t *testing.T) {
	t.Chdir(writeTree(t))
	// No quotes, no shouted words: without this fallback there would be
	// nothing to search for and the command would be unusable.
	e, err := Probe(ProbeOptions{Request: "where does panic appear in this project"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !contains(e.Candidates.Terms, "panic") {
		t.Errorf("terms = %v, want the content word panic", e.Candidates.Terms)
	}
	for _, noise := range []string{"where", "this"} {
		if contains(e.Candidates.Terms, noise) {
			t.Errorf("terms = %v should not contain the function word %q", e.Candidates.Terms, noise)
		}
	}
}

func TestCandidateListsAreCapped(t *testing.T) {
	t.Chdir(writeTree(t))
	words := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		words = append(words, "word"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	e, err := Probe(ProbeOptions{Request: strings.Join(words, " ")})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(e.Candidates.Terms) > MaxTermCandidates {
		t.Errorf("terms = %d, want at most %d", len(e.Candidates.Terms), MaxTermCandidates)
	}
}

func TestProbeSurvivesAnUnreadableLookingRequest(t *testing.T) {
	t.Chdir(writeTree(t))
	// Nothing here should panic: the request is untrusted text.
	e, err := Probe(ProbeOptions{Request: "$(rm -rf /) ; `whoami` 'x' \"y\" | cat"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	for _, p := range e.Candidates.Paths {
		if strings.ContainsAny(p, ";&|`$") {
			t.Errorf("a shell fragment was offered as a path: %q", p)
		}
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestPortugueseInputIsStillHandled keeps the language-independent promise
// honest. The catalog, the questions and the output are all English, but the
// phrase the user types is not assumed to be: Portuguese function words are
// filtered and the words that carry meaning survive as candidates.
func TestPortugueseInputIsStillHandled(t *testing.T) {
	t.Chdir(writeTree(t))
	e, err := Probe(ProbeOptions{Request: "liste todos os arquivos desse diretório"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	for _, noise := range []string{"liste", "todos", "desse", "diretório", "arquivos"} {
		if contains(e.Candidates.Terms, noise) {
			t.Errorf("terms = %v should have filtered the function word %q", e.Candidates.Terms, noise)
		}
	}
}
