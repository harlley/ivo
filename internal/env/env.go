// Package env probes the machine jev-cli is running on and turns that into the
// small, filtered `state` the model sees, plus the closed sets of candidate
// values the model is allowed to choose from.
//
// Two rules drive everything here:
//
//   - The model can only *choose*, never *produce*. So every string that ends
//     up in a command line has to be found here, in code, and offered as an
//     option: paths that `stat` confirms, patterns and terms matched by regex.
//   - `state` is not treated as hostile by the model, and extra context costs
//     accuracy (context rot). So we send a filtered view, not a dump.
package env

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// Limits on how much context we are willing to put in front of the model.
const (
	// MaxPathCandidates caps the paths lifted out of the request text.
	MaxPathCandidates = 24
	// MaxPatternCandidates caps the filename patterns lifted out of the text.
	MaxPatternCandidates = 24
	// MaxTermCandidates caps the search terms lifted out of the text.
	MaxTermCandidates = 24
)

// Entry is one item in the working directory. Only what a question could need:
// no sizes, no timestamps, no ownership.
type Entry struct {
	Name    string
	IsDir   bool
	Symlink bool
}

// Candidates are the verbatim spans code found in the user's request. Every
// one of them is a string we are willing to pass to a program unchanged.
type Candidates struct {
	// Paths are tokens that exist on disk (or are anchored with "/" or "~" and
	// resolve to something that does).
	Paths []string
	// Patterns are filename globs: explicit "*.go"-style tokens, plus the
	// extensions the request mentions.
	Patterns []string
	// Terms are free text the user seems to want to search for.
	Terms []string
}

// Env is everything jev-cli knows about the current invocation before it asks
// the model anything.
type Env struct {
	Request string
	OS      string
	CWD     string
	Home    string
	Shell   string

	Entries    []Entry
	EntryCount int
	Truncated  bool
	// EntriesError is set when the working directory could not be read. The
	// CLI still works (absolute paths may be usable), so this is data, not a
	// fatal error.
	EntriesError string

	Bins map[string]bool

	Candidates Candidates
}

// Has reports whether a binary was found on PATH.
func (e *Env) Has(bin string) bool { return e.Bins[bin] }

// ProbeOptions configures Probe.
type ProbeOptions struct {
	// Request is the raw natural-language phrase, verbatim. It is the only
	// untrusted input in the whole pipeline and it goes into `state` as-is.
	Request string
	// MaxEntries caps how many directory entries go into the state.
	MaxEntries int
	// Binaries are the programs the catalog might need, to be looked up on
	// PATH.
	Binaries []string
}

// Probe inspects the machine. It never fails on a missing directory listing or
// a missing git repository: those are just empty facts.
func Probe(opts ProbeOptions) (*Env, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	e := &Env{
		Request: strings.TrimSpace(opts.Request),
		OS:      runtime.GOOS,
		CWD:     cwd,
		Home:    home,
		Shell:   shell,
		Bins:    probeBins(opts.Binaries),
	}

	e.Entries, e.EntryCount, e.Truncated, e.EntriesError = readEntries(cwd, opts.MaxEntries)
	e.Candidates = extractCandidates(e.Request, cwd, home)

	return e, nil
}

func probeBins(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		_, err := exec.LookPath(n)
		out[n] = err == nil
	}
	return out
}

// readEntries returns a filtered, ordered view of a directory: subdirectories
// first, then files, each alphabetically. Ordering is ours, not the model's,
// so the option list is stable across invocations.
func readEntries(dir string, max int) (entries []Entry, total int, truncated bool, errMsg string) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, false, err.Error()
	}
	total = len(des)
	all := make([]Entry, 0, len(des))
	for _, de := range des {
		all = append(all, Entry{
			Name:    de.Name(),
			IsDir:   de.IsDir(),
			Symlink: de.Type()&os.ModeSymlink != 0,
		})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].IsDir != all[j].IsDir {
			return all[i].IsDir
		}
		return all[i].Name < all[j].Name
	})
	if max > 0 && len(all) > max {
		return all[:max], total, true, ""
	}
	return all, total, false, ""
}

