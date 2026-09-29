package discover

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// NameIndex is what this machine says its own commands do: one line per
// command, taken from the NAME section of its manual page, which is the one
// section every page is required to have.
//
// It exists because the machine's own index does not. macOS ships no whatis
// database, so apropos there matches loose words against a partial list: a two
// word query comes back with hundreds of hits, no ranking, and the command that
// answers the question is not among the first forty. Reading the pages directly
// costs a fraction of a second and answers the question the discovery stage is
// really asking: which installed command is documented as doing this.
type NameIndex map[string]string

// nameIndexVersion invalidates an index written by an older reader.
const nameIndexVersion = 1

// nameIndexTTL is how long an index is trusted. A machine installs a program
// and its page together, so a week is a safe lag, and rebuilding early is only
// a fraction of a second.
const nameIndexTTL = 7 * 24 * time.Hour

// namePreviewBytes is how much of a page is read to find its NAME section. The
// section is at the top of every manual page; a page that needs more than this
// is not read at all.
const namePreviewBytes = 8192

// LoadNameIndex reads the cached index, and builds it when it is missing or
// old. A build that fails leaves an empty index, which costs the tier and
// nothing else.
func LoadNameIndex() NameIndex {
	if cached, ok := readNameIndex(); ok {
		return cached
	}
	built := BuildNameIndex()
	writeNameIndex(built)
	return built
}

// BuildNameIndex reads the NAME section of every command manual page this
// machine has. Only sections one and eight are read, because those are the
// commands, and a machine has thousands of pages in the library sections that
// no one types at a shell.
func BuildNameIndex() NameIndex {
	index := NameIndex{}
	for _, dir := range manPathDirs() {
		for _, section := range []string{"man1", "man8"} {
			entries, err := os.ReadDir(filepath.Join(dir, section))
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				name := commandNameOf(entry.Name(), section)
				if name == "" {
					continue
				}
				if _, seen := index[name]; seen {
					continue
				}
				if desc := describePage(filepath.Join(dir, section, entry.Name())); desc != "" {
					index[name] = desc
				}
			}
		}
	}
	return index
}

// commandNameOf turns a page file name into the command it documents, or ""
// when the name does not look like a command page. A trailing section suffix is
// what makes the difference between a command and a configuration file.
func commandNameOf(file, section string) string {
	for _, suffix := range []string{"." + strings.TrimPrefix(section, "man") + ".gz", "." + strings.TrimPrefix(section, "man")} {
		if strings.HasSuffix(file, suffix) {
			name := strings.TrimSuffix(file, suffix)
			if name == "" || strings.ContainsAny(name, "/ ") {
				return ""
			}
			return name
		}
	}
	return ""
}

// describePage pulls the one line description out of a page, or "" when the
// page has no NAME section this reader understands.
//
// Three shapes matter and they are the three this project keeps meeting: mdoc,
// which describes the command with .Nd; the man macros, which write
// "name - description" as text; and a generated page, which writes the same
// text with escaped dashes.
func describePage(path string) string {
	head, err := readPageHead(path)
	if err != nil {
		return ""
	}
	if match := ndLine.FindSubmatch(head); match != nil {
		return cleanRoffText(string(match[1]))
	}
	section := shNameLine.FindIndex(head)
	if section == nil {
		return ""
	}
	// The first element is the remainder of the NAME line itself, and the
	// description follows within a few lines. Reading a bounded window keeps a
	// page whose NAME section is malformed from walking the whole file.
	lines := strings.Split(string(head[section[1]:]), "\n")
	if len(lines) > 1 {
		lines = lines[1:]
	}
	if len(lines) > 5 {
		lines = lines[:5]
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ".") {
			continue
		}
		text := cleanRoffText(line)
		if idx := strings.Index(text, " - "); idx >= 0 {
			return strings.TrimSpace(text[idx+3:])
		}
		return text
	}
	return ""
}

func readPageHead(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var reader io.Reader = file
	if strings.HasSuffix(path, ".gz") {
		zipped, err := gzip.NewReader(file)
		if err != nil {
			return nil, err
		}
		defer zipped.Close()
		reader = zipped
	}
	return io.ReadAll(io.LimitReader(reader, namePreviewBytes))
}

var (
	shNameLine = regexp.MustCompile(`(?m)^\.S[HS]\s+"?NAME"?\s*$`)
	ndLine     = regexp.MustCompile(`(?m)^\.Nd\s+(.+)$`)
	roffFont   = regexp.MustCompile(`\\f.`)
	roffEscape = regexp.MustCompile(`\\\(em|\\\(en|\\-`)
)

// cleanRoffText turns the source of a line into the text it renders to.
func cleanRoffText(line string) string {
	line = roffEscape.ReplaceAllString(line, "-")
	line = roffFont.ReplaceAllString(line, "")
	line = strings.NewReplacer(`\&`, "", `\"`, "", `\(aq`, "'", `\(dq`, `"`).Replace(line)
	return strings.Join(strings.Fields(line), " ")
}

