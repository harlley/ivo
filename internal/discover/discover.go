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
	"strings"
	"time"

	"github.com/harlleyoliveira/jev-cli/internal/run"
)

// Option is one option a program documents about itself.
type Option struct {
	// Flags are the spellings, shortest first: ["-l"], or ["-a", "--all"].
	Flags []string
	// Arg is the placeholder when the option takes a value, such as "PATTERN".
	// Empty means the option stands on its own.
	Arg string
	// Desc is the program's own description, flattened into one paragraph.
	Desc string
}

// Bool reports whether the option can be used on its own, with no value.
func (o Option) Bool() bool { return o.Arg == "" }

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
	Options []Option
}

// minHelpOptions is how many options --help has to document before it is
// trusted. Below it the manual is read instead: plenty of programs answer
// --help with a one line usage stub, and a stub is not documentation.
const minHelpOptions = 5

// chooseSource picks between the two documentation sources.
//
// --help comes first on purpose. It is written for exactly this purpose, it is
// a fraction of the length of a manual page, and reading it is one cheap
// command. The manual is the fallback for the programs that only have one,
// which on macOS includes ls: BSD tools answer --help with an error and a usage
// line.
func chooseSource(help, manual Docs, helpOK, manualOK bool) (Docs, bool) {
	switch {
	case helpOK && len(help.Options) >= minHelpOptions:
		return help, true
	case manualOK:
		return manual, true
	case helpOK:
		return help, true
	}
	return Docs{}, false
}

// Load reads a program's documentation, preferring its --help output and
// falling back to its manual page. The result is cached on disk, because
// spawning a program to read its documentation on every invocation would be
// wasteful and the answer rarely changes.
func Load(program string) (Docs, error) {
	if cached, ok := readCache(program); ok {
		return cached, nil
	}

	help := Docs{Program: program, Source: "help"}
	if text, err := output(program, "--help"); err == nil {
		help.Options = ParseHelp(text)
		help.Bytes = len(text)
		help.Summary = summaryOf(text)
	}

	manual := Docs{Program: program, Source: "man"}
	if text, err := output("man", "-P", "cat", program); err == nil {
		manual.Options = ParseMan(text)
		manual.Bytes = len(text)
		manual.Summary = summaryOf(text)
	}

	docs, ok := chooseSource(help, manual, len(help.Options) > 0, len(manual.Options) > 0)
	if !ok {
		return Docs{}, fmt.Errorf("%s documents no options this layer can read", program)
	}
	writeCache(docs)
	return docs, nil
}

func output(program string, args ...string) (string, error) {
	cmd := exec.Command(program, args...)
	raw, err := cmd.Output()
	if err != nil {
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

		if indent == 5 && strings.HasPrefix(strings.TrimSpace(trimmed), "-") {
			flush()
			spec, desc := splitSpec(strings.TrimSpace(trimmed))
			flags, arg := parseSpec(spec)
			if len(flags) == 0 {
				continue
			}
			current = &Option{Flags: flags, Arg: arg, Desc: desc}
			continue
		}
		if current != nil && indent >= 9 {
			if current.Desc != "" {
				current.Desc += " "
			}
			current.Desc += strings.TrimSpace(trimmed)
		}
	}
	flush()
	return options
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
		if idx := strings.Index(part, "="); idx > 0 {
			flags = append(flags, strings.TrimSpace(part[:idx]))
			if value := strings.TrimSpace(part[idx+1:]); value != "" {
				arg = value
			}
			continue
		}
		// "-D format" documents a value in the same breath as the flag.
		if idx := strings.Index(part, " "); idx > 0 {
			flags = append(flags, strings.TrimSpace(part[:idx]))
			if value := strings.TrimSpace(part[idx+1:]); value != "" {
				arg = value
			}
			continue
		}
		flags = append(flags, part)
	}
	return flags, arg
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

// ---------------------------------------------------------------------------
// the words a request is made of
// ---------------------------------------------------------------------------

// ContentWords keeps the words of a phrase that carry meaning, dropping the
// ones every phrase contains. The phrase is not assumed to be English, so the
// list covers Portuguese as well.
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

// stopwords is the only non-English data in this codebase, and it is here so a
// Portuguese phrase is filtered as cleanly as an English one.
var stopwords = map[string]bool{
	"as": true, "os": true, "de": true, "do": true, "da": true, "dos": true, "das": true,
	"em": true, "no": true, "na": true, "nos": true, "nas": true, "um": true, "uma": true,
	"que": true, "qual": true, "quais": true, "para": true, "por": true, "com": true,
	"sem": true, "ou": true, "se": true, "meu": true, "minha": true, "meus": true,
	"minhas": true, "este": true, "esta": true, "esse": true, "essa": true, "isso": true,
	"nesse": true, "nessa": true, "neste": true, "nesta": true, "desse": true,
	"dessa": true, "aquele": true, "aquela": true, "seu": true, "sua": true, "seus": true,
	"suas": true, "ser": true, "sao": true, "são": true, "foi": true, "tem": true,
	"ter": true, "mais": true, "muito": true, "pouco": true, "nao": true, "não": true,
	"sim": true, "ja": true, "já": true, "ate": true, "até": true, "sobre": true,
	"entre": true, "depois": true, "antes": true, "aqui": true, "ali": true,
	"liste": true, "listar": true, "mostre": true, "mostrar": true, "procure": true,
	"procurar": true, "busque": true, "buscar": true, "encontre": true, "encontrar": true,
	"quero": true, "queria": true, "veja": true, "conte": true, "contar": true,
	"quantas": true, "quantos": true, "quanto": true, "quanta": true, "onde": true,
	"como": true, "quando": true, "arquivo": true, "arquivos": true, "diretorio": true,
	"diretório": true, "pasta": true, "pastas": true, "todos": true, "todas": true,
	"todo": true, "toda": true,
	"the": true, "an": true, "of": true, "in": true, "on": true, "at": true, "to": true,
	"for": true, "with": true, "and": true, "is": true, "are": true, "this": true,
	"that": true, "these": true, "those": true, "me": true, "my": true, "show": true,
	"list": true, "find": true, "search": true, "grep": true, "count": true, "what": true,
	"which": true, "where": true, "when": true, "how": true, "all": true, "file": true,
	"files": true, "folder": true, "folders": true, "directory": true, "here": true,
	"please": true, "give": true, "print": true, "cat": true, "does": true,
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
	return filepath.Join(dir, "jev", "docs", program+".json")
}

// summaryOf takes the first line that says something, which is how a program
// introduces itself in its help or its manual. Short usage lines are skipped:
// "usage: zed" says less than nothing.
func summaryOf(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 12 || strings.HasPrefix(strings.ToLower(line), "usage") {
			continue
		}
		return line
	}
	return ""
}

// cacheVersion invalidates entries written by an older reader.
const cacheVersion = 3

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
	if err := json.Unmarshal(raw, &entry); err != nil || entry.Version != cacheVersion || len(entry.Options) == 0 {
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
