package discover

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/harlley/ivo/internal/run"
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

// TestChooseSourcePrefersHelpUntilItIsThin is the rule, and it has two
// regression cases: git --help lists twenty-three subcommands and no options, so
// measuring the source in options alone rejected it and fell back to a manual
// page that documents neither; and bsdtar answers --help with thirteen options
// while its manual documents ninety-two, so keeping help by default hid the flag
// surface behind a usage summary.
func TestChooseSourcePrefersHelpUntilItIsThin(t *testing.T) {
	withOptions := func(source string, n int) Docs {
		docs := Docs{Program: "p", Source: source}
		for i := 0; i < n; i++ {
			docs.Options = append(docs.Options, Option{Flags: []string{"-x"}, Argv: []string{"-x"}})
		}
		return docs
	}
	withSubcommands := func(n int) Docs {
		docs := Docs{Program: "git", Source: "help"}
		for i := 0; i < n; i++ {
			docs.Subcommands = append(docs.Subcommands, Subcommand{Name: "log", Desc: "Show commit logs"})
		}
		return docs
	}

	cases := []struct {
		name         string
		help, manual Docs
		want         string
	}{
		// A help output with a real list is not thin, so the loader never reads
		// the manual and there is nothing to compare.
		{"a real help list leaves nothing to compare", withOptions("help", 40), Docs{}, "help"},
		{"a thin help loses to the fuller manual", withOptions("help", 13), withOptions("man", 92), "man"},
		{"help documents only subcommands, and still wins", withSubcommands(23), withOptions("man", 44), "help"},
		{"help is a usage stub, so the manual is the fallback", withOptions("help", 0), withOptions("man", 44), "man"},
		{"the manual is a stub too, so there is nothing", withOptions("help", 0), withOptions("man", 0), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			docs, ok := chooseSource(tc.help, tc.manual)
			if tc.want == "" {
				if ok {
					t.Fatalf("chose %q, want no source", docs.Source)
				}
				return
			}
			if !ok || docs.Source != tc.want {
				t.Fatalf("chose %q (ok=%v), want %q", docs.Source, ok, tc.want)
			}
		})
	}
}

