package discover

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Program is one command this machine can run.
type Program struct {
	// Name is the command as a person would type it.
	Name string
	// Summary is what the program says it is for, read from its own
	// documentation. Empty when the documentation was not read, which is the
	// normal case for a candidate that only matched by name.
	Summary string
	// ReadOnly records that the program cannot change anything. It is decided
	// here, in code, and it is what the gate before execution keys on.
	ReadOnly bool
}

// readOnlyPrograms is the set this layer is willing to run without being asked
// twice. Everything else is still offered, and still refused until --allow-write
// says otherwise: the alternative is a catalog that grows by hand again.
var readOnlyPrograms = map[string]bool{
	"ls": true, "eza": true, "exa": true, "tree": true, "find": true, "fd": true,
	"cat": true, "bat": true, "head": true, "tail": true, "less": true, "more": true,
	"rg": true, "grep": true, "ag": true, "ack": true, "wc": true, "nl": true,
	"file": true, "stat": true, "du": true, "df": true, "pwd": true, "realpath": true,
	"sort": true, "uniq": true, "cut": true, "tr": true, "column": true, "jq": true,
	"diff": true, "comm": true, "join": true, "date": true, "cal": true,
	"whoami": true, "id": true, "groups": true, "printenv": true, "uname": true,
	"hostname": true, "ps": true, "uptime": true, "which": true, "type": true,
	"man": true, "whatis": true, "apropos": true, "echo": true, "printf": true,
}

// IsReadOnly reports whether this layer is willing to run a program without
// being asked twice. A program nobody has classified is not.
func IsReadOnly(name string) bool { return readOnlyPrograms[name] }

// Commands lists the commands this shell knows, which is the right universe to
// navigate: what a person can type, not every file in every directory on PATH.
//
// The shell answers this natively and instantly, which matters more than it
// sounds: reading the summaries of two thousand programs took seven seconds,
// while listing their names takes a tenth of one. A shell is used here to
// *list* commands and never to run one.
func Commands() ([]string, error) {
	if cached, ok := readCommandCache(); ok {
		return cached, nil
	}

	names := nativeCommands()
	if len(names) == 0 {
		names = executableNames()
	}
	if len(names) == 0 {
		return nil, os.ErrNotExist
	}
	writeCommandCache(names)
	return names, nil
}

// nativeCommands asks the shell for its command list, in whichever of the two
// spellings this machine's shell understands.
func nativeCommands() []string {
	candidates := [][]string{
		{"bash", "-lc", "compgen -c"},
		{"zsh", "-lc", "print -l -- ${(k)commands}"},
		{"sh", "-lc", "compgen -c"},
	}
	for _, argv := range candidates {
		raw, err := exec.Command(argv[0], argv[1:]...).Output()
		if err != nil || len(raw) == 0 {
			continue
		}
		if names := cleanNames(string(raw)); len(names) > 0 {
			return names
		}
	}
	return nil
}

// executableNames walks PATH, which is the fallback when no shell will answer.
func executableNames() []string {
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".") || entry.IsDir() {
				continue
			}
			if info, err := entry.Info(); err == nil && info.Mode()&0o111 != 0 {
				seen[name] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// cleanNames keeps the usable command names: no builtins that cannot be run as
// a program, no shell keywords, no duplicates.
func cleanNames(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		name := strings.TrimSpace(line)
		if len(name) < 2 || strings.ContainsAny(name, " /\\") || strings.HasPrefix(name, ".") {
			continue
		}
		if seen[name] {
			continue
		}
		// A name the shell knows but the system cannot run is a builtin or a
		// keyword, and there is no program behind it to inspect or execute.
		if _, err := exec.LookPath(name); err != nil {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Retrieve keeps the commands the request names.
//
// This is the first filter, and it is deliberately cheap: the request naming a
// program is the strongest signal there is, and it is what a person does when
// they know the tool ("open this in zed"). Reading two thousand summaries to
// guess at the rest would cost more than it finds, so a program that is not
// named here is reached by asking for it by name, or through the curated tools
// that cover the common verbs.
func Retrieve(commands []string, request string, limit int) []Program {
	lowered := strings.ToLower(request)
	words := map[string]bool{}
	for _, word := range ContentWords(request) {
		words[strings.ToLower(word)] = true
	}
	if limit < 1 {
		return nil
	}

	type scored struct {
		program Program
		score   int
	}
	var ranked []scored
	for _, name := range commands {
		lower := strings.ToLower(name)
		score := 0
		if len(lower) > 1 && strings.Contains(lowered, lower) {
			score += 10
		}
		if words[lower] {
			score += 4
		}
		if score == 0 {
			continue
		}
		ranked = append(ranked, scored{
			program: Program{Name: name, ReadOnly: readOnlyPrograms[name]},
			score:   score,
		})
	}

	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].program.Name < ranked[j].program.Name
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	out := make([]Program, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, r.program)
	}
	return out
}

// Describe fills in what the programs say they are for, which is only worth
// doing for the handful of candidates that survived the first filter. The
// documentation is cached, so this is one cheap call per program once.
func Describe(programs []Program) []Program {
	out := make([]Program, len(programs))
	copy(out, programs)
	for i := range out {
		if out[i].Summary != "" {
			continue
		}
		docs, err := Load(out[i].Name)
		if err != nil {
			continue
		}
		out[i].Summary = firstLine(docs.Summary)
	}
	return out
}

// firstLine keeps a summary to one line, since the help of some programs opens
// with a paragraph.
func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if idx := strings.Index(text, "\n"); idx >= 0 {
		text = text[:idx]
	}
	return strings.TrimSpace(text)
}

// ---------------------------------------------------------------------------
// cache
// ---------------------------------------------------------------------------

const commandCacheVersion = 1

// commandCacheTTL is shorter than the documentation cache: installing software
// changes the command list far more often than it changes a manual page.
const commandCacheTTL = 24 * time.Hour

func commandCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "jev", "commands.json")
}

func readCommandCache() ([]string, bool) {
	path := commandCachePath()
	if path == "" {
		return nil, false
	}
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) > commandCacheTTL {
		return nil, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var entry struct {
		Version  int
		Commands []string
	}
	if err := json.Unmarshal(raw, &entry); err != nil || entry.Version != commandCacheVersion || len(entry.Commands) == 0 {
		return nil, false
	}
	return entry.Commands, true
}

func writeCommandCache(commands []string) {
	path := commandCachePath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	raw, err := json.Marshal(struct {
		Version  int
		Commands []string
	}{Version: commandCacheVersion, Commands: commands})
	if err != nil {
		return
	}
	_ = os.WriteFile(path, raw, 0o644)
}
