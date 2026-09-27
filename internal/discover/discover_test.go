package discover

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/harlleyoliveira/jev-cli/internal/run"
)

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	// The manual fixture holds the terminal overstrike the reader has to
	// survive before anything can be parsed.
	return string(run.StripOverstrike(raw))
}

// The fixtures are real output captured from this machine: a mandoc manual page
// and a GNU style --help.
func TestParseManReadsBSDManualPages(t *testing.T) {
	options := ParseMan(loadFixture(t, "man-ls.txt"))
	if len(options) < 20 {
		t.Fatalf("parsed %d options, want the manual's option list", len(options))
	}

	byFlag := map[string]Option{}
	for _, o := range options {
		for _, f := range o.Flags {
			byFlag[f] = o
		}
	}

	all, ok := byFlag["-A"]
	if !ok {
		t.Fatal("could not find -A")
	}
	if !all.Bool() {
		t.Errorf("-A takes a value (%q) but it stands on its own", all.Arg)
	}
	if !strings.Contains(all.Desc, "directory entries whose names begin with a dot") {
		t.Errorf("-A description = %q", all.Desc)
	}
	if all.Name() != "A" {
		t.Errorf("-A name = %q", all.Name())
	}

	// -D takes a value, which is exactly what the layer must not guess.
	if d, ok := byFlag["-D"]; !ok {
		t.Error("could not find -D")
	} else if d.Bool() {
		t.Error("-D documents an argument and must not be treated as a flag")
	}

	// The long spelling wins as the name when both are documented.
	if colour, ok := byFlag["--color"]; ok && colour.Name() != "color" {
		t.Errorf("--color name = %q", colour.Name())
	}
}

func TestParseHelpReadsGNUStyleHelp(t *testing.T) {
	options := ParseHelp(loadFixture(t, "help-rg.txt"))
	if len(options) < 30 {
		t.Fatalf("parsed %d options, want ripgrep's option list", len(options))
	}

	byFlag := map[string]Option{}
	for _, o := range options {
		for _, f := range o.Flags {
			byFlag[f] = o
		}
	}
	zip, ok := byFlag["--search-zip"]
	if !ok {
		t.Fatal("could not find --search-zip")
	}
	if !zip.Bool() {
		t.Errorf("--search-zip takes a value but stands on its own")
	}
	if zip.Name() != "search-zip" {
		t.Errorf("name = %q, want search-zip", zip.Name())
	}
	if !strings.Contains(zip.Desc, "compressed") {
		t.Errorf("description = %q", zip.Desc)
	}

	// An option documented with a value is recognised as such.
	if regexp, ok := byFlag["-e"]; !ok {
		t.Error("could not find -e")
	} else if regexp.Bool() {
		t.Error("-e takes PATTERN and must not be treated as a flag")
	}

	// The description runs across several lines and has to be flattened.
	if cases, ok := byFlag["-s"]; ok {
		if strings.Contains(cases.Desc, "\n") {
			t.Errorf("description should be one paragraph, got %q", cases.Desc)
		}
		if len(cases.Desc) < 40 {
			t.Errorf("description looks truncated: %q", cases.Desc)
		}
	}
}

// TestFlagsOffersEverythingBooleanAndNothingElse pins the shape of the
// candidate set: the program's own list, in its own order, with the options
// that would need a value left out.
func TestFlagsOffersEverythingBooleanAndNothingElse(t *testing.T) {
	docs := Docs{Program: "ls", Source: "man", Options: ParseMan(loadFixture(t, "man-ls.txt"))}
	flags := Flags(docs, 0)
	if len(flags) < 30 {
		t.Fatalf("got %d boolean options, want the manual's list", len(flags))
	}
	for _, o := range flags {
		if !o.Bool() {
			t.Errorf("%v needs a value the layer cannot invent", o.Flags)
		}
	}
	if capped := Flags(docs, 5); len(capped) != 5 {
		t.Errorf("limit ignored: got %d", len(capped))
	}
}

// TestLexicalMatchingWouldMissTheObviousOption records the finding that made
// this package enumerate everything instead of pre-selecting. Both of these are
// options a plain request asks for, and neither shares a word with the manual's
// own description of it.
func TestLexicalMatchingWouldMissTheObviousOption(t *testing.T) {
	docs := Docs{Program: "ls", Source: "man", Options: ParseMan(loadFixture(t, "man-ls.txt"))}
	byFlag := map[string]Option{}
	for _, o := range docs.Options {
		for _, f := range o.Flags {
			byFlag[f] = o
		}
	}

	cases := []struct{ request, flag, desc string }{
		{"list everything including hidden files", "-A", "begin with a dot"},
		{"list the files with details", "-l", "long format"},
	}
	for _, tc := range cases {
		option, ok := byFlag[tc.flag]
		if !ok {
			t.Fatalf("%s is missing from the fixture", tc.flag)
		}
		if strings.Contains(strings.ToLower(option.Desc), tc.request) {
			t.Fatalf("the fixture changed: %s now quotes the request", tc.flag)
		}
		matched := false
		for _, word := range ContentWords(tc.request) {
			if strings.Contains(strings.ToLower(option.Desc), word) {
				matched = true
			}
		}
		if matched {
			t.Logf("%s now matches lexically; the manual's wording changed", tc.flag)
		}
	}
}

