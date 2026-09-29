// Package discover reads a program's own documentation and turns it into
// candidate options.
//
// A hand written catalog does not scale: every flag has to be typed out, and
// the wording of the question the model sees is the wording we invented, which
// is where wrong answers come from. A program already documents itself, in its
// --help output or its manual page, so the layer reads that instead.
//
// The guarantee is unchanged. Code extracts the candidates, so every option is
// a literal token the program itself documents; the model only picks among
// them, and it never writes one.
package discover

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/harlley/ivo/internal/run"
)

// Option is one option a program documents about itself, or, when it names a
// subcommand, one command that program offers.
type Option struct {
	// AfterOperand identifies expression predicates following the positional input.
	AfterOperand bool
	// Values are literal argument choices documented by the program.
	Values []OptionValue
	// Flags are the spellings, shortest first: ["-l"], or ["-a", "--all"].
	Flags []string
	// Argv is what choosing this contributes to the command line, which is the
	// flag alone for a boolean option and the flag with its value for one that
	// takes a value.
	Argv []string
	// Arg is the placeholder when the option takes a value, such as "PATTERN".
	// Empty means the option stands on its own.
	Arg string
	// Desc is the program's own description, flattened into one paragraph.
	Desc string
}

type OptionValue struct {
	Value string
	Desc  string
}

// Key is the identifier a question uses for this option: the tokens themselves,
// so two values of the same flag are two distinct answers.
func (o Option) Key() string { return strings.Join(o.Argv, " ") }

// ConflictsWith prevents duplicate spellings or values of a non-repeatable option.
// Explicitly repeatable options retain the repetitions their manuals permit.
func (o Option) ConflictsWith(chosen []string) bool {
	desc := strings.ToLower(o.Desc)
	repeatable := strings.Contains(desc, "multiple") || strings.Contains(desc, "repeat") || strings.Contains(desc, "more than once") || strings.Contains(desc, "more than one")
	for _, key := range chosen {
		if key == o.Key() && !repeatable {
			return true
		}
		if repeatable {
			continue
		}
		for _, flag := range o.Flags {
			if key == flag || strings.HasPrefix(key, flag+" ") {
				return true
			}
		}
	}
	return false
}

// Subcommand is one command a program offers, as the program itself lists it.
type Subcommand struct {
	Name string
	Desc string
}

// Bool reports whether the option can be used on its own, with no value.
func (o Option) Bool() bool { return o.Arg == "" }

// Numeric reports a documented numeric placeholder, not a program-specific rule.
func (o Option) Numeric() bool { return valueKind(o.Arg) == kindCount }

// Name is the identifier a question can be built on: the longest spelling,
// without its dashes.
func (o Option) Name() string {
	chosen := ""
	for _, f := range o.Flags {
		if len(f) > len(chosen) {
			chosen = f
		}
	}
	if chosen == "" {
		return ""
	}
	return strings.TrimLeft(chosen, "-")
}

// Docs is what one program documents.
type Docs struct {
	Program string
	// Source records where the options came from: "help" or "man".
	Source string
	// Bytes is how much text that source took, which is what the source choice
	// is made on: the option list becomes the criteria of the questions the
	// model answers, so a shorter source is a cheaper and less noisy one.
	Bytes int
	// Summary is the first thing the source says about the program, which is
	// what the model sees when choosing between programs.
	Summary string
	// Detail is a bounded excerpt of the program's own description. A name-line
	// alone often cannot distinguish a live measurement from a hardware report.
	Detail  string
	Options []Option
	// Subcommands are the commands this program offers, when its own help lists
	// them. git is the example that matters: git log is neither an option nor a
	// program of its own.
	Subcommands []Subcommand
}