// probeGit walks up looking for a .git directory before spawning git, so the
// common non-repo case costs nothing.
var (
	quotedRe = regexp.MustCompile("\"([^\"]+)\"|'([^']+)'|`([^`]+)`")
	extRe    = regexp.MustCompile(`(?:^|[\s,;:(])(\.[A-Za-z][A-Za-z0-9]{0,7})\b`)
	capsRe   = regexp.MustCompile(`\b[A-Z][A-Z0-9_]{2,}\b`)
	globRe   = regexp.MustCompile(`\*[^\s,;:]*`)
)

// extractCandidates over-finds spans in the request. Over-finding is the point:
// the model's job is to pick the right one, and a span it was never offered is
// a span it cannot pick. Nothing here is trusted yet, every path is confirmed
// with stat before it becomes an option.
func extractCandidates(request, cwd, home string) Candidates {
	var paths, patterns, terms []string
	addPath := func(p string) { paths = appendUnique(paths, p, MaxPathCandidates) }
	addPattern := func(p string) { patterns = appendUnique(patterns, p, MaxPatternCandidates) }
	addTerm := func(t string) { terms = appendUnique(terms, t, MaxTermCandidates) }

	// 1. Quoted spans are the strongest signal a human gave us: "a.go", 'TODO'.
	for _, m := range quotedRe.FindAllStringSubmatch(request, -1) {
		for _, g := range m[1:] {
			g = strings.TrimSpace(g)
			if g == "" {
				continue
			}
			addTerm(g)
			if p, ok := resolveExisting(g, cwd, home); ok {
				addPath(p)
			}
		}
	}

	// 2. Word-shaped tokens.
	for _, tok := range strings.Fields(request) {
		tok = strings.Trim(tok, "\"'`.,;:!?()[]{}<>")
		if tok == "" {
			continue
		}
		if strings.Contains(tok, "*") {
			addPattern(tok)
		}
		if p, ok := resolveExisting(tok, cwd, home); ok {
			addPath(p)
		} else if looksLikePath(tok) {
			// Anchored but missing on disk. Offering it would produce a
			// command that cannot run, so we do not.
			continue
		}
		if capsRe.MatchString(tok) {
			addTerm(tok)
		}
	}

	// 3. Extensions named anywhere, turned into globs: ".go" -> "*.go".
	for _, m := range extRe.FindAllStringSubmatch(request, -1) {
		addPattern("*" + m[1])
	}

	// 4. Whole globs.
	for _, g := range globRe.FindAllString(request, -1) {
		addPattern(g)
	}

	// 4b. Words that name a kind of file: "go files" -> "*.go". The
	//     extension may not exist in this directory yet, and that is fine.
	//     the user asked for it, and the model can reject the option.
	for _, tok := range strings.Fields(request) {
		word := strings.ToLower(strings.Trim(tok, "\"'`.,;:!?()[]{}<>"))
		if ext, ok := extensionWords[word]; ok {
			addPattern("*" + ext)
		}
	}

	// 5. Fallback: content words. Over-finding is deliberate. The model's job
	//    is to pick the right one, and a word it was never offered is a word it
	//    cannot pick, but a short option list that omits the answer entirely
	//    makes the command unusable. Without this pass, "where does panic
	//    appear in this project" would have nothing to offer as a search term.
	if len(terms) < MaxTermCandidates {
		for _, tok := range strings.Fields(request) {
			word := strings.Trim(tok, "\"'`.,;:!?()[]{}<>*")
			if len([]rune(word)) < 3 || stopwords[strings.ToLower(word)] {
				continue
			}
			addTerm(word)
		}
	}

	return Candidates{Paths: paths, Patterns: patterns, Terms: terms}
}

