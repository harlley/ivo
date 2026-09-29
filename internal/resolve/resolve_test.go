package resolve_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/harlley/ivo/internal/catalog"
	"github.com/harlley/ivo/internal/discover"
	"github.com/harlley/ivo/internal/env"
	"github.com/harlley/ivo/internal/model"
	"github.com/harlley/ivo/internal/resolve"
	"github.com/harlley/ivo/internal/run"
)

func testEnv(t *testing.T) *env.Env {
	t.Helper()
	raw, err := os.ReadFile("../discover/testdata/man-ls.txt")
	if err != nil {
		t.Fatal(err)
	}
	return &env.Env{Request: "list the files with details", OS: "darwin", CWD: "/tmp/project", Commands: []string{"ls", "pwd", "rg"},
		Candidates: env.Candidates{Paths: []string{"src"}, Terms: []string{"TODO"}, Patterns: []string{"*.go"}},
		Docs: map[string]discover.Docs{
			"ls":  {Program: "ls", Summary: "List directory contents.", Options: discover.ParseMan(string(run.StripOverstrike(raw)))},
			"pwd": {Program: "pwd", Summary: "Print the working directory."},
			"rg":  {Program: "rg", Summary: "Search text.", Options: []discover.Option{{Flags: []string{"-g"}, Arg: "GLOB", Desc: "Include matching files."}, {Flags: []string{"-e"}, Arg: "PATTERN", Desc: "Search for a pattern."}}},
		}}
}
func plan(t *testing.T) *resolve.Plan {
	t.Helper()
	p, err := resolve.Build(testEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func choice(key string, confidence float64) model.Answer {
	return model.Answer{Type: model.KindChoice, Choice: key, Confidence: confidence, Probabilities: map[string]float64{key: confidence}}
}
func noul(v float64) model.Answer { return model.Answer{Type: model.KindNoul, Noul: v} }
func stageOne(name string, confidence float64) map[string]model.Answer {
	return map[string]model.Answer{"intent": choice(name, confidence), "guardrail.tool_is_clear": noul(.99), "operand?": noul(.99), "operand": choice(".", .99)}
}
func nomination(name string) map[string]model.Answer {
	return map[string]model.Answer{"discover.0": choice(name, .99)}
}

type scriptedAsker struct {
	t        *testing.T
	scripted []map[string]model.Answer
	requests []model.Request
}

func (a *scriptedAsker) Ask(_ context.Context, request model.Request) (*model.Result, error) {
	if request.Model != "" {
		a.t.Fatalf("resolver selected a provider model: %q", request.Model)
	}
	a.requests = append(a.requests, request)
	if err := request.Validate(); err != nil {
		a.t.Fatal(err)
	}
	if len(a.scripted) == 0 {
		a.t.Fatalf("unexpected request %d: %v", len(a.requests), askedIDs(request))
	}
	answers := a.scripted[0]
	a.scripted = a.scripted[1:]
	for id := range answers {
		if _, ok := request.Questions[id]; !ok {
			a.t.Fatalf("request %d does not ask %s: %v", len(a.requests), id, askedIDs(request))
		}
	}
	return &model.Result{Response: &model.Response{Model: "scripted", Answers: answers, Usage: model.Usage{InputTokens: 1}}}, nil
}
func askedIDs(request model.Request) []string {
	var ids []string
	for id := range request.Questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// applyOperandSplit keeps a tool's expected answers together in a test case, but
// sends the operand answers in their separate stage after the program was
// selected.
func applyOperandSplit(answers []map[string]model.Answer) []map[string]model.Answer {
	var stages []map[string]model.Answer
	for _, stage := range answers {
		if _, hasOperand := stage["operand?"]; !hasOperand {
			stages = append(stages, stage)
			continue
		}
		selection, operands := map[string]model.Answer{}, map[string]model.Answer{}
		for id, answer := range stage {
			if strings.HasPrefix(id, "operand") {
				operands[id] = answer
			} else {
				selection[id] = answer
			}
		}
		stages = append(stages, selection, operands)
	}
	return stages
}

func evaluate(t *testing.T, p *resolve.Plan, answers ...map[string]model.Answer) *resolve.Evaluation {
	t.Helper()
	answers = applyOperandSplit(answers)
	asker := &scriptedAsker{t: t, scripted: answers}
	result, err := p.Evaluate(context.Background(), asker, resolve.DecideOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(asker.scripted) != 0 {
		t.Fatalf("%d scripted requests unused", len(asker.scripted))
	}
	if result.Usage.InputTokens != len(answers) {
		t.Fatalf("usage lost a stage: %+v", result)
	}
	return result
}

func TestBuildStartsWithInstalledProgramsAndNoPredefinedTools(t *testing.T) {
	p := plan(t)
	if len(p.Tools) != 0 || len(p.Bindings) != 0 {
		t.Fatal("Build added predefined tools")
	}
	if _, ok := p.Request.Questions["intent"]; ok {
		t.Fatal("tool selection must follow discovery")
	}
	if err := p.Request.Validate(); err != nil {
		t.Fatal(err)
	}
	criteria := p.Request.Questions["discover.0"].(model.ChoiceQuestion).Criteria
	if len(criteria) != len(p.Env.Commands)+1 {
		t.Fatalf("criteria=%v", criteria)
	}
	for _, name := range p.Env.Commands {
		if _, ok := criteria[name]; !ok {
			t.Fatal(name)
		}
	}
	if _, err := resolve.Build(&env.Env{}); err == nil {
		t.Fatal("empty inventory must be reported")
	}
}

func TestTheWalkAccumulatesDocumentedOptions(t *testing.T) {
	p := plan(t)
	result := evaluate(t, p, nomination("ls"), stageOne("ls", .99),
		map[string]model.Answer{"satisfied.0": noul(0), "options.0": choice("-a", .99)},
		map[string]model.Answer{"satisfied.1": noul(0), "options.1": choice("-l", .99)},
		map[string]model.Answer{"satisfied.2": noul(.99)})
	if result.Decision.Verdict != resolve.VerdictAct || strings.Join(result.Decision.Argv, " ") != "ls -a -l ." {
		t.Fatalf("%+v", result.Decision)
	}
}

func TestTheWalkStopsWhenTheCallAlreadySatisfies(t *testing.T) {
	result := evaluate(t, plan(t), nomination("ls"), stageOne("ls", .99), map[string]model.Answer{"satisfied.0": noul(.99), "options.0": choice("-a", .99)})
	if strings.Join(result.Decision.Argv, " ") != "ls ." {
		t.Fatal(result.Decision.Argv)
	}
}

func TestAnInventedOptionNeverReachesTheCommand(t *testing.T) {
	result := evaluate(t, plan(t), nomination("ls"), stageOne("ls", .99),
		map[string]model.Answer{"satisfied.0": noul(0), "options.0": choice("--exec=rm -rf /", 1)},
		map[string]model.Answer{"satisfied.1": noul(.99)})
	if strings.Join(result.Decision.Argv, " ") != "ls ." {
		t.Fatal(result.Decision.Argv)
	}
}

func TestGenericSearchKeepsTextFlagAndPath(t *testing.T) {
	p := plan(t)
	result := evaluate(t, p, nomination("rg"), stageOne("rg", .99),
		map[string]model.Answer{"satisfied.0": noul(0), "options.0": choice("-g *.go", .99)},
		map[string]model.Answer{"satisfied.1": noul(0), "options.1": choice("-e TODO", .99)},
		map[string]model.Answer{"satisfied.2": noul(.99)})
	if strings.Join(result.Decision.Argv, " ") != "rg -g *.go -e TODO ." {
		t.Fatal(result.Decision.Argv)
	}
}

func TestUndocumentedProgramStillNeedsVerification(t *testing.T) {
	e := testEnv(t)
	e.Commands = []string{"custom"}
	e.Docs["custom"] = discover.Docs{Program: "custom"}
	p, err := resolve.Build(e)
	if err != nil {
		t.Fatal(err)
	}
	result := evaluate(t, p, nomination("custom"), map[string]model.Answer{"intent": choice("custom", .99), "guardrail.tool_is_clear": noul(.99), "operand?": noul(0)},
		map[string]model.Answer{catalog.VerifyQuestionID: noul(0)}, map[string]model.Answer{catalog.SideEffectQuestionID: noul(.01)})
	if result.Decision.Verdict != resolve.VerdictAsk {
		t.Fatalf("%+v", result.Decision)
	}
}

func TestDecisionGatesRemainInForceForDiscoveredPrograms(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers map[string]model.Answer
		want    resolve.Verdict
	}{
		{"ambiguous", map[string]model.Answer{"intent": choice("ls", .42)}, resolve.VerdictAsk},
		{"unclear", map[string]model.Answer{"guardrail.tool_is_clear": noul(0)}, resolve.VerdictAsk},
		{"destructive mismatch", map[string]model.Answer{"guardrail.destructive_request": noul(.99)}, resolve.VerdictUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answers := stageOne("ls", .99)
			for id, a := range tc.answers {
				answers[id] = a
			}
			result := evaluate(t, plan(t), nomination("ls"), answers, map[string]model.Answer{"satisfied.0": noul(.99)})
			if result.Decision.Verdict != tc.want {
				t.Fatalf("%+v", result.Decision)
			}
		})
	}
}

func TestANumberCannotDisappearFromTheResolvedCommand(t *testing.T) {
	e := testEnv(t)
	e.Request = "list 3 files"
	p, err := resolve.Build(e)
	if err != nil {
		t.Fatal(err)
	}
	result := evaluate(t, p, nomination("ls"), stageOne("ls", .99), map[string]model.Answer{"satisfied.0": noul(.99)})
	if result.Decision.Verdict != resolve.VerdictAsk || !strings.Contains(result.Decision.Reason, "3") {
		t.Fatalf("%+v", result.Decision)
	}
}

func TestTheWalkStopsAtItsBoundAndVerifiesTheLastAddition(t *testing.T) {
	e := testEnv(t)
	var options []discover.Option
	for i := 0; i < resolve.MaxOptionRounds+1; i++ {
		options = append(options, discover.Option{Flags: []string{fmt.Sprintf("--flag%d", i)}, Desc: "An option."})
	}
	e.Docs["ls"] = discover.Docs{Program: "ls", Options: options}
	p, err := resolve.Build(e)
	if err != nil {
		t.Fatal(err)
	}
	script := []map[string]model.Answer{nomination("ls"), stageOne("ls", .99)}
	for i := 0; i < resolve.MaxOptionRounds; i++ {
		script = append(script, map[string]model.Answer{fmt.Sprintf("satisfied.%d", i): noul(0), fmt.Sprintf("options.%d", i): choice(fmt.Sprintf("--flag%d", i), .99)})
	}
	// The last check says the call is still not the whole answer, and the walk
	// had one option left, so it is asked for by name and the call is read once
	// more. That is the repair, and it is bounded to one.
	script = append(script,
		map[string]model.Answer{catalog.VerifyQuestionID: noul(0)},
		map[string]model.Answer{fmt.Sprintf("options.%d", resolve.MaxOptionRounds): choice(fmt.Sprintf("--flag%d", resolve.MaxOptionRounds), .99)},
		map[string]model.Answer{catalog.VerifyQuestionID: noul(0)},
	)
	asker := &scriptedAsker{t: t, scripted: applyOperandSplit(script)}
	result, err := p.Evaluate(context.Background(), asker, resolve.DecideOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision.Verdict != resolve.VerdictAsk {
		t.Fatalf("%+v", result)
	}
	if result.Rounds != resolve.MaxOptionRounds+1 {
		t.Errorf("rounds = %d, want the walk plus the one repair", result.Rounds)
	}
	if len(result.Flags) != resolve.MaxOptionRounds+1 {
		t.Errorf("flags = %v, want the repaired call even though it is not being offered", result.Flags)
	}
	// The promise is the bound, in requests: the word filter, the tool stage, the
	// operand question, the rounds, the check, the repair, and the check that
	// reads the repaired call.
	if want := 3 + resolve.MaxOptionRounds + 3; len(asker.requests) != want {
		t.Errorf("the model was asked %d times, want %d", len(asker.requests), want)
	}
	if len(asker.scripted) != 0 {
		t.Errorf("%d scripted requests unused", len(asker.scripted))
	}
}

func TestSubcommandVerificationUsesItsOwnDocumentation(t *testing.T) {
	e := testEnv(t)
	e.Request = "use git to show commit patches"
	e.Commands = []string{"git"}
	e.Docs["git"] = discover.Docs{Program: "git", Detail: "Root command documentation.", Subcommands: []discover.Subcommand{{Name: "log", Desc: "Show commit history."}}}
	e.Docs["git log"] = discover.Docs{Program: "git log", Detail: "Patch output requires -p.", Options: []discover.Option{{Flags: []string{"-p"}, Desc: "Show patches."}}}
	p, err := resolve.Build(e)
	if err != nil {
		t.Fatal(err)
	}
	asker := &scriptedAsker{t: t, scripted: []map[string]model.Answer{
		nomination("git"),
		{"intent": choice("git", .99), "guardrail.tool_is_clear": noul(.99)},
		{"operand?": noul(0)},
		{"satisfied.0": noul(0), "options.0": choice("log", .99)},
		{"satisfied.1": noul(0), "options.1": choice("-p", .99)},
		{"satisfied.2": noul(.99)},
		{catalog.SideEffectQuestionID: noul(.01)},
	}}
	result, err := p.Evaluate(context.Background(), asker, resolve.DecideOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.Decision.Argv, " ") != "git log -p" {
		t.Fatal(result.Decision.Argv)
	}
	state := asker.requests[len(asker.requests)-1].State.(map[string]any)
	selected := state["selected_program"].(map[string]any)
	if selected["name"] != "git log" || selected["documentation"] != "Patch output requires -p." {
		t.Fatalf("verification still saw root documentation: %v", selected)
	}
}

func TestWeakNarrowChoiceWidensBeforeAppending(t *testing.T) {
	e := testEnv(t)
	e.Request = "list files"
	p, err := resolve.Build(e)
	if err != nil {
		t.Fatal(err)
	}
	asker := &scriptedAsker{t: t, scripted: []map[string]model.Answer{
		nomination("ls"),
		{"intent": choice("ls", .99), "guardrail.tool_is_clear": noul(.99)},
		{"operand?": noul(.99), "operand": choice(".", .99)},
		{"satisfied.0": noul(0), "options.0": choice("-a", .3)},
		{"satisfied.1": noul(.99)},
	}}
	result, err := p.Evaluate(context.Background(), asker, resolve.DecideOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Flags) != 0 {
		t.Fatalf("accepted an uncertain option: %v", result.Flags)
	}
}
