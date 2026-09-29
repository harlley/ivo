package discover

import (
	"sort"
	"strings"
	"testing"
)

// byFlag indexes options the way the walk looks them up.
func byFlag(options []Option) map[string]Option {
	index := map[string]Option{}
	for _, option := range options {
		for _, flag := range option.Flags {
			index[flag] = option
		}
	}
	return index
}

// TestParseManHTMLReadsMdocPages reads a page written in mdoc, where the markup
// names the flags: <code class="Fl"> is a flag and <var class="Ar"> is the
// placeholder for its value.
func TestParseManHTMLReadsMdocPages(t *testing.T) {
	options := ParseManHTML(loadFixture(t, "mandoc-ls.html"))
	if len(options) < 40 {
		t.Fatalf("read %d options, want the manual's option list", len(options))
	}

	index := byFlag(options)
	for _, flag := range []string{"-l", "-A", "-a", "-D", "-n"} {
		if _, ok := index[flag]; !ok {
			t.Errorf("could not find %s", flag)
		}
	}
	if got := index["-D"].Arg; got != "format" {
		t.Errorf("-D arg = %q, want format: the placeholder is markup", got)
	}
	if got := index["-l"].Arg; got != "" {
		t.Errorf("-l arg = %q, want none: it stands on its own", got)
	}
	if !strings.Contains(index["-l"].Desc, "long format") {
		t.Errorf("-l desc = %q, want the long format description", index["-l"].Desc)
	}
}

// TestParseManHTMLKeepsOptionalValues is the third spelling a flag can have:
// --color[=when] offers a value and does not require it. The bracket is not
// part of the flag, and the value it names is still a placeholder.
func TestParseManHTMLKeepsOptionalValues(t *testing.T) {
	index := byFlag(ParseManHTML(loadFixture(t, "mandoc-ls.html")))
	color, ok := index["--color"]
	if !ok {
		t.Fatalf("could not find --color in %d options", len(index))
	}
	if color.Arg != "when" {
		t.Errorf("--color arg = %q, want when", color.Arg)
	}
}

// TestParseManHTMLAgreesWithTheTextDevice matters because the text device is the
// fallback: a machine without mandoc has to offer the same options, or the
// answer depends on which machine it was cached on.
func TestParseManHTMLAgreesWithTheTextDevice(t *testing.T) {
	names := func(options []Option) []string {
		var out []string
		for _, option := range options {
			out = append(out, strings.Join(option.Flags, "=")+" <"+option.Arg+">")
		}
		sort.Strings(out)
		return out
	}
	fromHTML := names(ParseManHTML(loadFixture(t, "mandoc-ls.html")))
	fromText := names(ParseMan(loadFixture(t, "man-ls.txt")))
	if strings.Join(fromHTML, "\n") != strings.Join(fromText, "\n") {
		t.Errorf("the two readers disagree\nhtml:\n%s\ntext:\n%s",
			strings.Join(fromHTML, "\n"), strings.Join(fromText, "\n"))
	}
}

// TestParseManHTMLReadsGeneratedPages reads a page written for the man macros,
// which is what a generated page is: git, rg and gh all ship one. No class
// names a flag here, so the spec line is read as text and the page's own angle
// brackets carry the placeholder.
func TestParseManHTMLReadsGeneratedPages(t *testing.T) {
	options := ParseManHTML(loadFixture(t, "mandoc-git-log.html"))
	if len(options) < 80 {
		t.Fatalf("read %d options, want the section's option list", len(options))
	}

	index := byFlag(options)
	count, ok := index["--max-count"]
	if !ok {
		t.Fatalf("could not find --max-count in %d options", len(options))
	}
	if index["-n"].Arg != count.Arg {
		t.Errorf("-n and --max-count are one option: %q and %q", index["-n"].Arg, count.Arg)
	}
	if count.Arg != "number" {
		t.Errorf("--max-count arg = %q, want number", count.Arg)
	}
	if !strings.Contains(count.Desc, "Limit the number of commits") {
		t.Errorf("--max-count desc = %q, want the count description", count.Desc)
	}
	if got := index["--expand-tabs"].Arg; got != "n" {
		t.Errorf("--expand-tabs arg = %q, want n: the page writes it as &lt;n&gt;", got)
	}
}