// chooseSource picks between the two documentation sources. The help output
// comes first: it is written for exactly this purpose, it is a fraction of the
// length of a manual page, and reading it is one cheap command. It keeps that
// place unless the manual documents more, which is what happens when the help
// is a usage summary rather than the list: bsdtar answers --help with thirteen
// options and documents ninety-two in its manual.
//
// A help output counts as documentation when it documents options *or*
// subcommands. Measuring it in options alone was a bug: git --help lists
// twenty-three subcommands and no options at all, so the source was rejected
// and the fallback, a manual page that lists neither, was used instead. For the
// same reason the manual only takes over when the help has no commands of its
// own to contribute.
func chooseSource(help, manual Docs) (Docs, bool) {
	if len(manual.Options) > len(help.Options) && len(help.Subcommands) == 0 {
		return manual, true
	}
	if DocumentsAnything(help) {
		return help, true
	}
	if len(manual.Options) > 0 {
		return manual, true
	}
	return Docs{}, false
}

// helpIsThin is where a help output stops looking like the list and starts
// looking like a usage summary, which is the only case that pays for reading a
// manual as well. The bar is low on purpose: the many programs with no --help at
// all already read the manual, and the answer is cached for a week.
const helpIsThin = 20

// thinHelp reports whether a help output documents too little to be the list.
func thinHelp(help Docs) bool { return len(help.Options) < helpIsThin }

// DocumentsAnything reports whether a source is documentation at all: options,
// subcommands, or neither because the program answered with a usage line.
//
// It is exported because the same mistake was made twice: measuring
// documentation in options alone hides every program that documents commands
// instead, and git is one.
func DocumentsAnything(docs Docs) bool {
	return len(docs.Options) > 0 || len(docs.Subcommands) > 0
}

// Load reads a program's documentation, preferring its --help output and
// falling back to its manual page. The result is cached on disk, because
// spawning a program to read its documentation on every invocation would be
// wasteful and the answer rarely changes.
func Load(program string) (Docs, error) {
	if cached, ok := readCache(program); ok {
		return cached, nil
	}

	// "git log" is a command a program offers, not a program, and its help is
	// asked for in that shape.
	if name, sub, found := strings.Cut(program, " "); found {
		docs, err := loadSubcommand(name, strings.TrimSpace(sub))
		if err != nil {
			return Docs{}, err
		}
		writeCache(docs)
		return docs, nil
	}

	help := Docs{Program: program, Source: "help"}
	if text, err := output(program, "--help"); err == nil {
		help.Options = ParseHelp(text)
		help.Subcommands = ParseSubcommands(text)
		help.Bytes = len(text)
		help.Summary = summaryOf(text)
	}

	// The help output wins whenever it documents anything, so the manual is read
	// only when it does not, or when what it documents is thin enough to be a
	// usage summary. Reading it every time cost one command per program, and
	// rendering a manual page is not free.
	manual := Docs{Program: program, Source: "man"}
	if !DocumentsAnything(help) || thinHelp(help) {
		manual = loadManual(program)
	}

	docs, ok := chooseSource(help, manual)
	if !ok {
		return Docs{}, fmt.Errorf("%s documents no options this layer can read", program)
	}
	writeCache(docs)
	return docs, nil
}

// Inspect reads metadata for a program discovered by purpose, without executing
// the candidate. Some programs do not recognize --help and could act instead.
// Even an empty manual is returned: callers must not fall back to running the
// candidate merely because no documentation was found.
func Inspect(program string) Docs {
	if cached, ok := readCache(program); ok {
		if cached.Detail == "" {
			cached.Detail = loadManual(program).Detail
		}
		return cached
	}
	return loadManual(program)
}

func descriptionOf(text string) string {
	// The synopsis specifies positional argument order. Keeping only the prose
	// description can turn a pattern operand into a file operand, for example.
	if start := strings.Index(text, "SYNOPSIS"); start >= 0 {
		text = text[start:]
	} else if start := strings.Index(text, "DESCRIPTION"); start >= 0 {
		text = text[start+len("DESCRIPTION"):]
	}
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > 1500 {
		return string(runes[:1500])
	}
	return text
}