// Shortlist ranks the commands whose own summary matches the words of the
// request. It is a recall step and not a decision: the model chooses among the
// result, so a loose ranking is enough, and whatever the ranking misses is
// still reachable through the tier below it.
func (index NameIndex) Shortlist(request string, exists func(string) bool, limit int) []string {
	words := indexWords(request)
	if len(words) == 0 || len(index) == 0 {
		return nil
	}
	type scored struct {
		name  string
		score int
	}
	ranked := make([]scored, 0, 32)
	for name, desc := range index {
		if exists != nil && !exists(name) {
			continue
		}
		lowered := strings.ToLower(name + " " + desc)
		score := 0
		for _, word := range words {
			loweredWord := strings.ToLower(word)
			if !strings.Contains(lowered, loweredWord) {
				continue
			}
			score++
			if strings.Contains(strings.ToLower(name), loweredWord) {
				score++
			}
		}
		if score > 0 {
			ranked = append(ranked, scored{name, score})
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score == ranked[j].score {
			return ranked[i].name < ranked[j].name
		}
		return ranked[i].score > ranked[j].score
	})
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	names := make([]string, 0, len(ranked))
	for _, entry := range ranked {
		names = append(names, entry.name)
	}
	return names
}

// indexWords splits a request into the words worth matching against a command's
// own summary.
//
// It is deliberately not ContentWords. That list drops the words that are noise
// inside a search term, and the noise includes the nouns of the domain itself:
// file, files, directory, list, show, search, find. Those are exactly the words
// that say what a command is for, and dropping them leaves "list the files in
// this directory" with nothing to match, so the tier finds nothing at all. The
// right filter depends on what the words are for, and these words are for
// matching a purpose.
func indexWords(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, field := range strings.Fields(text) {
		word := strings.ToLower(strings.Trim(field, "\"'`.,;:!?()[]{}<>*"))
		if len([]rune(word)) < 3 || seen[word] || indexStopwords[word] {
			continue
		}
		seen[word] = true
		out = append(out, word)
	}
	return out
}

// indexStopwords are the function words of a sentence, in both languages the
// layer accepts. They carry no purpose to match against, which is the only test
// a word has to pass to be dropped here.
var indexStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"these": true, "those": true, "from": true, "into": true, "onto": true,
	"are": true, "was": true, "were": true, "does": true, "did": true, "how": true,
	"what": true, "which": true, "where": true, "when": true, "who": true, "why": true,
	"much": true, "many": true, "some": true, "any": true, "every": true, "its": true,
	"it": true, "is": true, "be": true, "been": true, "am": true, "you": true,
	"your": true, "my": true, "me": true, "our": true, "their": true, "there": true,
	"here": true, "please": true, "want": true, "need": true, "like": true,
	"all": true, "can": true, "could": true, "would": true, "should": true,
	"para": true, "por": true, "com": true, "sem": true, "que": true, "qual": true,
	"quais": true, "dos": true, "das": true, "uma": true, "um": true, "no": true,
	"na": true, "nos": true, "nas": true, "em": true, "de": true, "do": true,
	"da": true, "os": true, "as": true, "se": true, "meu": true, "minha": true,
	"este": true, "esta": true, "esse": true, "essa": true, "isso": true,
}

// manPathDirs asks man where its pages are. manpath is the program that knows,
// and a machine without it falls back to the directories every Unix has.
func manPathDirs() []string {
	raw := ""
	if text, err := output("manpath"); err == nil {
		raw = text
	}
	if strings.TrimSpace(raw) == "" {
		raw = os.Getenv("MANPATH")
	}
	if strings.TrimSpace(raw) == "" {
		raw = "/usr/share/man:/usr/local/share/man:/opt/homebrew/share/man"
	}
	var dirs []string
	for _, dir := range strings.Split(raw, ":") {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// nameIndexFile is what the index looks like on disk.
type nameIndexFile struct {
	Version int
	BuiltAt time.Time
	Names   map[string]string
}

func nameIndexPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "ivo", "names.json")
}

func readNameIndex() (NameIndex, bool) {
	path := nameIndexPath()
	if path == "" {
		return nil, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var stored nameIndexFile
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, false
	}
	if stored.Version != nameIndexVersion || len(stored.Names) == 0 {
		return nil, false
	}
	if time.Since(stored.BuiltAt) > nameIndexTTL {
		return nil, false
	}
	return stored.Names, true
}

func writeNameIndex(index NameIndex) {
	path := nameIndexPath()
	if path == "" || len(index) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	raw, err := json.Marshal(nameIndexFile{
		Version: nameIndexVersion,
		BuiltAt: time.Now(),
		Names:   index,
	})
	if err != nil {
		return
	}
	_ = os.WriteFile(path, raw, 0o644)
}
