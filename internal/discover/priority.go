package discover

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// Asking the model about every command on the machine costs about nineteen
// thousand tokens, every invocation, and the request has to pay it even when
// the answer is one of the twenty commands a person uses all day. So the
// commands are asked about in tiers: the ones this machine is likely to be asked
// for first, then the ones whose own manual summary matches the words of the
// request, and only then the whole list.
//
// This file is the first tier. It is a list of names and nothing else, which is
// the property that makes it safe: a name cannot make an answer wrong, it can
// only change who is asked first, and a command that is not on the list stays
// reachable through the tiers below. The shaped tools that came before were a
// list of what could be done, and a request whose answer was not on that list
// could not be answered at all. That is the difference between a hint and a
// catalogue of capabilities.

// seedCommands is the priority list: the commands a person reaches for by
// default, plus the ones that report on the machine itself, because "how much
// memory is available" names no program and is exactly the kind of question the
// tiers below are worst at.
//
// It is written for a Unix that covers macOS and Linux both. A name that this
// machine does not have is dropped when the list is read, so the list can be
// generous without costing anything.
var seedCommands = []string{
	// Looking at a directory and at files.
	"ls", "pwd", "cat", "head", "tail", "less", "more", "wc", "tree", "file", "stat",
	"find", "mdfind", "open", "pbcopy", "pbpaste",
	// Searching inside files.
	"grep", "rg", "ag", "awk", "sed", "jq", "sort", "uniq", "cut", "tr", "diff",
	// Writing that does not destroy.
	"touch", "mkdir", "cp", "mv", "ln", "tee", "patch", "base64", "shasum",
	// Writing that destroys, which nobody should have to name twice.
	"rm", "rmdir", "trash", "kill", "pkill", "chmod", "chown", "defaults",
	// Archives and transfer.
	"tar", "zip", "unzip", "gzip", "curl", "wget", "rsync", "scp", "ssh",
	// The machine itself.
	"ps", "top", "df", "du", "vm_stat", "memory_pressure", "sysctl", "uptime",
	"uname", "sw_vers", "system_profiler", "diskutil", "iostat", "lsof",
	"netstat", "ping", "dig", "traceroute", "hostname", "whoami", "id",
	// Development.
	"git", "gh", "docker", "kubectl", "make", "go", "cargo", "npm", "node",
	"python3", "pip3", "sqlite3", "openssl", "vim", "nano", "code", "zed",
	// The manuals themselves, and the shell's own reporting.
	"man", "apropos", "which", "env", "printenv", "date", "cal", "echo",
	"printf", "seq", "bc", "time", "watch", "sleep", "xargs", "sudo",
}

// maxPriority caps how many names the first tier contributes. Each name costs
// about ten bytes in the question, so the cap is what keeps this tier near a
// thousand tokens instead of twenty thousand.
const maxPriority = 100

// maxSeedPriority is how much of the cap the seed may take. The seed is written
// for every Unix this runs on, so part of it is always wasted here; holding it
// below the cap is what leaves room for the commands this machine actually
// runs, which are the better evidence of what to ask about first.
const maxSeedPriority = 70

// maxLearned caps the learned half. Past this the list stops being a list of
// what a person uses and starts being a copy of the machine.
const maxLearned = 200

// priorityFile is what the learned half looks like on disk.
type priorityFile struct {
	Version int
	Used    map[string]int
}

// Priority returns the commands to ask about first: the seed this machine has,
// then the commands this person has actually run, most used first.
//
// The learned half is what makes the list converge on the machine it runs on: a
// command used once is a command worth offering next time, without anyone
// writing it down. It is state, so it is kept in the cache directory and it can
// be turned off, which the eval does, because two runs that see different
// states are not comparable.
func Priority() []string {
	seen := map[string]bool{}
	out := make([]string, 0, maxPriority)
	for i, name := range seedCommands {
		if i == maxSeedPriority {
			break
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if learnedEnabled() {
		for _, entry := range learnedByUse() {
			if len(out) == maxPriority {
				break
			}
			if seen[entry.name] {
				continue
			}
			seen[entry.name] = true
			out = append(out, entry.name)
		}
	}
	return out
}

// Learned reports whether the learned half is part of the list right now, which
// is what the CLI shows and what the eval turns off.
func Learned() bool { return learnedEnabled() }

// learnedEnabled reads the switch. It is an environment variable rather than a
// flag because the eval runner is not the only caller: anything that has to be
// reproducible sets it once and gets the seed.
func learnedEnabled() bool { return os.Getenv("IVO_LEARNED") != "0" }

type learnedEntry struct {
	name  string
	count int
}

// learnedByUse returns the commands that were run here, most used first.
func learnedByUse() []learnedEntry {
	state, err := readPriority()
	if err != nil {
		return nil
	}
	entries := make([]learnedEntry, 0, len(state.Used))
	for name, count := range state.Used {
		if name == "" || count < 1 {
			continue
		}
		entries = append(entries, learnedEntry{name, count})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count == entries[j].count {
			return entries[i].name < entries[j].name
		}
		return entries[i].count > entries[j].count
	})
	return entries
}

// Promote records that a command was used. It is called after a call was
// confirmed and ran, and not when a command was merely nominated: a nomination
// that was wrong must not push a command up the list it was nominated from,
// or one bad answer would bias every answer after it.
func Promote(name string) {
	if name == "" || !learnedEnabled() {
		return
	}
	state, err := readPriority()
	if err != nil {
		return
	}
	if state.Used == nil {
		state.Used = map[string]int{}
	}
	state.Used[name]++
	trimLearned(&state)
	writePriority(state)
}

// trimLearned keeps the most used entries, so the file cannot grow into a copy
// of the command list one promotion at a time. It takes the state by pointer
// because replacing the map in a copy is how the trim silently did nothing.
func trimLearned(state *priorityFile) {
	if len(state.Used) <= maxLearned {
		return
	}
	entries := make([]learnedEntry, 0, len(state.Used))
	for name, count := range state.Used {
		entries = append(entries, learnedEntry{name, count})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count == entries[j].count {
			return entries[i].name < entries[j].name
		}
		return entries[i].count > entries[j].count
	})
	kept := map[string]int{}
	for _, entry := range entries[:maxLearned] {
		kept[entry.name] = entry.count
	}
	state.Used = kept
}

// priorityVersion invalidates a file written by an older layout.
const priorityVersion = 1

func priorityPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "ivo", "priority.json")
}

func readPriority() (priorityFile, error) {
	state := priorityFile{Version: priorityVersion, Used: map[string]int{}}
	path := priorityPath()
	if path == "" {
		return state, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return state, nil
	}
	var stored priorityFile
	if err := json.Unmarshal(raw, &stored); err != nil || stored.Version != priorityVersion {
		return state, nil
	}
	if stored.Used != nil {
		state.Used = stored.Used
	}
	return state, nil
}

func writePriority(state priorityFile) {
	path := priorityPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return
	}
	// A cache that cannot be written is not a failure worth reporting: the list
	// simply stays as it was.
	_ = os.WriteFile(path, raw, 0o644)
}