func loadSubcommand(program, sub string) (Docs, error) {
	name := program + " " + sub

	// A command is documented in two places, and they are not equivalent: the
	// compact help is a summary, the manual is the list. git log -h lists five
	// options and its manual lists a hundred and eighty-three, including the
	// one that says how many commits to show.
	//
	// So both are read, and the fuller one wins. That costs one extra command
	// the first time and nothing afterwards, because the result is cached.
	var best Docs
	consider := func(docs Docs) {
		if len(docs.Options) == 0 || len(docs.Options) <= len(best.Options) {
			return
		}
		best = docs
	}
	help := func(source string, text string) Docs {
		return Docs{
			Program: name,
			Source:  source,
			Options: ParseHelp(text),
			Bytes:   len(text),
			Summary: summaryOf(text),
		}
	}
	if text, err := output(program, sub, "-h"); err == nil {
		consider(help("help", text))
	}
	manual := loadManual(program + "-" + sub)
	manual.Program = name
	consider(manual)
	if text, err := output(program, sub, "--help"); err == nil {
		consider(help("help", text))
	}
	if len(best.Options) == 0 {
		return Docs{}, fmt.Errorf("%s documents no options this layer can read", name)
	}
	return best, nil
}

func output(program string, args ...string) (string, error) {
	cmd := exec.Command(program, args...)
	// A pager would wait for a reader that is not there, and capturing its
	// output is not reading. Nothing here is interactive.
	cmd.Env = append(os.Environ(),
		"PAGER=cat", "MANPAGER=cat", "GIT_PAGER=cat", "GH_PAGER=cat", "SYSTEMD_PAGER=cat")
	// Both streams: plenty of programs answer a help flag on stderr, rm and
	// most BSD tools among them, and capturing only stdout made them look
	// undocumented.
	raw, err := cmd.CombinedOutput()
	if err != nil && len(raw) == 0 {
		return "", err
	}
	return string(run.StripOverstrike(raw)), nil
}

// ParseHelp reads GNU style --help output:
//
//	-a, --all                  do not ignore entries starting with .
//	    ...continued on the following, more indented lines...
func ParseHelp(text string) []Option {
	var options []Option
	var current *Option

	flush := func() {
		if current != nil && current.Desc != "" {
			options = append(options, *current)
		}
		current = nil
	}

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimRight(line, " \t")
		if strings.TrimSpace(trimmed) == "" {
			flush()
			continue
		}
		indent := len(trimmed) - len(strings.TrimLeft(trimmed, " "))
		body := strings.TrimSpace(trimmed)

		if indent <= 6 && strings.HasPrefix(body, "-") {
			flush()
			spec, desc := splitSpec(body)
			flags, arg := parseSpec(spec)
			if len(flags) == 0 {
				continue
			}
			current = &Option{Flags: flags, Arg: arg, Desc: desc}
			continue
		}
		if current != nil {
			if current.Desc != "" {
				current.Desc += " "
			}
			current.Desc += body
		}
	}
	flush()
	return options
}

// ParseMan reads mandoc style manual output, which indents the option by five
// spaces and aligns its description in a column:
//
//	-A      Include directory entries whose names begin with a dot.
//	        Continued lines are indented further still.
func ParseMan(text string) []Option {
	// A manual documents its options in one column, and it is not always five:
	// git's pages use seven, ls's use five. Rather than guess, each plausible
	// column is tried and the one that documents the most options wins.
	best := []Option{}
	for _, column := range optionColumns(text) {
		if parsed := parseManAt(text, column); len(parsed) > len(best) {
			best = parsed
		}
	}
	return best
}

// optionColumns lists the indents that could be a manual's option column.
func optionColumns(text string) []int {
	counts := map[int]int{}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimRight(line, " \t")
		body := strings.TrimLeft(trimmed, " ")
		if len(body) < 2 || !strings.HasPrefix(body, "-") {
			continue
		}
		indent := len(trimmed) - len(body)
		if indent >= 2 && indent <= 12 {
			counts[indent]++
		}
	}
	columns := make([]int, 0, len(counts))
	for column := range counts {
		columns = append(columns, column)
	}
	sort.Ints(columns)
	return columns
}

