package catalog_test

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/harlleyoliveira/jev-cli/internal/catalog"
	"github.com/harlleyoliveira/jev-cli/internal/discover"
	"github.com/harlleyoliveira/jev-cli/internal/env"
	"github.com/harlleyoliveira/jev-cli/internal/run"
	"github.com/harlleyoliveira/jev-cli/internal/typesafe"
)

// testEnv is a hand-built environment: no filesystem, no processes, so the
// layer can be tested exactly.
func testEnv(t *testing.T) *env.Env {
	t.Helper()
	bins := map[string]bool{}
	for _, b := range catalog.Binaries() {
		bins[b] = true
	}
	return &env.Env{
		Request: "list all files in this directory",
		OS:      "darwin",
		CWD:     "/tmp/project",
		Home:    "/Users/test",
		Shell:   "/bin/zsh",
		Bins:    bins,
		Entries: []env.Entry{
			{Name: "src", IsDir: true},
			{Name: "main.go"},
			{Name: "README.md"},
		},
		EntryCount: 3,
		Docs:       testDocs(t),
		Candidates: env.Candidates{
			Patterns: []string{"*.go"},
			Terms:    []string{"TODO"},
		},
	}
}

func TestEveryToolBindsAndValidates(t *testing.T) {
	e := testEnv(t)
	all := catalog.All()
	available := catalog.Available(all, e)
	if len(available) != len(all) {
		t.Fatalf("available = %d, want all %d tools in a fully equipped environment", len(available), len(all))
	}
	bindings := catalog.Bindings(all, e)
	for _, tool := range all {
		binding, ok := bindings[tool.Name]
		if !ok {
			t.Errorf("%s: no binding", tool.Name)
			continue
		}
		if err := binding.Validate(tool.Name); err != nil {
			t.Errorf("%s: %v", tool.Name, err)
		}
		if !tool.ReadOnly {
			t.Errorf("%s: every tool must be read-only", tool.Name)
		}
		if tool.What == "" {
			t.Errorf("%s: missing the description the model chooses by", tool.Name)
		}
		for _, param := range binding.Params {
			if param.Desc == "" {
				t.Errorf("%s.%s: a parameter needs a description to build its question from", tool.Name, param.Name)
			}
		}
	}
	if len(all) != 5 {
		t.Errorf("the catalog has %d tools; this version is meant to stay small", len(all))
	}
	// read_manual is the layer documenting itself, so it must be able to talk
	// about the programs the other tools use.
	manual, ok := bindings["read_manual"]
	if !ok {
		t.Fatal("read_manual has no binding")
	}
	// man formats for a terminal, so the binding has to say that its output
	// needs adapting. Without it the manual arrives as "N backspace N".
	if manual.Output != catalog.OutputStripOverstrike {
		t.Errorf("read_manual output = %v, want OutputStripOverstrike", manual.Output)
	}
}

func TestUnavailableToolsAreDropped(t *testing.T) {
	e := testEnv(t)
	// search_text binds to ripgrep or to grep, so it only disappears when both
	// are missing.
	e.Bins["rg"] = false
	if len(catalog.Available(catalog.All(), e)) != 5 {
		t.Error("search_text should survive on grep alone")
	}
	e.Bins["grep"] = false
	available := catalog.Available(catalog.All(), e)
	for _, tool := range available {
		if tool.Name == "search_text" {
			t.Error("search_text needs ripgrep or grep")
		}
	}
	if len(available) != 4 {
		t.Errorf("available = %d, want 4 without either search program", len(available))
	}

	// read_manual disappears with man, since that is all it runs.
	e.Bins["man"] = false
	for _, tool := range catalog.Available(catalog.All(), e) {
		if tool.Name == "read_manual" {
			t.Error("read_manual needs man")
		}
	}
}