// extensionWords maps a word a request might use for a kind of file onto the
// glob that selects it. Over-finding is deliberate: a word wrongly added costs
// one option, and the model gets to say none of them fit.
var extensionWords = map[string]string{
	"go": ".go", "golang": ".go", "python": ".py", "py": ".py", "javascript": ".js",
	"js": ".js", "typescript": ".ts", "ts": ".ts", "markdown": ".md", "md": ".md",
	"json": ".json", "yaml": ".yaml", "yml": ".yaml", "toml": ".toml", "text": ".txt",
	"texto": ".txt", "txt": ".txt", "shell": ".sh", "sh": ".sh", "bash": ".sh",
	"html": ".html", "css": ".css", "sql": ".sql", "rust": ".rs", "rs": ".rs",
	"java": ".java", "ruby": ".rb", "rb": ".rb", "php": ".php", "c": ".c",
	"cpp": ".cpp", "csv": ".csv", "xml": ".xml", "mod": ".mod",
}

// stopwords keeps the fallback pass from filling the option list with the
// words every request contains. It is deliberately short: a word wrongly kept
// costs one option, while a word wrongly dropped can cost the whole command.
//
// It holds English and Portuguese words because the phrase the user types is
// not assumed to be English. Along with extensionWords below, this is the only
// non-English data in the codebase, and it exists so that a Portuguese phrase
// reaches the model with the same quality as an English one.
var stopwords = map[string]bool{
	// Portuguese function words and the verbs requests usually open with.
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
	"entre": true, "depois": true, "antes": true,
	"aqui": true, "ali": true, "liste": true, "listar": true, "mostre": true,
	"mostrar": true, "procure": true, "procurar": true, "busque": true, "buscar": true,
	"encontre": true, "encontrar": true, "quero": true, "queria": true, "veja": true,
	"conte": true, "contar": true, "quantas": true, "quantos": true, "quanto": true,
	"quanta": true, "onde": true,
	"como": true, "quando": true, "arquivo": true, "arquivos": true, "diretorio": true,
	"diretório": true, "pasta": true, "pastas": true, "todos": true, "todas": true,
	"todo": true, "toda": true,
	// English function words and command verbs.
	"the": true, "an": true, "of": true, "in": true, "on": true, "at": true, "to": true,
	"for": true, "with": true, "and": true, "is": true, "are": true, "this": true,
	"that": true, "these": true, "those": true, "me": true, "my": true, "show": true,
	"list": true, "find": true, "search": true, "grep": true, "count": true, "what": true,
	"which": true, "where": true, "when": true, "how": true, "all": true, "file": true,
	"files": true, "folder": true, "folders": true, "directory": true, "here": true,
	"please": true, "give": true, "print": true, "cat": true,
}

// resolveExisting turns a token into a path we can vouch for, or reports that
// we cannot. Relative tokens are resolved against the working directory.
func resolveExisting(tok, cwd, home string) (string, bool) {
	if tok == "" || strings.ContainsAny(tok, "*?[]$") {
		return "", false
	}
	candidate := tok
	if strings.HasPrefix(tok, "~/") && home != "" {
		candidate = filepath.Join(home, tok[2:])
	}
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(cwd, candidate)
	}
	if _, err := os.Stat(candidate); err != nil {
		return "", false
	}
	// Return the token as the user wrote it: verbatim, so the command line
	// matches what they asked for and what they can read.
	return tok, true
}

func looksLikePath(tok string) bool {
	return strings.Contains(tok, "/") || strings.HasPrefix(tok, "~") || strings.HasPrefix(tok, ".")
}

func appendUnique(list []string, v string, max int) []string {
	if v == "" || len(list) >= max {
		return list
	}
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// EntryNames renders the directory listing for `state`: subdirectories get a
// trailing slash so the model can tell them apart without extra fields.
func EntryNames(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name
		if e.IsDir {
			name += "/"
		}
		out = append(out, name)
	}
	return out
}