func parseManAt(text string, column int) []Option {
	var options []Option
	var current *Option

	flush := func() {
		if current != nil && current.Desc != "" {
			options = append(options, *current)
		}
		current = nil
	}

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimRight(line, " \t")
		if strings.TrimSpace(trimmed) == "" {
			flush()
			continue
		}
		indent := len(trimmed) - len(strings.TrimLeft(trimmed, " "))

		if indent == column && strings.HasPrefix(strings.TrimSpace(trimmed), "-") {
			flush()
			spec, desc := splitSpec(strings.TrimSpace(trimmed))
			flags, arg := parseSpec(spec)
			if len(flags) == 0 {
				continue
			}
			current = &Option{Flags: flags, Arg: arg, Desc: desc}
			continue
		}
		if current != nil && indent > column {
			if current.Desc != "" {
				current.Desc += " "
			}
			current.Desc += strings.TrimSpace(trimmed)
		}
	}
	flush()
	return options
}

// subcommandLine matches a listed command: a short bare name, indented, followed
// by a description that starts with a capital letter. git prints its commands
// this way ("   clone      Clone a repository into a new directory"), and so do
// docker, cargo and gh.
var subcommandLine = regexp.MustCompile(`^ {2,4}([a-z][a-z0-9-]{1,20}) {2,}([A-Z].*)$`)

// ParseSubcommands reads the command list a program prints in its own help.
func ParseSubcommands(text string) []Subcommand {
	seen := map[string]bool{}
	var out []Subcommand
	for _, line := range strings.Split(text, "\n") {
		match := subcommandLine.FindStringSubmatch(strings.TrimRight(line, " \t"))
		if match == nil {
			continue
		}
		name, desc := match[1], strings.TrimSpace(match[2])
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, Subcommand{Name: name, Desc: FirstSentence(desc)})
	}
	return out
}

// cleanArg keeps the name of a value and drops the punctuation around it: a
// manual writes the placeholder as "<number>" and the name is "number", which is
// what decides whether the value can be filled from the request at all.
func cleanArg(raw string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), "<>[]"))
}

// splitSpec separates an option's spellings from its description, which the two
// formats align with a run of spaces.
func splitSpec(line string) (spec, desc string) {
	for i := 1; i < len(line)-1; i++ {
		if line[i] == ' ' && line[i+1] == ' ' {
			return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i:])
		}
	}
	return strings.TrimSpace(line), ""
}

// parseSpec turns "-a, --all" or "-D format" or "--color=when" into the flag
// spellings and the value placeholder, if any.
func parseSpec(spec string) (flags []string, arg string) {
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "-") {
			continue
		}
		// "git log -h" writes a negatable flag as --[no-]source, which is a
		// family and not a flag: the positive spelling is the one to offer.
		part = strings.ReplaceAll(part, "[no-]", "")

		flag, value := part, ""
		if idx := strings.Index(part, "="); idx > 0 {
			flag, value = part[:idx], part[idx+1:]
		} else if idx := strings.Index(part, " "); idx > 0 {
			flag, value = part[:idx], part[idx+1:]
		}
		flag = strings.TrimSpace(flag)

		// An optional value is written in brackets beside the flag, the way GNU
		// pages write --color[=WHEN]: the flag is what can be typed, and the
		// bracket is not part of it. A lone dash is not a flag either.
		flag = strings.TrimRight(flag, "[]")
		if len(flag) < 2 {
			continue
		}

		// The placeholder test belongs to the flag, not to the whole part. A
		// manual writes the count option as "-<number>, -n <number>,
		// --max-count=<number>": the first is a shape, and the other two are
		// flags whose value happens to be named in the same breath.
		if strings.ContainsAny(flag, "<>") {
			continue
		}
		flags = append(flags, flag)
		if value := cleanArg(value); value != "" {
			arg = value
		}
	}
	return flags, arg
}

// Numbers returns the numbers a request mentions, in order. They are the values
// an option like "-n <count>" can be given without inventing one: the request
// said "the last 3 commits", so 3 is a value this layer already has.
func Numbers(request string) []string {
	seen := map[string]bool{}
	var out []string
	for _, match := range numberPattern.FindAllString(request, -1) {
		if seen[match] {
			continue
		}
		seen[match] = true
		out = append(out, match)
	}
	return out
}

var numberPattern = regexp.MustCompile(`\b\d{1,4}\b`)

