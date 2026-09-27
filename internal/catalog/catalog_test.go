package catalog_test

import (
	"strings"
	"testing"

	"github.com/harlleyoliveira/jev-cli/internal/catalog"
	"github.com/harlleyoliveira/jev-cli/internal/env"
	"github.com/harlleyoliveira/jev-cli/internal/typesafe"
)

// testEnv is a hand-built environment: no filesystem, no processes, so the
// catalog can be tested exactly.
func testEnv() *env.Env {
	bins := map[string]bool{}
	for _, b := range catalog.Binaries() {
		bins[b] = true
	}
	return &env.Env{
		Request: "liste todos os arquivos desse diretório",
		OS:      "darwin",
		CWD:     "/tmp/projeto",
		Home:    "/Users/test",
		Shell:   "/bin/zsh",
		Bins:    bins,
		Entries: []env.Entry{
			{Name: "src", IsDir: true},
			{Name: "main.go"},
			{Name: "README.md"},
		},
		EntryCount: 3,
		Git:        env.Git{IsRepo: true, Root: "/tmp/projeto", Branch: "main"},
		Candidates: env.Candidates{
			Patterns: []string{"*.go"},
			Terms:    []string{"TODO"},
		},
	}
}

func TestEveryEntryBuildsAValidSpec(t *testing.T) {
	e := testEnv()
	all := catalog.All()
	available := catalog.Available(all, e)
	if len(available) != len(all) {
		t.Fatalf("available = %d, want all %d entries in a fully equipped environment", len(available), len(all))
	}
	specs := catalog.Specs(all, e)
	for _, cmd := range all {
		spec, ok := specs[cmd.ID]
		if !ok {
			t.Errorf("%s: no spec", cmd.ID)
			continue
		}
		if err := spec.Validate(cmd.ID); err != nil {
			t.Errorf("%s: %v", cmd.ID, err)
		}
		if !cmd.ReadOnly {
			t.Errorf("%s: v1 must be read-only", cmd.ID)
		}
		if cmd.What == "" {
			t.Errorf("%s: missing the contrastive description of the option", cmd.ID)
		}
	}
}

func TestUnavailableCommandsAreDropped(t *testing.T) {
	e := testEnv()
	e.Bins["git"] = false
	e.Git = env.Git{}
	for _, cmd := range catalog.Available(catalog.All(), e) {
		if strings.HasPrefix(cmd.ID, "git_") {
			t.Errorf("%s should be unavailable outside a repository", cmd.ID)
		}
	}
}

// answersFor builds the full answer set for one command's spec, exercising
// every slot. Optional slots come back as "the user said nothing".
func answersFor(spec catalog.Spec, cmdID string, e *env.Env) map[string]typesafe.Answer {
	out := map[string]typesafe.Answer{}
	for _, slot := range spec.Slots {
		qid := slot.QuestionID(cmdID)
		if slot.IsFlag() {
			out[qid] = noul(0)
		} else {
			out[qid] = choice(firstKey(slot, e), 1)
		}
		if slot.Stated {
			// "The user said nothing about this", so the declared default
			// stands. Individual tests override this to exercise the gate.
			out[qid+"?"] = noul(0)
		}
	}
	return out
}

func firstKey(slot catalog.Slot, e *env.Env) string {
	opts := slot.Options
	if slot.OptionsFor != nil {
		opts = slot.OptionsFor(e)
	}
	for _, o := range opts {
		if o.Key != catalog.NoneKey {
			return o.Key
		}
	}
	return catalog.NoneKey
}

func noul(v float64) typesafe.Answer {
	return typesafe.Answer{Type: typesafe.KindNoul, Noul: v}
}

func choice(key string, confidence float64) typesafe.Answer {
	return typesafe.Answer{
		Type:          typesafe.KindChoice,
		Choice:        key,
		Confidence:    confidence,
		Probabilities: map[string]float64{key: confidence},
	}
}

func specFor(t *testing.T, e *env.Env, id string) (catalog.Command, catalog.Spec) {
	t.Helper()
	for _, cmd := range catalog.All() {
		if cmd.ID != id {
			continue
		}
		spec, ok := cmd.Build(e)
		if !ok {
			t.Fatalf("%s: not available", id)
		}
		return cmd, spec
	}
	t.Fatalf("no catalog entry %q", id)
	return catalog.Command{}, catalog.Spec{}
}

func assemble(t *testing.T, e *env.Env, id string, overrides map[string]typesafe.Answer) catalog.Assembled {
	t.Helper()
	cmd, spec := specFor(t, e, id)
	answers := answersFor(spec, cmd.ID, e)
	for k, v := range overrides {
		answers[k] = v
	}
	got, err := catalog.Assemble(cmd, spec, answers, e, nil)
	if err != nil {
		t.Fatalf("Assemble(%s): %v", id, err)
	}
	return got
}