func TestContentWordsDropsWhatEveryPhraseHas(t *testing.T) {
	got := ContentWords("list all files in this directory with details")
	joined := strings.Join(got, " ")
	for _, noise := range []string{"list", "all", "files", "this", "directory"} {
		if strings.Contains(" "+joined+" ", " "+noise+" ") {
			t.Errorf("ContentWords kept %q: %v", noise, got)
		}
	}
	if !strings.Contains(joined, "details") {
		t.Errorf("ContentWords dropped the word that matters: %v", got)
	}

	// The phrase is not assumed to be English.
	got = ContentWords("liste todos os arquivos desse diretório")
	if len(got) != 0 {
		t.Errorf("a Portuguese request of function words left %v", got)
	}
	if got := ContentWords("mostre o conteúdo do README.md"); !contains(got, "README.md") {
		t.Errorf("ContentWords = %v, want the file name", got)
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

// TestChooseSourcePrefersHelpAndFallsBackToTheManual is the efficiency rule: the
// help output is one cheap command written for this purpose, so it wins whenever
// it actually documents something, and the manual is read only when it does not.
func TestChooseSourcePrefersHelpAndFallsBackToTheManual(t *testing.T) {
	full := func(source string, n int, bytes int) Docs {
		docs := Docs{Program: "p", Source: source, Bytes: bytes}
		for i := 0; i < n; i++ {
			docs.Options = append(docs.Options, Option{Flags: []string{"-x"}})
		}
		return docs
	}

	cases := []struct {
		name             string
		help, manual     Docs
		helpOK, manualOK bool
		wantSource       string
	}{
		{"help documents options, so the manual is never read", full("help", 40, 4000), full("man", 44, 18000), true, true, "help"},
		{"help is a usage stub, so the manual wins", full("help", 2, 80), full("man", 44, 18000), true, true, "man"},
		{"only the manual documents anything", full("help", 0, 40), full("man", 44, 18000), true, true, "man"},
		{"only help works", full("help", 12, 900), Docs{}, true, false, "help"},
		{"neither works", Docs{}, Docs{}, false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			docs, ok := chooseSource(tc.help, tc.manual, tc.helpOK, tc.manualOK)
			if tc.wantSource == "" {
				if ok {
					t.Fatalf("chose %q, want no source", docs.Source)
				}
				return
			}
			if !ok || docs.Source != tc.wantSource {
				t.Fatalf("chose %q (ok=%v), want %q", docs.Source, ok, tc.wantSource)
			}
		})
	}
}

func TestCommandsAreListedNativelyAndCleanly(t *testing.T) {
	commands, err := Commands()
	if err != nil {
		t.Skipf("no command list on this machine: %v", err)
	}
	if len(commands) < 100 {
		t.Fatalf("listed %d commands, want the shell's command list", len(commands))
	}
	seen := map[string]bool{}
	for _, name := range commands {
		if seen[name] {
			t.Errorf("%q is listed twice", name)
		}
		seen[name] = true
		if strings.ContainsAny(name, " /\\") {
			t.Errorf("%q is not a command name", name)
		}
		if _, err := exec.LookPath(name); err != nil {
			t.Errorf("%q cannot be run as a program", name)
		}
	}
	// The listing is what a person can type, and it is cheap: a shell answers
	// it in a tenth of a second, where reading every program's summary took
	// seven seconds for eight of them.
	if !seen["ls"] {
		t.Error("ls should be in the shell's command list")
	}
}

func TestRetrieveKeepsTheCommandsTheRequestNames(t *testing.T) {
	commands := []string{"ls", "zed", "rm", "sed", "git", "docker", "cat", "rg"}

	got := Retrieve(commands, "open the current project in zed", 8)
	if len(got) == 0 {
		t.Fatal("Retrieve found nothing for a request that names zed")
	}
	if got[0].Name != "zed" {
		t.Fatalf("Retrieve = %v, want zed first", got)
	}
	for _, p := range got {
		if p.Name == "docker" || p.Name == "cat" {
			t.Errorf("%q shares nothing with the request and should not be offered", p.Name)
		}
	}

	// Risk is decided in code, not by the model.
	got = Retrieve(commands, "remove the directory with rm", 8)
	if len(got) == 0 || got[0].Name != "rm" {
		t.Fatalf("Retrieve = %v, want rm first", got)
	}
	if got[0].ReadOnly {
		t.Error("rm must not be marked read-only")
	}

	if got := Retrieve(commands, "zzz nothing here", 8); len(got) != 0 {
		t.Errorf("Retrieve = %v, want nothing", got)
	}
}

func TestDescribeUsesTheProgramsOwnWords(t *testing.T) {
	described := Describe([]Program{{Name: "ls", ReadOnly: true}})
	if len(described) != 1 {
		t.Fatalf("Describe returned %d programs", len(described))
	}
	if described[0].Summary == "" {
		t.Error("ls should be able to say what it is for")
	}
	if strings.HasPrefix(strings.ToLower(described[0].Summary), "usage") {
		t.Errorf("summary = %q, want a description rather than a usage line", described[0].Summary)
	}
}