// TestThinHelpDrawsTheLineAtTheSummary pins the boundary, because it is the
// only thing that decides whether a program pays for a manual as well.
func TestThinHelpDrawsTheLineAtTheSummary(t *testing.T) {
	count := func(n int) Docs {
		docs := Docs{}
		for i := 0; i < n; i++ {
			docs.Options = append(docs.Options, Option{Flags: []string{"-x"}, Argv: []string{"-x"}})
		}
		return docs
	}
	if !thinHelp(count(helpIsThin - 1)) {
		t.Errorf("a help output of %d options is a summary", helpIsThin-1)
	}
	if thinHelp(count(helpIsThin)) {
		t.Errorf("a help output of %d options is the list", helpIsThin)
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

// TestWordCandidatesKeepsShortNames is the bug that made "rm" invisible: the
// word filter used the search term rules, which drop anything shorter than three
// characters, and half of the standard commands are shorter than that.
func TestWordCandidatesKeepsShortNames(t *testing.T) {
	got := WordCandidates("apague o diretório build com rm")
	joined := strings.Join(got, " ")
	for _, want := range []string{"rm", "build"} {
		if !contains(got, want) {
			t.Errorf("WordCandidates = %v, missing %q", got, want)
		}
	}
	for _, word := range got {
		if len([]rune(word)) < 2 {
			t.Errorf("single letters should still be dropped, got %q in %v", word, got)
		}
	}
	_ = joined

	// The filter is for program names, so it does not use the stopword list.
	if got := WordCandidates("where is ps"); !contains(got, "ps") {
		t.Errorf("WordCandidates = %v, want ps", got)
	}
	if got := WordCandidates("go to the directory"); !contains(got, "go") {
		t.Errorf("WordCandidates = %v, want go", got)
	}
	// Repeats are asked once.
	if got := WordCandidates("ls ls LS"); len(got) != 1 {
		t.Errorf("WordCandidates = %v, want one entry", got)
	}
}

// TestParseSubcommandsReadsACommandList is the case that made git unreachable:
// git --help documents twenty-three commands and no options at all, so a reader
// that only understood an OPTIONS section saw nothing.
func TestParseSubcommandsReadsACommandList(t *testing.T) {
	subcommands := ParseSubcommands(loadFixture(t, "help-git.txt"))
	if len(subcommands) < 15 {
		t.Fatalf("read %d commands, want git's own list", len(subcommands))
	}

	byName := map[string]Subcommand{}
	for _, sub := range subcommands {
		byName[sub.Name] = sub
	}
	for name, want := range map[string]string{
		"clone":  "Clone a repository",
		"commit": "Record changes",
		"log":    "Show commit logs",
	} {
		sub, ok := byName[name]
		if !ok {
			t.Errorf("%q is missing from the command list", name)
			continue
		}
		if !strings.Contains(sub.Desc, want) {
			t.Errorf("%s description = %q, want it to mention %q", name, sub.Desc, want)
		}
	}
	// Prose and synopsis lines are not commands.
	for _, noise := range []string{"these", "usage", "or"} {
		if _, ok := byName[noise]; ok {
			t.Errorf("%q was read as a command", noise)
		}
	}
}

// TestCandidatesCarryTheValuesTheRequestMentions: "-n" needs a number, and the
// only number this layer may use is one the request already said.
func TestCandidatesCarryTheValuesTheRequestMentions(t *testing.T) {
	docs := Docs{Program: "git log", Options: []Option{
		{Flags: []string{"--oneline"}, Desc: "Show the commit log in one line."},
		{Flags: []string{"-n", "--max-count"}, Arg: "number", Desc: "Limit the number of commits."},
		{Flags: []string{"--author"}, Arg: "pattern", Desc: "Limit to an author."},
	}}

	candidates := Candidates(docs, "list the last 3 commits", NamedValues{}, 0)
	keys := map[string]bool{}
	for _, c := range candidates {
		keys[c.Key()] = true
	}
	if !keys["--oneline"] {
		t.Error("a boolean option should be offered")
	}
	if !keys["-n 3"] {
		t.Errorf("the counted option should carry the number the request mentions, got %v", keys)
	}
	if keys["--author"] {
		t.Error("an option whose value the request does not supply must not be offered")
	}

	// With no number in the request there is nothing to fill the count with.
	for _, c := range Candidates(docs, "list the commits", NamedValues{}, 0) {
		if strings.HasPrefix(c.Key(), "-n") {
			t.Errorf("invented a value for -n: %q", c.Key())
		}
	}
}

func TestSubcommandCandidatesCarryTheCommands(t *testing.T) {
	subs := []Subcommand{{Name: "log", Desc: "Show commit logs"}, {Name: "clone", Desc: "Clone a repository"}}
	got := SubcommandCandidates(subs, 0)
	if len(got) != 2 {
		t.Fatalf("got %d candidates", len(got))
	}
	if got[0].Key() != "log" || got[0].Argv[0] != "log" {
		t.Errorf("first candidate = %+v, want the log command as one token", got[0])
	}
}

// TestLoadReadsAProgramThatOnlyListsCommands is the end of the git story: the
// program documents commands and no options, and the layer has to see it.
func TestLoadReadsAProgramThatOnlyListsCommands(t *testing.T) {
	docs, err := Load("git")
	if err != nil {
		t.Skipf("git is not usable here: %v", err)
	}
	if docs.Source != "help" {
		t.Errorf("source = %q, want help: git --help documents its own commands", docs.Source)
	}
	if len(docs.Subcommands) < 10 {
		t.Fatalf("read %d commands from %s (%d options)", len(docs.Subcommands), docs.Source, len(docs.Options))
	}
}

// TestParseManKeepsAFlagNextToAPlaceholder is the git log count story. The
// manual spells one option three ways:
//
//	-<number>, -n <number>, --max-count=<number>
//
// The first is a shape that cannot be typed, and the other two are flags whose
// value is only named in the same breath. Testing the placeholder against the
// whole spelling threw away -n and left git log unable to express a count.
func TestParseManKeepsAFlagNextToAPlaceholder(t *testing.T) {
	for _, tc := range []struct {
		spec  string
		flags []string
		arg   string
	}{
		{"-<number>, -n <number>, --max-count=<number>", []string{"-n", "--max-count"}, "number"},
		{"-n <number>, --max-count=<number>", []string{"-n", "--max-count"}, "number"},
		{"-<number>, -n, --max-count", []string{"-n", "--max-count"}, ""},
		{"-D <format>, --date=<format>", []string{"-D", "--date"}, "format"},
		{"-a, --all", []string{"-a", "--all"}, ""},
		{"--[no-]recurse-submodules", []string{"--recurse-submodules"}, ""},
		{"--color[=WHEN], --colour[=WHEN]", []string{"--color", "--colour"}, "WHEN"},
		{"-", nil, ""},
	} {
		flags, arg := parseSpec(tc.spec)
		if strings.Join(flags, " ") != strings.Join(tc.flags, " ") {
			t.Errorf("parseSpec(%q) flags = %v, want %v", tc.spec, flags, tc.flags)
		}
		if arg != tc.arg {
			t.Errorf("parseSpec(%q) arg = %q, want %q", tc.spec, arg, tc.arg)
		}
	}
}

// TestParseManReadsGitLogCount walks the real manual page, because the fixture
// above would keep passing even if the reader never reached that entry.
func TestParseManReadsGitLogCount(t *testing.T) {
	text := strings.Join([]string{
		"OPTIONS",
		"       -<number>, -n <number>, --max-count=<number>",
		"           Limit the number of commits to output.",
		"",
		"       --follow",
		"           List history beyond renames (works only for a single file).",
	}, "\n")

	byFlag := map[string]Option{}
	for _, o := range ParseMan(text) {
		for _, f := range o.Flags {
			byFlag[f] = o
		}
	}
	count, ok := byFlag["-n"]
	if !ok {
		t.Fatalf("no -n in %v", byFlag)
	}
	if count.Arg != "number" {
		t.Errorf("-n arg = %q, want number", count.Arg)
	}
	if !strings.Contains(count.Desc, "Limit the number of commits") {
		t.Errorf("-n desc = %q, want the count description", count.Desc)
	}
}

// TestGitLogDocumentsItsCount is the integration half: the manual on this
// machine must yield the count flag, because "list the last 3 commits" has no
// other way to say three.
func TestGitLogDocumentsItsCount(t *testing.T) {
	docs, err := Load("git log")
	if err != nil {
		t.Skipf("git log is not readable here: %v", err)
	}
	if len(docs.Options) == 0 {
		t.Skipf("read no options from %s", docs.Source)
	}
	for _, o := range docs.Options {
		for _, f := range o.Flags {
			if f == "-n" || f == "--max-count" {
				return
			}
		}
	}
	t.Errorf("read %d options from %s and none of them take a count", len(docs.Options), docs.Source)
}

// TestCandidatesBindTheValuesTheRequestNames is the gap that made "grep for the
// word timeout" come back as `grep -R -w .`: a flag whose value is a pattern or
// a path was dropped entirely, because the only value this layer knew how to
// bind was a number.
func TestCandidatesBindTheValuesTheRequestNames(t *testing.T) {
	docs := Docs{Program: "grep", Options: []Option{
		{Flags: []string{"-e", "--regexp"}, Arg: "pattern", Desc: "Match the pattern."},
		{Flags: []string{"--include"}, Arg: "glob", Desc: "Search only files matching the glob."},
		{Flags: []string{"-f"}, Arg: "file", Desc: "Read patterns from a file."},
		{Flags: []string{"-n", "--line-number"}, Desc: "Prefix each line with its number."},
		{Flags: []string{"--binary-files"}, Arg: "type", Desc: "How to handle binary files."},
	}}
	values := NamedValues{
		Paths:    []string{"notes.txt", "other.txt", "third.txt"},
		Patterns: []string{"*.go"},
		Terms:    []string{"timeout"},
	}
	keys := map[string]bool{}
	for _, c := range Candidates(docs, "grep for timeout in the go files of notes.txt", values, 0) {
		keys[c.Key()] = true
	}
	for _, want := range []string{"-e timeout", "--include *.go", "-f notes.txt", "-n"} {
		if !keys[want] {
			t.Errorf("expected %q among the candidates: %v", want, keys)
		}
	}
	withPaths := 0
	for key := range keys {
		if strings.HasPrefix(key, "-f ") {
			withPaths++
		}
	}
	if withPaths > maxValuesPerFlag {
		t.Errorf("the flag was offered with %d paths, want at most %d", withPaths, maxValuesPerFlag)
	}
	if keys["--binary-files"] {
		t.Error("a placeholder this layer cannot fill from the request must not be offered")
	}
}