func TestFlagsAreOptIn(t *testing.T) {
	e := testEnv()
	got := assemble(t, e, "list_directory", nil)
	if strings.Join(got.Argv, " ") != "ls ." {
		t.Errorf("argv = %q, want %q", got.Argv, "ls .")
	}
}

func TestFlagsAreAddedWhenTheUserAsksForThem(t *testing.T) {
	e := testEnv()
	got := assemble(t, e, "list_directory", map[string]typesafe.Answer{
		"list_directory.ls_hidden":  noul(0.91),
		"list_directory.ls_hidden?": noul(0.95),
		"list_directory.ls_long":    noul(0.88),
		"list_directory.ls_long?":   noul(0.90),
	})
	if strings.Join(got.Argv, " ") != "ls -a -l ." {
		t.Errorf("argv = %q", got.Argv)
	}
}

func TestMutuallyExclusiveFlagsNeverBothLand(t *testing.T) {
	e := testEnv()
	got := assemble(t, e, "list_directory", map[string]typesafe.Answer{
		"list_directory.ls_sort_time":  noul(0.70),
		"list_directory.ls_sort_time?": noul(0.85),
		"list_directory.ls_sort_size":  noul(0.90),
		"list_directory.ls_sort_size?": noul(0.92),
	})
	if strings.Join(got.Argv, " ") != "ls -S ." {
		t.Errorf("argv = %q, want only the stronger sort flag", got.Argv)
	}
}

func TestOptionalArgumentsFallBackToTheDeclaredDefault(t *testing.T) {
	e := testEnv()
	got := assemble(t, e, "find_files", nil)
	// Nothing was stated: the depth default stands, and the name match is
	// dropped entirely because there is no pattern for it to attach to.
	if strings.Join(got.Argv, " ") != "find . -maxdepth 3" {
		t.Errorf("argv = %q", got.Argv)
	}
}

func TestAPatternBringsItsOwnFlag(t *testing.T) {
	e := testEnv()
	got := assemble(t, e, "find_files", map[string]typesafe.Answer{
		"name_pattern":  choice("*.go", 0.95),
		"name_pattern?": noul(0.97),
	})
	if strings.Join(got.Argv, " ") != "find . -maxdepth 3 -name *.go" {
		t.Errorf("argv = %q", got.Argv)
	}
}

func TestCaseInsensitiveMatchIsUsedWhenAsked(t *testing.T) {
	e := testEnv()
	got := assemble(t, e, "find_files", map[string]typesafe.Answer{
		"name_pattern":           choice("*.go", 0.95),
		"name_pattern?":          noul(0.97),
		"find_files.find_match":  choice("insensitive", 0.9),
		"find_files.find_match?": noul(0.9),
		"find_files.find_type":   choice("f", 0.95),
		"find_files.find_type?":  noul(0.95),
	})
	if strings.Join(got.Argv, " ") != "find . -maxdepth 3 -type f -iname *.go" {
		t.Errorf("argv = %q", got.Argv)
	}
}

func TestUnresolvableRequiredValueIsReportedNotGuessed(t *testing.T) {
	e := testEnv()
	// show_file has no fallback for its target: "print the current directory"
	// is not a thing, so it must fail loudly rather than pick something.
	got := assemble(t, e, "show_file", map[string]typesafe.Answer{
		"target_path": choice("no_path_filter", 0.9),
	})
	if len(got.Missing) != 1 || got.Missing[0] != "target_path" {
		t.Fatalf("Missing = %v, want [target_path]", got.Missing)
	}
}

func TestEscapeHatchOnARequiredValueIsAlsoUnresolved(t *testing.T) {
	e := testEnv()
	got := assemble(t, e, "count_lines", map[string]typesafe.Answer{
		"target_path": choice(catalog.NoneKey, 0.8),
	})
	if len(got.Missing) != 1 {
		t.Fatalf("Missing = %v, want the target to be unresolved", got.Missing)
	}
}