// The placeholder shapes this layer can fill from the request. A manual is
// written in one of a handful of vocabularies for these, and a placeholder that
// matches none of them is left alone.
var (
	countKind   = regexp.MustCompile(`^(n|num|number|count|depth|lines|limit|max|min|width|size|seconds|minutes|bytes|kb|mb|gb)$`)
	pathKind    = regexp.MustCompile(`^(file|files|filename|filenames|path|paths|dir|dirs|directory|directories|archive|archives|source|dest|destination|target|infile|outfile|input|output)$`)
	patternKind = regexp.MustCompile(`^(glob|globs|include|includes|exclude|excludes|ignore|filter)$`)
	textKind    = regexp.MustCompile(`^(pattern|patterns|regexp|regex|regexps|expression|expr|string|strings|text|word|words|term|terms|needle|query|search|name)$`)
)

// Candidates returns the options worth asking about, with the exact tokens each
// one contributes.
//
// A boolean option contributes its flag. An option that takes a count
// contributes the flag and one of the numbers the request mentioned, so "-n"
// becomes "-n 3" for a request about the last three commits. An option that
// takes anything else is left out: inventing a value is exactly what this layer
// refuses to do.
func Candidates(docs Docs, request string, values NamedValues, limit int) []Option {
	numbers := Numbers(request)
	var out []Option
	add := func(option Option) {
		if limit > 0 && len(out) >= limit {
			return
		}
		out = append(out, option)
	}
	// One option per value, and only a couple of values per flag: the walk is
	// asked which of these the request needs, and a question with every path in
	// it is the question that stops being answered.
	spread := func(option Option, candidates []string) {
		for i, value := range candidates {
			if i == maxValuesPerFlag {
				return
			}
			valued := option
			valued.Argv = []string{option.Flags[0], value}
			// The description stays the manual's own words. Writing the value
			// into it put the request's own words into a field the relevance
			// filter reads, which made every flag that takes a value look
			// relevant: the offer list filled with `--color git` and
			// `--output changes`, and the narrowing that keeps the question
			// small stopped working. The value is in the key, which is what the
			// model chooses from.
			valued.Desc = option.Desc
			add(valued)
		}
	}

	for _, option := range docs.Options {
		if len(option.Values) > 0 && !option.Numeric() {
			for _, value := range option.Values {
				bound := option
				bound.Argv = []string{option.Flags[0], value.Value}
				bound.Desc = option.Desc + " Value " + value.Value + ": " + value.Desc
				add(bound)
			}
			continue
		}
		if option.Bool() {
			option.Argv = []string{option.Flags[0]}
			add(option)
			continue
		}
		// A flag that takes a value is offered only with a value the code found
		// in the request itself. Inventing one is what this layer refuses to do,
		// and leaving the flag out entirely is what made "grep for timeout" come
		// back without the word to look for.
		switch valueKind(option.Arg) {
		case kindCount:
			spread(option, numbers)
		case kindPath:
			paths := append([]string{}, values.Paths...)
			seenPaths := map[string]bool{}
			for _, path := range paths {
				seenPaths[path] = true
			}
			for _, value := range values.Terms {
				if strings.ContainsAny(value, "/\\") || strings.Contains(value, ".") {
					if !strings.ContainsAny(value, "*? ") && !seenPaths[value] {
						seenPaths[value] = true
						paths = append(paths, value)
					}
				}
			}
			spread(option, paths)
		case kindPattern:
			spread(option, values.Patterns)
		case kindText:
			textValues := append([]string{}, values.Terms...)
			description := strings.ToLower(option.Desc)
			placeholder := strings.ToLower(option.Arg)
			regex := strings.Contains(description, "regular expression") || strings.Contains(description, "regex") || strings.Contains(placeholder, "regex")
			if !regex {
				textValues = append(append([]string{}, values.Patterns...), textValues...)
			}
			spread(option, textValues)
		}
	}
	return out
}

// maxValuesPerFlag bounds how many values of one kind a single flag is offered
// with.
const maxValuesPerFlag = 2

// NamedValues are the tokens the code found in a request: paths that exist here,
// filename patterns, and the words that look like search terms. They are the
// only values this layer will put in a command, because they came from the
// request rather than from the model.
type NamedValues struct {
	Paths    []string
	Patterns []string
	Terms    []string
}