// TestParseManHTMLReadsTheSummary covers the other thing a manual is read for:
// the one line that says what a program does, which the model sees when it
// chooses between programs. The fixture starts at the options and has no NAME
// section, so the shape is written out here.
func TestParseManHTMLReadsTheSummary(t *testing.T) {
	src := `<section class="Sh">
<h1 class="Sh" id="NAME"><a class="permalink" href="#NAME">NAME</a></h1>
<p class="Pp"><code class="Nm">ls</code> &#x2014; <span class="Nd">list
    directory contents</span></p>
</section>`
	got := summaryOf(plainTextOf(src))
	if !strings.Contains(got, "list directory contents") {
		t.Errorf("summary = %q, want the NAME section", got)
	}
	if strings.Contains(got, "NAME") {
		t.Errorf("summary = %q, want the description and not the heading", got)
	}
}

// TestLoadManualFallsBackToTheTextDevice is the machine without mandoc: the
// reader it has left has to keep working.
func TestLoadManualFallsBackToTheTextDevice(t *testing.T) {
	renderer := manRenderer
	manRenderer = func() string { return "" }
	defer func() { manRenderer = renderer }()

	docs := loadManual("ls")
	if len(docs.Options) == 0 {
		t.Skipf("this machine cannot read the ls manual at all: %+v", docs)
	}
	if docs.Source != "man" {
		t.Errorf("source = %q, want man", docs.Source)
	}
	if _, ok := byFlag(docs.Options)["-l"]; !ok {
		t.Errorf("the text device read %d options and none of them is -l", len(docs.Options))
	}
}

// TestReadManHTMLReadsTheWholePage is the integration half: the fixtures are
// sections, and this reads what the layer actually reads.
func TestReadManHTMLReadsTheWholePage(t *testing.T) {
	if manRenderer() == "" {
		t.Skip("this machine has no mandoc")
	}
	docs, ok := readManHTML("git-log")
	if !ok {
		t.Skip("the git log manual could not be rendered here")
	}
	if docs.Bytes == 0 || docs.Summary == "" {
		t.Errorf("read %d bytes and the summary %q", docs.Bytes, docs.Summary)
	}
	count, ok := byFlag(docs.Options)["--max-count"]
	if !ok {
		t.Fatalf("read %d options and none of them takes a count", len(docs.Options))
	}
	if count.Arg == "" {
		t.Error("--max-count was read without the value it takes")
	}

	// The NAME section, which is where the tool summary comes from, is only in
	// the whole page: the fixtures are cut at the options.
	ls, ok := readManHTML("ls")
	if !ok {
		t.Skip("the ls manual could not be rendered here")
	}
	if !strings.Contains(ls.Summary, "list directory contents") {
		t.Errorf("ls summary = %q, want its NAME section", ls.Summary)
	}
}

func TestSynopsisSuppliesRequiredOptionArgument(t *testing.T) {
	src := `<section><h1 id="SYNOPSIS">SYNOPSIS</h1>
 <code class="Fl">-o</code> <var class="Ar">fmt</var></section>
 <dl><dt><code class="Fl">-o</code></dt><dd>Display the specified columns.</dd>
 <dt><code class="Fl">-f</code></dt><dd>Display a full listing.</dd></dl>`
	options := ParseManHTML(src)
	index := byFlag(options)
	if index["-o"].Arg != "fmt" {
		t.Fatalf("lost required format: %+v", index["-o"])
	}
	for _, candidate := range Candidates(Docs{Options: options}, "show every process", NamedValues{}, 0) {
		if candidate.Key() == "-o" {
			t.Fatal("offered -o without its format")
		}
	}
}