func TestForcedLiteralReplacesTheDecision(t *testing.T) {
	e := testEnv()
	cmd, spec := specFor(t, e, "list_directory")
	answers := answersFor(spec, cmd.ID, e)
	got, err := catalog.Assemble(cmd, spec, answers, e, map[string]string{"target_path": "/etc/hosts"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if strings.Join(got.Argv, " ") != "ls /etc/hosts" {
		t.Errorf("argv = %q", got.Argv)
	}
}

func TestSearchPrefersRipgrepAndKeepsTheTermPositional(t *testing.T) {
	e := testEnv()
	got := assemble(t, e, "search_text", map[string]typesafe.Answer{
		"search_terms": choice("TODO", 0.9),
	})
	if strings.Join(got.Argv, " ") != "rg -e TODO ." {
		t.Errorf("argv = %q", got.Argv)
	}

	e.Bins["rg"] = false
	got = assemble(t, e, "search_text", map[string]typesafe.Answer{
		"search_terms": choice("TODO", 0.9),
	})
	if strings.Join(got.Argv, " ") != "grep -r -e TODO ." {
		t.Errorf("grep fallback argv = %q", got.Argv)
	}
}

func TestAnAnswerOutsideTheClosedSetIsAProtocolError(t *testing.T) {
	// The API guarantees answers come from the options we supplied. If that
	// ever stops being true, the right move is to fail loudly: the value is
	// never turned into a token, and no fallback quietly papers over it.
	e := testEnv()
	cmd, spec := specFor(t, e, "list_directory")
	answers := answersFor(spec, cmd.ID, e)
	answers["target_path"] = choice(".; rm -rf /", 0.99)

	_, err := catalog.Assemble(cmd, spec, answers, e, nil)
	if err == nil {
		t.Fatal("expected an error for an answer outside the closed set")
	}
	if !strings.Contains(err.Error(), "not one of its options") {
		t.Errorf("error = %v, want it to name the contract violation", err)
	}
}

func TestAllowlistContainsProgramsAndNoFlags(t *testing.T) {
	e := testEnv()
	set := map[string]bool{}
	for _, a := range catalog.Allowlist(catalog.Specs(catalog.All(), e), e) {
		set[a] = true
		if strings.HasPrefix(a, "-") {
			t.Errorf("allowlist contains a flag: %q", a)
		}
	}
	// Programs reachable in this environment, including the ones a slot
	// provides rather than the template (head/tail come from show_reader).
	for _, want := range []string{"ls", "find", "rg", "cat", "head", "tail", "wc", "du", "file", "git", "pwd"} {
		if !set[want] {
			t.Errorf("allowlist is missing %q", want)
		}
	}
	if set["grep"] {
		t.Error("grep is not reachable while ripgrep is installed")
	}

	// With ripgrep gone, the grep branch is what must be reachable.
	e.Bins["rg"] = false
	set = map[string]bool{}
	for _, a := range catalog.Allowlist(catalog.Specs(catalog.All(), e), e) {
		set[a] = true
	}
	if !set["grep"] {
		t.Error("allowlist is missing grep in an environment without ripgrep")
	}
	if set["rg"] {
		t.Error("rg should be unreachable when it is not installed")
	}
}

func TestQuestionIdsAreDeduplicatedAcrossCommands(t *testing.T) {
	e := testEnv()
	all := catalog.All()
	specs := catalog.Specs(all, e)
	questions := catalog.Questions(all, specs, e)

	// target_path is shared by eight commands and must be asked once.
	count := 0
	for id := range questions {
		if id == "target_path" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("target_path asked %d times, want 1", count)
	}
	if _, ok := questions["find_files.find_depth?"]; !ok {
		t.Error("optional slots must come with their stated question")
	}
	if _, ok := questions["intent"]; ok {
		t.Error("intent is added by resolve, not by Questions")
	}
}

// ---------------------------------------------------------------------------
// Invariants that the whole design rests on. These are the tests that would
// have to fail for jev-cli to become "a model that writes shell commands".
// ---------------------------------------------------------------------------

// authoredTokens is every token the catalog itself wrote down: template
// literals, flag token groups, option arguments and empty-value fallbacks.
func authoredTokens(cmdID string, spec catalog.Spec, e *env.Env) map[string]bool {
	set := map[string]bool{}
	for _, tok := range spec.Argv {
		if !isPlaceholderToken(tok) {
			set[tok] = true
		}
	}
	for _, slot := range spec.Slots {
		for _, argv := range slot.TrueArgvByCmd {
			for _, tok := range argv {
				set[tok] = true
			}
		}
		for _, opt := range optionsOf(slot, e) {
			for _, tok := range append(append([]string{}, opt.Argv...), slot.EmptyFallback...) {
				set[tok] = true
			}
		}
		for _, tok := range slot.EmptyFallback {
			set[tok] = true
		}
	}
	delete(set, "")
	return set
}

func isPlaceholderToken(tok string) bool {
	return len(tok) > 2 && tok[0] == '{' && tok[len(tok)-1] == '}'
}

func optionsOf(slot catalog.Slot, e *env.Env) []catalog.Value {
	if slot.OptionsFor != nil {
		return slot.OptionsFor(e)
	}
	return slot.Options
}

// answerCombos sweeps every slot across every value it can take, so the sweep
// covers the flags together, not just one at a time.
func answerCombos(spec catalog.Spec, cmdID string, e *env.Env) []map[string]typesafe.Answer {
	combos := []map[string]typesafe.Answer{{}}
	const cap = 4000

	for _, slot := range spec.Slots {
		qid := slot.QuestionID(cmdID)
		var variants []map[string]typesafe.Answer

		switch {
		case slot.IsFlag():
			if slot.Stated {
				variants = []map[string]typesafe.Answer{
					{qid: noul(0), qid + "?": noul(0)},
					{qid: noul(1), qid + "?": noul(0)},
					{qid: noul(0), qid + "?": noul(1)},
					{qid: noul(1), qid + "?": noul(1)},
				}
			} else {
				variants = []map[string]typesafe.Answer{{qid: noul(0)}, {qid: noul(1)}}
			}
		case slot.Stated:
			anyOption := catalog.NoneKey
			for _, opt := range optionsOf(slot, e) {
				anyOption = opt.Key
				break
			}
			variants = []map[string]typesafe.Answer{
				{qid: choice(anyOption, 1), qid + "?": noul(0)},
			}
			for _, opt := range optionsOf(slot, e) {
				variants = append(variants, map[string]typesafe.Answer{qid: choice(opt.Key, 1), qid + "?": noul(1)})
			}
		default:
			for _, opt := range optionsOf(slot, e) {
				variants = append(variants, map[string]typesafe.Answer{qid: choice(opt.Key, 1)})
			}
		}

		next := make([]map[string]typesafe.Answer, 0, len(combos)*len(variants))
		for _, base := range combos {
			for _, v := range variants {
				merged := map[string]typesafe.Answer{}
				for k, val := range base {
					merged[k] = val
				}
				for k, val := range v {
					merged[k] = val
				}
				next = append(next, merged)
			}
		}
		combos = next
		if len(combos) > cap {
			combos = combos[:cap]
		}
	}
	return combos
}

func TestEveryTokenInArgvWasAuthoredHere(t *testing.T) {
	e := testEnv()
	all := catalog.All()
	specs := catalog.Specs(all, e)

	for _, cmd := range all {
		spec, ok := specs[cmd.ID]
		if !ok {
			t.Fatalf("%s: no spec", cmd.ID)
		}
		allowed := authoredTokens(cmd.ID, spec, e)
		combos := answerCombos(spec, cmd.ID, e)

		for n, combo := range combos {
			answers := answersFor(spec, cmd.ID, e)
			for k, v := range combo {
				answers[k] = v
			}
			got, err := catalog.Assemble(cmd, spec, answers, e, nil)
			if err != nil {
				t.Fatalf("%s combo %d: %v", cmd.ID, n, err)
			}
			for i, tok := range got.Argv {
				if !allowed[tok] {
					t.Fatalf("%s combo %d: token %q is not authored anywhere in the catalog (argv: %v)",
						cmd.ID, n, tok, got.Argv)
				}
				if i == 0 && isShell(tok) {
					t.Fatalf("%s combo %d: argv[0] is a shell (%q)", cmd.ID, n, tok)
				}
			}
			if len(got.Missing) == 0 && len(got.Argv) == 0 {
				t.Fatalf("%s combo %d: resolved with no missing slots but produced no argv", cmd.ID, n)
			}
		}
		t.Logf("%s: %d combinations, all tokens authored", cmd.ID, len(combos))
	}
}

func isShell(program string) bool {
	switch program {
	case "sh", "bash", "zsh", "dash", "ksh", "csh", "tcsh", "fish", "env", "xargs", "eval", "exec", "sudo":
		return true
	}
	return false
}

func TestNoCommandCanReachAShell(t *testing.T) {
	e := testEnv()
	for _, cmd := range catalog.All() {
		for _, needs := range cmd.Needs {
			if isShell(needs) {
				t.Errorf("%s declares a shell as a dependency: %q", cmd.ID, needs)
			}
		}
	}
	for _, bin := range catalog.Allowlist(catalog.Specs(catalog.All(), e), e) {
		if isShell(bin) {
			t.Errorf("the allowlist lets a shell through: %q", bin)
		}
	}
	// And nothing in any template asks a program to run a command string.
	for _, cmd := range catalog.All() {
		spec, ok := cmd.Build(e)
		if !ok {
			continue
		}
		for _, tok := range spec.Argv {
			if tok == "-c" {
				t.Errorf("%s has a -c token, which is how a shell is told to run a string", cmd.ID)
			}
		}
	}
}