// The kinds of value a documented placeholder stands for. The placeholder is
// the manual's own word for the value, so this classification reads the
// documentation rather than a table of flags: a manual that says FILE means a
// path, one that says PATTERN means something to look for.
const (
	kindNone    = ""
	kindCount   = "count"
	kindPath    = "path"
	kindPattern = "pattern"
	kindText    = "text"
)

// valueKind classifies a placeholder. The punctuation and the spacing of a
// placeholder vary between manuals ("<file>", "FILE", "file ..."), so it is
// normalised first and matched whole.
func valueKind(arg string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(arg), ""))
	normalized = strings.Trim(normalized, ".<>[]")
	switch {
	case countKind.MatchString(strings.SplitN(normalized, "[", 2)[0]):
		return kindCount
	case pathKind.MatchString(normalized):
		return kindPath
	case patternKind.MatchString(normalized):
		return kindPattern
	case textKind.MatchString(normalized):
		return kindText
	}
	return kindNone
}

// SubcommandCandidates turns a program's command list into options, so the same
// walk can choose git log before choosing git log's flags.
func SubcommandCandidates(subcommands []Subcommand, limit int) []Option {
	var out []Option
	for _, sub := range subcommands {
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, Option{
			Flags: []string{sub.Name},
			Argv:  []string{sub.Name},
			Desc:  sub.Desc,
		})
	}
	return out
}

// Relevant keeps the candidates that have something to do with the request,
// with a generous cap.
//
// It is the same narrowing the program filter deliberately avoids, and it is
// right here for the opposite reason: a command like git log documents a hundred
// and fifty flags, and a question offering all of them gets the answer
// "suppress progress reporting" for a request about the last three commits.
// Words in the request that appear in a flag's own description are a real
// signal at this level, and the cap keeps the question small rather than
// complete.
func Relevant(options []Option, request string, limit int) []Option {
	if limit <= 0 || len(options) <= limit {
		return options
	}
	words := ContentWords(request)
	if len(words) == 0 {
		return options[:limit]
	}

	// Order is not a lever here. The options become the criteria of a Choice,
	// which is a JSON object, and Go writes those in key order: the model reads
	// them alphabetically whatever order they were ranked in. What is offered is
	// the lever, so a request that shares words with some options is offered
	// those and nothing else.
	var relevant, rest []Option
	for _, option := range options {
		haystack := strings.ToLower(option.Desc + " " + strings.Join(option.Flags, " "))
		score := 0
		for _, word := range words {
			if strings.Contains(haystack, strings.ToLower(word)) {
				score++
			}
		}
		if score > 0 {
			relevant = append(relevant, option)
			continue
		}
		rest = append(rest, option)
	}

	// Narrowing to a handful is as bad as offering everything: a question with
	// one candidate leaves the model nothing to judge between. The relevant
	// options come first, and the rest fill the question up to the cap.
	const floor = 10
	if len(relevant) < floor {
		relevant = append(relevant, rest...)
	}
	if len(relevant) > limit {
		relevant = relevant[:limit]
	}
	return relevant
}