// answersFor builds the full answer set for one binding, exercising every
// parameter. Gated parameters come back as "the request said nothing".
func answersFor(binding catalog.Binding, tool string, e *env.Env) map[string]typesafe.Answer {
	out := map[string]typesafe.Answer{}
	for _, param := range binding.Params {
		qid := param.QuestionID(tool)
		if param.Kind == catalog.FlagParam {
			out[qid] = noul(0)
		} else {
			out[qid] = choice(firstValue(param, e), 1)
		}
		if param.Gated {
			out[qid+"?"] = noul(0)
		}
	}
	return out
}

func firstValue(param catalog.Param, e *env.Env) string {
	for _, v := range valuesOf(param, e) {
		if v.Key != catalog.NoneKey {
			return v.Key
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

func bindingFor(t *testing.T, e *env.Env, name string) (catalog.Tool, catalog.Binding) {
	t.Helper()
	for _, tool := range catalog.All() {
		if tool.Name != name {
			continue
		}
		binding, ok := tool.Bind(e)
		if !ok {
			t.Fatalf("%s: not available", name)
		}
		return tool, binding
	}
	t.Fatalf("no tool named %q", name)
	return catalog.Tool{}, catalog.Binding{}
}

func fill(t *testing.T, e *env.Env, name string, flags []string) catalog.Result {
	t.Helper()
	return fillWith(t, e, name, nil, flags)
}

// fillWith also overrides the answers a parameter is filled from, which is how
// the operand tests reach the escape hatch and the low confidence paths.
func fillWith(t *testing.T, e *env.Env, name string, overrides map[string]typesafe.Answer, flags []string) catalog.Result {
	t.Helper()
	tool, binding := bindingFor(t, e, name)
	answers := answersFor(binding, tool.Name, e)
	for k, v := range overrides {
		answers[k] = v
	}
	got, err := catalog.Fill(tool, binding, answers, flags, e)
	if err != nil {
		t.Fatalf("Fill(%s): %v", name, err)
	}
	return got
}

func TestAFlagStaysOffUnlessTheRequestAsksForIt(t *testing.T) {
	e := testEnv(t)
	got := fill(t, e, "list_directory", nil)
	if strings.Join(got.Argv, " ") != "ls ." {
		t.Errorf("argv = %q, want %q", got.Argv, "ls .")
	}
	if got.Call.Tool != "list_directory" {
		t.Errorf("call tool = %q", got.Call.Tool)
	}
	if got.Call.Args["target"] != "." {
		t.Errorf("call args = %v, want the target filled", got.Call.Args)
	}
}

func TestFlagsComeFromTheDocumentedOptions(t *testing.T) {
	e := testEnv(t)
	_, binding := bindingFor(t, e, "list_directory")

	// The option list is the program's own, so the tool page and the question
	// are built from the manual rather than from this repository.
	if len(binding.Options) < 30 {
		t.Fatalf("binding offers %d options, want the manual's list", len(binding.Options))
	}
	if binding.Discover != "ls" {
		t.Errorf("discover = %q, want ls", binding.Discover)
	}

	// The flags the stage chose land in the template, in the order chosen.
	got := fill(t, e, "list_directory", []string{"-a", "-l"})
	if strings.Join(got.Argv, " ") != "ls -a -l ." {
		t.Errorf("argv = %q", got.Argv)
	}
}

func TestAnOptionTheProgramDoesNotDocumentIsRefused(t *testing.T) {
	// The flag stage can only ever return a key from the option list, so a
	// token outside it is a contract violation and must fail loudly rather than
	// reach the command line.
	e := testEnv(t)
	tool, binding := bindingFor(t, e, "list_directory")
	_, err := catalog.Fill(tool, binding, answersFor(binding, tool.Name, e), []string{"--exec=rm -rf /"}, e)
	if err == nil {
		t.Fatal("expected an error for an option the program does not document")
	}
	if !strings.Contains(err.Error(), "is not an option") {
		t.Errorf("error = %v, want it to name the contract violation", err)
	}
}

// TestTheOptionQuestionOnlyListsTheChosenToolsOptions is the shape the staging
// exists for: the model compares the request against one tool's options, not
// against every flag of every tool at once.
func TestTheOptionQuestionOnlyListsTheChosenToolsOptions(t *testing.T) {
	e := testEnv(t)
	_, ls := bindingFor(t, e, "list_directory")

	question := catalog.OptionQuestion(ls.Options, nil, 0).(typesafe.ChoiceQuestion)
	if len(question.Criteria) < 30 {
		t.Errorf("the option question lists %d options, want the tool's whole manual", len(question.Criteria))
	}
	if _, ok := question.Criteria["-a"]; !ok {
		t.Error("the option question is missing -a")
	}
	// A flag from another program must not appear.
	if _, ok := question.Criteria["--search-zip"]; ok {
		t.Error("the option question offered an option belonging to another program")
	}

	// An option already chosen is not offered again, and the wording says why
	// the round is happening.
	second := catalog.OptionQuestion(ls.Options, []string{"-a"}, 1).(typesafe.ChoiceQuestion)
	if _, ok := second.Criteria["-a"]; ok {
		t.Error("an option already chosen was offered again")
	}
	if !strings.Contains(fmt.Sprint(second.Instructions), "still does not satisfy") {
		t.Errorf("instructions = %v", second.Instructions)
	}
}

// TestSatisfiedQuestionCarriesTheCandidate: the stopping condition has to name
// the call it judges, or the model cannot answer it.
func TestSatisfiedQuestionCarriesTheCandidate(t *testing.T) {
	question := catalog.SatisfiedQuestion("ls -a .").(typesafe.NoulQuestion)
	rendered := fmt.Sprint(question.Instructions)
	for _, want := range []string{"ls -a .", "candidate", "satisfy"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("instructions = %v, missing %q", rendered, want)
		}
	}
}

func TestAnUnfillableParameterIsReportedNotGuessed(t *testing.T) {
	e := testEnv(t)
	// show_file cannot print "no file at all", so the escape hatch is the only
	// way to leave its target unfilled, and it must fail loudly.
	got := fillWith(t, e, "show_file", map[string]typesafe.Answer{
		"target_path": choice(catalog.NoneKey, 0.9),
	}, nil)
	if len(got.Unfilled) != 1 || got.Unfilled[0] != "target" {
		t.Fatalf("Unfilled = %v, want [target]", got.Unfilled)
	}
	if _, ok := got.Call.Args["target"]; ok {
		t.Errorf("an unfilled parameter must not appear in the call: %v", got.Call.Args)
	}
}

func TestAnAnswerOutsideTheClosedSetIsAProtocolError(t *testing.T) {
	// The API guarantees answers come from the values we supplied. If that ever
	// stops being true, the right move is to fail loudly: the value is never
	// turned into a token, and no default quietly papers over it.
	e := testEnv(t)
	tool, binding := bindingFor(t, e, "list_directory")
	answers := answersFor(binding, tool.Name, e)
	answers["target_path"] = choice(".; rm -rf /", 0.99)

	_, err := catalog.Fill(tool, binding, answers, nil, e)
	if err == nil {
		t.Fatal("expected an error for an answer outside the closed set")
	}
	if !strings.Contains(err.Error(), "not one of its values") {
		t.Errorf("error = %v, want it to name the contract violation", err)
	}
}

func TestSearchPrefersRipgrepAndKeepsTheTermPositional(t *testing.T) {
	e := testEnv(t)
	got := fillWith(t, e, "search_text", map[string]typesafe.Answer{
		"search_terms": choice("TODO", 0.9),
	}, nil)
	if strings.Join(got.Argv, " ") != "rg -e TODO ." {
		t.Errorf("argv = %q", got.Argv)
	}

	e.Bins["rg"] = false
	got = fillWith(t, e, "search_text", map[string]typesafe.Answer{
		"search_terms": choice("TODO", 0.9),
	}, nil)
	if strings.Join(got.Argv, " ") != "grep -r -e TODO ." {
		t.Errorf("grep fallback argv = %q", got.Argv)
	}
}

func TestTheFileFilterCarriesItsOwnFlag(t *testing.T) {
	e := testEnv(t)
	got := fillWith(t, e, "search_text", map[string]typesafe.Answer{
		"search_terms":       choice("TODO", 0.9),
		"search_text.files":  choice("*.go", 0.93),
		"search_text.files?": noul(0.91),
	}, nil)
	if strings.Join(got.Argv, " ") != "rg -g *.go -e TODO ." {
		t.Errorf("argv = %q, want the file filter applied", got.Argv)
	}
	if got.Call.Args["files"] != "*.go" {
		t.Errorf("call args = %v, want files *.go", got.Call.Args)
	}

	// And when the request says nothing about files, no flag is left dangling.
	got = fillWith(t, e, "search_text", map[string]typesafe.Answer{
		"search_terms": choice("TODO", 0.9),
	}, nil)
	if strings.Contains(strings.Join(got.Argv, " "), "-g") {
		t.Errorf("argv = %q, want no flag without a filter", got.Argv)
	}
}

func TestShowFilePicksHowMuchToPrint(t *testing.T) {
	e := testEnv(t)
	got := fillWith(t, e, "show_file", map[string]typesafe.Answer{
		"show_file.how_much": choice("first_lines", 0.9),
		"target_path":        choice("README.md", 0.98),
	}, nil)
	if strings.Join(got.Argv, " ") != "head -n 20 README.md" {
		t.Errorf("argv = %q", got.Argv)
	}
}

// ---------------------------------------------------------------------------
// Invariants the whole layer rests on. These are the tests that would have to
// fail for jev-cli to become "a model that writes shell commands".
// ---------------------------------------------------------------------------

// authoredTokens is every token the layer itself wrote down: template literals,
// flag tokens, value arguments.
func authoredTokens(binding catalog.Binding, e *env.Env) map[string]bool {
	set := map[string]bool{}
	for _, tok := range binding.Argv {
		if !isPlaceholderToken(tok) {
			set[tok] = true
		}
	}
	for _, param := range binding.Params {
		for _, tok := range param.Argv {
			set[tok] = true
		}
		for _, v := range valuesOf(param, e) {
			for _, tok := range v.Argv {
				set[tok] = true
			}
		}
	}
	// The options a program documents can reach the command line too, through
	// the stage that chooses them.
	for _, option := range binding.Options {
		for _, tok := range option.Flags {
			set[tok] = true
		}
	}
	delete(set, "")
	return set
}

func isPlaceholderToken(tok string) bool {
	return len(tok) > 2 && tok[0] == '{' && tok[len(tok)-1] == '}'
}

func valuesOf(param catalog.Param, e *env.Env) []catalog.Value {
	if param.ValuesFor != nil {
		return param.ValuesFor(e)
	}
	return param.Values
}

// answerCombos sweeps every parameter across every value it can take, so the
// sweep covers the flags together, not just one at a time.
func answerCombos(binding catalog.Binding, tool string, e *env.Env) []map[string]typesafe.Answer {
	combos := []map[string]typesafe.Answer{{}}
	const cap = 4000

	for _, param := range binding.Params {
		qid := param.QuestionID(tool)
		var variants []map[string]typesafe.Answer

		switch {
		case param.Kind == catalog.FlagParam:
			if param.Gated {
				variants = []map[string]typesafe.Answer{
					{qid: noul(0), qid + "?": noul(0)},
					{qid: noul(1), qid + "?": noul(0)},
					{qid: noul(0), qid + "?": noul(1)},
					{qid: noul(1), qid + "?": noul(1)},
				}
			} else {
				variants = []map[string]typesafe.Answer{{qid: noul(0)}, {qid: noul(1)}}
			}
		case param.Gated:
			anyValue := catalog.NoneKey
			for _, v := range valuesOf(param, e) {
				anyValue = v.Key
				break
			}
			variants = []map[string]typesafe.Answer{
				{qid: choice(anyValue, 1), qid + "?": noul(0)},
			}
			for _, v := range valuesOf(param, e) {
				variants = append(variants, map[string]typesafe.Answer{qid: choice(v.Key, 1), qid + "?": noul(1)})
			}
		default:
			for _, v := range valuesOf(param, e) {
				variants = append(variants, map[string]typesafe.Answer{qid: choice(v.Key, 1)})
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
	e := testEnv(t)
	all := catalog.All()
	bindings := catalog.Bindings(all, e)

	for _, tool := range all {
		binding, ok := bindings[tool.Name]
		if !ok {
			t.Fatalf("%s: no binding", tool.Name)
		}
		allowed := authoredTokens(binding, e)
		combos := answerCombos(binding, tool.Name, e)

		// Every documented option is swept as well, since those tokens reach
		// the command line through the flag stage.
		optionSets := [][]string{nil, allOptionTokens(binding)}

		for n, combo := range combos {
			answers := answersFor(binding, tool.Name, e)
			for k, v := range combo {
				answers[k] = v
			}
			for _, flags := range optionSets {
				got, err := catalog.Fill(tool, binding, answers, flags, e)
				if err != nil {
					t.Fatalf("%s combo %d: %v", tool.Name, n, err)
				}
				for i, tok := range got.Argv {
					if !allowed[tok] {
						t.Fatalf("%s combo %d: token %q is not authored anywhere in the catalog (argv: %v)",
							tool.Name, n, tok, got.Argv)
					}
					if i == 0 && isShell(tok) {
						t.Fatalf("%s combo %d: argv[0] is a shell (%q)", tool.Name, n, tok)
					}
				}
				if len(got.Unfilled) == 0 && len(got.Argv) == 0 {
					t.Fatalf("%s combo %d: filled with nothing unfilled but produced no argv", tool.Name, n)
				}
			}
		}
		t.Logf("%s: %d combinations, all tokens authored", tool.Name, len(combos))
	}
}

// allOptionTokens is every option the program documents, as the flag stage could
// hand them over.
func allOptionTokens(binding catalog.Binding) []string {
	out := make([]string, 0, len(binding.Options))
	for _, option := range binding.Options {
		if len(option.Flags) > 0 {
			out = append(out, option.Flags[0])
		}
	}
	return out
}

func isShell(program string) bool {
	switch program {
	case "sh", "bash", "zsh", "dash", "ksh", "csh", "tcsh", "fish", "env", "xargs", "eval", "exec", "sudo":
		return true
	}
	return false
}

func TestNoToolCanReachAShell(t *testing.T) {
	e := testEnv(t)
	for _, tool := range catalog.All() {
		for _, needs := range tool.Needs {
			if isShell(needs) {
				t.Errorf("%s declares a shell as a dependency: %q", tool.Name, needs)
			}
		}
	}
	for _, bin := range catalog.Allowlist(catalog.Bindings(catalog.All(), e), e) {
		if isShell(bin) {
			t.Errorf("the allowlist lets a shell through: %q", bin)
		}
	}
	// And nothing in any template asks a program to run a command string.
	for _, tool := range catalog.All() {
		binding, ok := tool.Bind(e)
		if !ok {
			continue
		}
		for _, tok := range binding.Argv {
			if tok == "-c" {
				t.Errorf("%s has a -c token, which is how a shell is told to run a string", tool.Name)
			}
		}
	}
}

func TestAllowlistOnlyNamesProgramsThatExist(t *testing.T) {
	e := testEnv(t)
	// The allowlist is built from the tools that are actually available, which
	// is what the CLI does, not from every tool the catalog declares.
	allowed := catalog.Allowlist(catalog.Bindings(catalog.Available(catalog.All(), e), e), e)

	reachable := map[string]bool{}
	for _, a := range allowed {
		reachable[a] = true
		if strings.HasPrefix(a, "-") {
			t.Errorf("allowlist contains a flag: %q", a)
		}
		if !e.Has(a) {
			t.Errorf("allowlist names %q, which is not installed here", a)
		}
	}
	// head and tail are reachable through a parameter value rather than a
	// template, so the allowlist has to look inside the values too. grep is
	// absent here because ripgrep is installed, which is the point: the list is
	// exactly what can reach the program position, not everything mentioned.
	for _, want := range []string{"ls", "rg", "cat", "head", "tail", "pwd", "man"} {
		if !reachable[want] {
			t.Errorf("allowlist is missing %q", want)
		}
	}
	if reachable["grep"] {
		t.Error("grep cannot reach argv[0] while ripgrep is installed")
	}
	for _, notAProgram := range []string{".", "src", "main.go", "README.md", "TODO", "-a"} {
		if reachable[notAProgram] {
			t.Errorf("%q is a value, not a program, and must not be allowlisted", notAProgram)
		}
	}

	// A program that disappears from the machine must disappear from the
	// allowlist as well.
	e.Bins["ls"] = false
	for _, a := range catalog.Allowlist(catalog.Bindings(catalog.Available(catalog.All(), e), e), e) {
		if a == "ls" {
			t.Error("a program that is not installed reached the allowlist")
		}
	}
}

func TestSharedQuestionIdsAskTheSameQuestion(t *testing.T) {
	e := testEnv(t)
	type shape struct {
		kind   catalog.Kind
		gated  bool
		values string
	}
	type owned struct {
		shape shape
		tool  string
	}
	seen := map[string]owned{}

	// The shaped tools and the ones built from the machine's own commands: a
	// question id has to mean the same thing whichever tool asks it.
	tools := append(catalog.All(), catalog.NamedProgram(discover.Program{Name: "zed"}))
	for _, tool := range tools {
		binding, ok := tool.Bind(e)
		if !ok {
			continue
		}
		for _, param := range binding.Params {
			qid := param.QuestionID(tool.Name)
			keys := make([]string, 0, len(valuesOf(param, e)))
			for _, v := range valuesOf(param, e) {
				keys = append(keys, v.Key)
			}
			sort.Strings(keys)
			now := shape{kind: param.Kind, gated: param.Gated, values: strings.Join(keys, ",")}

			before, ok := seen[qid]
			if !ok {
				seen[qid] = owned{shape: now, tool: tool.Name}
				continue
			}
			if before.shape != now {
				t.Errorf("question %q is declared differently:\n  %s: kind=%v gated=%v values=[%s]\n  %s: kind=%v gated=%v values=[%s]",
					qid, before.tool, before.shape.kind, before.shape.gated, before.shape.values,
					tool.Name, now.kind, now.gated, now.values)
			}
		}
	}
}

func TestQuestionIdsAreDeduplicatedAcrossTools(t *testing.T) {
	e := testEnv(t)
	all := catalog.All()
	questions := catalog.Questions(all, catalog.Bindings(all, e), e)

	// target_path is shared by three tools and must be asked once.
	count := 0
	for id := range questions {
		if id == "target_path" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("target_path asked %d times, want 1", count)
	}
	if _, ok := questions["intent"]; ok {
		t.Error("the tool-choice question is added by resolve, not by Questions")
	}
}

// testDocs is the real documentation of the two programs whose flags the
// catalog discovers, captured from this machine. Injecting it keeps the tests
// hermetic: no process is spawned and no manual is read, but the parameters are
// built from the same text the CLI would read.
func testDocs(t *testing.T) map[string]discover.Docs {
	t.Helper()
	read := func(name string) string {
		raw, err := os.ReadFile("../discover/testdata/" + name)
		if err != nil {
			t.Fatalf("fixture %s: %v", name, err)
		}
		return string(run.StripOverstrike(raw))
	}
	return map[string]discover.Docs{
		"ls": {Program: "ls", Source: "man", Options: discover.ParseMan(read("man-ls.txt"))},
		"rg": {Program: "rg", Source: "help", Options: discover.ParseHelp(read("help-rg.txt"))},
	}
}