// Flags returns every boolean option a program documents, in the order its
// documentation lists them.
//
// This deliberately does not pre-select by keyword. A lexical pass looks
// appealing and does not work: BSD ls documents -A as "include directory
// entries whose names begin with a dot", so a request about hidden files shares
// no word with it, and -l is "list in long format" rather than "details".
// Matching meaning against wording is what the model is for.
//
// The cost of asking about an option the request does not need is tokens, not
// accuracy: questions are evaluated in isolation from one another, so an
// irrelevant one cannot drag the relevant ones down.
func Flags(docs Docs, limit int) []Option {
	var out []Option
	for _, option := range docs.Options {
		if !option.Bool() {
			continue
		}
		out = append(out, option)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

// FirstSentence keeps a documented description to a length a question can be
// built on, since some manuals write a paragraph per option.
func FirstSentence(desc string) string {
	if i := strings.Index(desc, ". "); i >= 0 {
		return desc[:i+1]
	}
	return desc
}

// WordCandidates splits a request into the words worth asking about: is this one
// the name of a program?
//
// It is deliberately not ContentWords. That list drops short words and function
// words because they are noise in a search term, and a program name is often
// exactly that: rm, cp, mv, ls, dd, ps and jq are all two characters, and id is
// a word every sentence has. Sending them costs one question each, and the model
// answers them in the same request, so the cost of asking is a few tokens.
func WordCandidates(request string) []string {
	seen := map[string]bool{}
	var out []string
	for _, token := range strings.Fields(request) {
		word := strings.Trim(token, "\"'`,;:!?()[]{}<>*")
		if len([]rune(word)) < 2 || seen[strings.ToLower(word)] {
			continue
		}
		seen[strings.ToLower(word)] = true
		out = append(out, word)
	}
	return out
}

// ---------------------------------------------------------------------------
// the words a request is made of
// ---------------------------------------------------------------------------

// ContentWords keeps the words of a phrase that carry meaning, dropping the
// English function words and common action words.
func ContentWords(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, token := range strings.Fields(text) {
		word := strings.Trim(token, "\"'`.,;:!?()[]{}<>*")
		key := strings.ToLower(word)
		if len([]rune(word)) < 3 || seen[key] || stopwords[key] {
			continue
		}
		// The spelling is kept as written, because a search term is used
		// verbatim; only the stopword test is case insensitive.
		seen[key] = true
		out = append(out, word)
	}
	return out
}

// stopwords removes English function words and common command-request wording.
var stopwords = map[string]bool{
	"the": true, "an": true, "of": true, "in": true, "on": true, "at": true, "to": true,
	"for": true, "with": true, "and": true, "is": true, "are": true, "this": true,
	"that": true, "these": true, "those": true, "me": true, "my": true, "show": true,
	"list": true, "find": true, "search": true, "grep": true, "count": true, "what": true,
	"which": true, "where": true, "when": true, "how": true, "all": true, "file": true,
	"files": true, "folder": true, "folders": true, "directory": true, "here": true,
	"please": true, "give": true, "print": true, "cat": true, "does": true,
	"use": true, "using": true, "used": true, "run": true, "running": true,
}

// ---------------------------------------------------------------------------
// cache
// ---------------------------------------------------------------------------

// cacheTTL is how long a program's documentation is trusted. Documentation
// changes when the program is upgraded, not between two commands.
const cacheTTL = 7 * 24 * time.Hour

func cachePath(program string) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "ivo", "docs", program+".json")
}

// summaryOf takes what a program says it is for.
//
// A manual page opens with a header ("RM(1) General Commands Manual RM(1)") and
// then a NAME section, which is the line worth having: "rm, unlink - remove
// directory entries". Help output usually opens with the description itself.
func summaryOf(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "NAME" {
			continue
		}
		for _, after := range lines[i+1:] {
			after = strings.TrimSpace(after)
			if after == "" {
				continue
			}
			// "rm, unlink - remove directory entries" keeps the description.
			if idx := strings.Index(after, " - "); idx >= 0 {
				return strings.TrimSpace(after[idx+3:])
			}
			return after
		}
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		lowered := strings.ToLower(line)
		if len(line) < 12 || strings.HasPrefix(lowered, "usage") || strings.Contains(lowered, "general commands manual") {
			continue
		}
		return line
	}
	return ""
}

// cacheVersion invalidates entries written by an older reader.
const cacheVersion = 14

func readCache(program string) (Docs, bool) {
	path := cachePath(program)
	if path == "" {
		return Docs{}, false
	}
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) > cacheTTL {
		return Docs{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Docs{}, false
	}
	var entry struct {
		Version int
		Docs
	}
	if err := json.Unmarshal(raw, &entry); err != nil || entry.Version != cacheVersion || !DocumentsAnything(entry.Docs) {
		return Docs{}, false
	}
	return entry.Docs, true
}

func writeCache(docs Docs) {
	path := cachePath(docs.Program)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	raw, err := json.Marshal(struct {
		Version int
		Docs
	}{Version: cacheVersion, Docs: docs})
	if err != nil {
		return
	}
	// A cache that cannot be written is not an error: the docs were read, and
	// the next run simply reads them again.
	_ = os.WriteFile(path, raw, 0o644)
}
