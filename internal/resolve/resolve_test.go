package resolve_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/harlleyoliveira/jev-cli/internal/catalog"
	"github.com/harlleyoliveira/jev-cli/internal/discover"
	"github.com/harlleyoliveira/jev-cli/internal/env"
	"github.com/harlleyoliveira/jev-cli/internal/resolve"
	"github.com/harlleyoliveira/jev-cli/internal/run"
	"github.com/harlleyoliveira/jev-cli/internal/typesafe"
)

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
		Candidates: env.Candidates{Terms: []string{"TODO"}, Patterns: []string{"*.go"}},
	}
}

func plan(t *testing.T) *resolve.Plan {
	t.Helper()
	p, err := resolve.Build(testEnv(t))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return p
}

func TestBuildProducesAValidRequest(t *testing.T) {
	p := plan(t)
	if err := p.Request.Validate(); err != nil {
		t.Fatalf("request is not valid: %v", err)
	}
	for _, want := range []string{"intent", "target_path", "guardrail.injection", "guardrail.severity"} {
		if _, ok := p.Request.Questions[want]; !ok {
			t.Errorf("request is missing %q", want)
		}
	}
	state, ok := p.Request.State.(map[string]any)
	if !ok {
		t.Fatalf("state is %T", p.Request.State)
	}
	directory := state["directory"].(map[string]any)
	entries := directory["entries"].([]string)
	if len(entries) != 3 || entries[0] != "src/" {
		t.Errorf("entries = %v, want subdirectories first with a trailing slash", entries)
	}
	if directory["entry_count"].(int) != 3 {
		t.Errorf("entry_count = %v, want 3 counted in code", directory["entry_count"])
	}
}

// autoAnswers fills every question with a harmless answer, so a test only has
// to override the ones that matter.
func autoAnswers(req typesafe.SystemOneRequest) map[string]typesafe.Answer {
	out := map[string]typesafe.Answer{}
	for id, q := range req.Questions {
		switch v := q.(type) {
		case typesafe.NoulQuestion:
			out[id] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0}
		case typesafe.ChoiceQuestion:
			keys := make([]string, 0, len(v.Criteria))
			for k := range v.Criteria {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			out[id] = typesafe.Answer{Type: typesafe.KindChoice, Choice: keys[0], Confidence: 1}
		case typesafe.ScoreQuestion:
			out[id] = typesafe.Answer{Type: typesafe.KindScore, Score: 0}
		}
	}
	return out
}

func choice(key string, confidence float64) typesafe.Answer {
	return typesafe.Answer{
		Type:          typesafe.KindChoice,
		Choice:        key,
		Confidence:    confidence,
		Probabilities: map[string]float64{key: confidence},
	}
}

func TestDecideVerdicts(t *testing.T) {
	p := plan(t)

	cases := []struct {
		name     string
		answers  map[string]typesafe.Answer
		verdict  resolve.Verdict
		flags    []string
		wantArgv string
		// wantSuggestion marks the verdicts that should still carry the tool's
		// best reading of the phrase, for display only.
		wantSuggestion bool
	}{
		{
			name: "a clear read-only request acts",
			answers: map[string]typesafe.Answer{
				"intent":                  choice("list_directory", 0.94),
				"guardrail.tool_is_clear": {Type: typesafe.KindNoul, Noul: 0.95},
			},
			verdict:  resolve.VerdictAct,
			wantArgv: "ls .",
		},
		{
			name: "the options chosen in the flag stage land in the command",
			answers: map[string]typesafe.Answer{
				"intent":                  choice("list_directory", 0.94),
				"guardrail.tool_is_clear": {Type: typesafe.KindNoul, Noul: 0.95},
			},
			flags:    []string{"-a"},
			verdict:  resolve.VerdictAct,
			wantArgv: "ls -a .",
		},
		{
			name: "low confidence asks instead of acting",
			answers: map[string]typesafe.Answer{
				"intent":                  choice("list_directory", 0.42),
				"guardrail.tool_is_clear": {Type: typesafe.KindNoul, Noul: 0.95},
			},
			verdict:        resolve.VerdictAsk,
			wantSuggestion: true,
		},
		{
			name: "the escape hatch wins when it is competitive",
			answers: map[string]typesafe.Answer{
				"intent": typesafe.Answer{
					Type: typesafe.KindChoice, Choice: "list_directory", Confidence: 0.55,
					Probabilities: map[string]float64{"list_directory": 0.55, catalog.NoneKey: 0.45},
				},
				"guardrail.tool_is_clear": {Type: typesafe.KindNoul, Noul: 0.95},
			},
			verdict: resolve.VerdictUnsupported,
		},
		{
			name: "a request to change something is refused",
			answers: map[string]typesafe.Answer{
				"intent":                        choice("list_directory", 0.94),
				"guardrail.tool_is_clear":       {Type: typesafe.KindNoul, Noul: 0.95},
				"guardrail.destructive_request": {Type: typesafe.KindNoul, Noul: 0.80},
			},
			verdict: resolve.VerdictUnsupported,
		},
		{
			name: "a request that tries to escape the catalog is blocked",
			answers: map[string]typesafe.Answer{
				"intent":                  choice("list_directory", 0.94),
				"guardrail.tool_is_clear": {Type: typesafe.KindNoul, Noul: 0.95},
				"guardrail.injection":     {Type: typesafe.KindNoul, Noul: 0.93},
			},
			verdict: resolve.VerdictBlocked,
		},
		{
			name: "an ambiguous request asks back",
			answers: map[string]typesafe.Answer{
				"intent":                  choice("list_directory", 0.94),
				"guardrail.tool_is_clear": {Type: typesafe.KindNoul, Noul: 0.10},
			},
			verdict:        resolve.VerdictAsk,
			wantSuggestion: true,
		},
		{
			name: "severe outcomes stop everything",
			answers: map[string]typesafe.Answer{
				"intent":                  choice("list_directory", 0.94),
				"guardrail.tool_is_clear": {Type: typesafe.KindNoul, Noul: 0.95},
				"guardrail.severity":      {Type: typesafe.KindScore, Score: 3.2},
			},
			verdict: resolve.VerdictBlocked,
		},
		{
			name: "an unresolvable target asks back",
			answers: map[string]typesafe.Answer{
				"intent":                  choice("show_file", 0.94),
				"guardrail.tool_is_clear": {Type: typesafe.KindNoul, Noul: 0.95},
				"target_path":             choice(catalog.NoneKey, 0.9),
			},
			verdict: resolve.VerdictAsk,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			answers := autoAnswers(p.Request)
			for k, v := range tc.answers {
				answers[k] = v
			}
			decision, err := p.Decide(&typesafe.SystemOneResponse{Answers: answers}, tc.flags, resolve.DecideOptions{})
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if decision.Verdict != tc.verdict {
				t.Fatalf("verdict = %q, want %q (reason: %s)", decision.Verdict, tc.verdict, decision.Reason)
			}
			if tc.wantArgv != "" {
				if got := strings.Join(decision.Argv, " "); got != tc.wantArgv {
					t.Errorf("argv = %q, want %q", got, tc.wantArgv)
				}
			}
			switch {
			case decision.Verdict == resolve.VerdictAct:
				if len(decision.Argv) == 0 {
					t.Error("an acting verdict must carry a command")
				}
			case tc.wantSuggestion:
				// A gated verdict may carry the best reading of the phrase, but
				// only for display: every caller executes on VerdictAct alone.
				if len(decision.Argv) == 0 {
					t.Error("expected a display-only suggestion alongside the ask")
				}
			default:
				if len(decision.Argv) != 0 {
					t.Errorf("a refused request must not carry even a suggestion: %v", decision.Argv)
				}
			}
			if decision.Reason == "" && decision.Verdict != resolve.VerdictAct {
				t.Error("every non-acting verdict needs a reason")
			}
		})
	}
}

func TestAlternativesAreOfferedOnALowConfidenceAnswer(t *testing.T) {
	p := plan(t)
	answers := autoAnswers(p.Request)
	answers["intent"] = typesafe.Answer{
		Type: typesafe.KindChoice, Choice: "list_directory", Confidence: 0.45,
		Probabilities: map[string]float64{
			"list_directory": 0.45,
			"search_text":    0.38,
			"show_file":      0.12,
			catalog.NoneKey:  0.05,
		},
	}
	decision, err := p.Decide(&typesafe.SystemOneResponse{Answers: answers}, nil, resolve.DecideOptions{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(decision.Alternatives) < 2 {
		t.Fatalf("alternatives = %v, want the runners-up", decision.Alternatives)
	}
	if decision.Alternatives[0].Key != "search_text" {
		t.Errorf("first alternative = %q, want search_text", decision.Alternatives[0].Key)
	}
	if decision.Alternatives[0].Description == "" {
		t.Error("alternatives should carry the catalog's own description")
	}
	for _, alt := range decision.Alternatives {
		if alt.Key == catalog.NoneKey {
			t.Error("the escape hatch is not an alternative command")
		}
		if alt.Key == "list_directory" {
			t.Error("the chosen option is not an alternative to itself")
		}
	}
}

// TestThePlanAsksEveryQuestionAssemblyNeeds closes a gap that once let a shared
// question id silently drop a whole question. The plan and the assembler must
// agree on what has to be answered.
func TestThePlanAsksEveryQuestionAssemblyNeeds(t *testing.T) {
	p := plan(t)

	for _, tool := range p.Tools {
		binding, ok := p.Bindings[tool.Name]
		if !ok {
			t.Fatalf("%s: no binding", tool.Name)
		}
		for _, param := range binding.Params {
			qid := param.QuestionID(tool.Name)
			if _, asked := p.Request.Questions[qid]; !asked {
				t.Errorf("%s: the plan never asks %q", tool.Name, qid)
			}
			if param.Gated {
				if _, asked := p.Request.Questions[qid+"?"]; !asked {
					t.Errorf("%s: the plan never asks the gate %q", tool.Name, qid+"?")
				}
			}
		}
	}
}

// scriptedAsker answers a sequence of requests, so the walk is covered without
// a network and without a model.
type scriptedAsker struct {
	t        *testing.T
	scripted []map[string]typesafe.Answer
	requests []typesafe.SystemOneRequest
}

func (a *scriptedAsker) SystemOne(_ context.Context, request typesafe.SystemOneRequest) (*typesafe.Result, error) {
	a.requests = append(a.requests, request)
	if len(a.scripted) == 0 {
		a.t.Fatalf("unexpected request %d", len(a.requests))
	}
	answers := a.scripted[0]
	a.scripted = a.scripted[1:]
	return &typesafe.Result{Response: &typesafe.SystemOneResponse{Model: "scripted", Answers: answers}}, nil
}

func noul(v float64) typesafe.Answer { return typesafe.Answer{Type: typesafe.KindNoul, Noul: v} }

// stageOne is what a first stage needs to pick a tool and act on it.
func stageOne(tool string, confidence float64) map[string]typesafe.Answer {
	return map[string]typesafe.Answer{
		"intent":                  choice(tool, confidence),
		"guardrail.tool_is_clear": noul(0.95),
		"target_path":             choice(".", 0.97),
	}
}

// TestTheWalkAccumulatesOptionsUntilTheCallSatisfies is the recursion the design
// turns on: the plain call does not satisfy the request, so one option is added,
// and the question is asked again with the call so far.
func TestTheWalkAccumulatesOptionsUntilTheCallSatisfies(t *testing.T) {
	p := plan(t)
	asker := &scriptedAsker{t: t, scripted: []map[string]typesafe.Answer{
		stageOne("list_directory", 0.95),
		// Round one: ls alone is not enough, so add -a.
		{"satisfied.0": noul(0.08), "options.0": choice("-a", 0.94)},
		// Round two: ls -a is still not enough, so add -l.
		{"satisfied.1": noul(0.21), "options.1": choice("-l", 0.90)},
		// Round three: ls -a -l answers it.
		{"satisfied.2": noul(0.93), "options.2": choice(catalog.NoneKey, 0.8)},
	}}

	evaluation, err := p.Evaluate(context.Background(), asker)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if evaluation.Stages != 4 {
		t.Errorf("stages = %d, want the tool stage and three rounds", evaluation.Stages)
	}
	if got := strings.Join(evaluation.Flags, " "); got != "-a -l" {
		t.Errorf("flags = %q, want -a -l", got)
	}
	if got := strings.Join(evaluation.Decision.Argv, " "); got != "ls -a -l ." {
		t.Errorf("argv = %q", got)
	}

	// Each round carries the call built so far, because judging whether it
	// already satisfies the request is the whole question.
	if got := fmt.Sprint(asker.requests[1].Questions["satisfied.0"]); !strings.Contains(got, "ls .") {
		t.Errorf("the first satisfaction question should describe ls ., got %v", got)
	}
	if got := fmt.Sprint(asker.requests[2].Questions["satisfied.1"]); !strings.Contains(got, "ls -a .") {
		t.Errorf("the second should describe ls -a ., got %v", got)
	}
}

func TestTheWalkStopsWhenTheCallIsAlreadyEnough(t *testing.T) {
	p := plan(t)
	asker := &scriptedAsker{t: t, scripted: []map[string]typesafe.Answer{
		stageOne("list_directory", 0.95),
		{"satisfied.0": noul(0.88), "options.0": choice("-a", 0.9)},
	}}

	evaluation, err := p.Evaluate(context.Background(), asker)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(evaluation.Flags) != 0 {
		t.Errorf("flags = %v, want none: the call was already enough", evaluation.Flags)
	}
	if got := strings.Join(evaluation.Decision.Argv, " "); got != "ls ." {
		t.Errorf("argv = %q", got)
	}
}

func TestTheWalkStopsAtTheEscapeHatchAndAtTheRoundLimit(t *testing.T) {
	p := plan(t)
	asker := &scriptedAsker{t: t, scripted: []map[string]typesafe.Answer{
		stageOne("list_directory", 0.95),
		{"satisfied.0": noul(0.10), "options.0": choice(catalog.NoneKey, 0.85)},
	}}
	evaluation, err := p.Evaluate(context.Background(), asker)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(evaluation.Flags) != 0 || evaluation.Rounds != 1 {
		t.Errorf("flags = %v after %d rounds, want none after one: nothing fits",
			evaluation.Flags, evaluation.Rounds)
	}

	// A model that never settles must not turn a call into a conversation.
	scripted := []map[string]typesafe.Answer{stageOne("list_directory", 0.95)}
	for round := 0; round < 6; round++ {
		scripted = append(scripted, map[string]typesafe.Answer{
			fmt.Sprintf("satisfied.%d", round): noul(0.05),
			fmt.Sprintf("options.%d", round):   choice("-a", 0.9),
		})
	}
	evaluation, err = p.Evaluate(context.Background(), &scriptedAsker{t: t, scripted: scripted})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if evaluation.Rounds > 3 {
		t.Errorf("rounds = %d, want the walk bounded at 3", evaluation.Rounds)
	}
	if len(evaluation.Flags) > 3 {
		t.Errorf("flags = %v, want at most one per round", evaluation.Flags)
	}
}

func TestAnOptionTheProgramDoesNotDocumentIsIgnored(t *testing.T) {
	p := plan(t)
	asker := &scriptedAsker{t: t, scripted: []map[string]typesafe.Answer{
		stageOne("list_directory", 0.95),
		{"satisfied.0": noul(0.10), "options.0": choice("--exec=rm -rf /", 0.99)},
	}}
	evaluation, err := p.Evaluate(context.Background(), asker)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(evaluation.Flags) != 0 {
		t.Errorf("flags = %v, want none: the program documents no such option", evaluation.Flags)
	}
}

// TestContentSearchCanBeLimitedToMatchingFiles covers the other real gap: the
// phrase "search for TODO in the go files" ran `rg -e TODO .`, silently
// searching every file instead of the go files that were asked for.
func TestContentSearchCanBeLimitedToMatchingFiles(t *testing.T) {
	p := plan(t)
	answers := autoAnswers(p.Request)
	answers["intent"] = choice("search_text", 0.95)
	answers["search_terms"] = choice("TODO", 1.0)
	answers["target_path"] = choice(".", 0.9)
	answers["guardrail.tool_is_clear"] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0.96}
	answers["search_text.files"] = choice("*.go", 0.93)
	answers["search_text.files?"] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0.91}

	decision, err := p.Decide(&typesafe.SystemOneResponse{Answers: answers}, nil, resolve.DecideOptions{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := strings.Join(decision.Argv, " "); got != "rg -g *.go -e TODO ." {
		t.Errorf("argv = %q, want the file filter applied", got)
	}
}

// ---------------------------------------------------------------------------
// The objective's own invariants, asserted directly.
// ---------------------------------------------------------------------------

// TestTheRequestUsesOnlyTheThreePrimitives pins down "using only jev's
// primitives, with no text generation": the wire body has exactly the three
// top-level fields the API defines, and every question is one of noul, choice
// or score with instructions attached. There is no field anywhere in which a
// caller could ask the model to produce a string.
func TestTheRequestUsesOnlyTheThreePrimitives(t *testing.T) {
	p := plan(t)

	raw, err := json.Marshal(p.Request)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, want := range []string{"state", "model", "questions"} {
		if _, ok := top[want]; !ok {
			t.Errorf("request is missing the %q field", want)
		}
	}
	for key := range top {
		switch key {
		case "state", "model", "questions":
		default:
			t.Errorf("unexpected top-level field %q: the endpoint takes exactly state, model and questions", key)
		}
	}

	var decoded struct {
		Questions map[string]struct {
			Type         string          `json:"type"`
			Instructions json.RawMessage `json:"instructions"`
			Criteria     json.RawMessage `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal questions: %v", err)
	}
	if len(decoded.Questions) < 5 {
		t.Fatalf("only %d questions; the request should carry the whole battery", len(decoded.Questions))
	}

	for id, q := range decoded.Questions {
		switch q.Type {
		case "noul", "choice", "score":
		default:
			t.Errorf("question %q has type %q, which is not one of the three System One primitives", id, q.Type)
		}
		if len(q.Instructions) == 0 {
			t.Errorf("question %q has no instructions", id)
		}
		if q.Type != "noul" && len(q.Criteria) == 0 {
			t.Errorf("question %q has no criteria", id)
		}
	}

	// Every Choice must stay inside the documented 255-option ceiling, or the
	// API answers 422 instead of an answer.
	for id, q := range p.Request.Questions {
		choice, ok := q.(typesafe.ChoiceQuestion)
		if !ok {
			continue
		}
		if len(choice.Criteria) < 2 {
			t.Errorf("choice %q has %d options; a choice needs at least 2", id, len(choice.Criteria))
		}
		if len(choice.Criteria) > typesafe.MaxChoiceOptions {
			t.Errorf("choice %q has %d options, over the %d ceiling", id, len(choice.Criteria), typesafe.MaxChoiceOptions)
		}
	}
}

func TestEndToEndThroughTheHTTPClient(t *testing.T) {
	p := plan(t)

	answers := autoAnswers(p.Request)
	answers["intent"] = typesafe.Answer{
		Type: typesafe.KindChoice, Choice: "list_directory", Confidence: 0.93,
		Probabilities: map[string]float64{"list_directory": 0.93, "search_text": 0.05, catalog.NoneKey: 0.02},
	}
	answers["target_path"] = choice(".", 0.99)

	answers["guardrail.injection"] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0.01}
	answers["guardrail.destructive_request"] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0.02}
	answers["guardrail.tool_is_clear"] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0.97}
	answers["guardrail.severity"] = typesafe.Answer{Type: typesafe.KindScore, Score: 0.1}

	var gotRequest typesafe.SystemOneRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "jev-test",
			"answers": answers,
			"usage":   map[string]int{"input_tokens": 900, "output_tokens": 40},
		})
	}))
	defer server.Close()

	client := typesafe.NewClient("test-key", typesafe.WithBaseURL(server.URL))
	result, err := client.SystemOne(t.Context(), p.Request)
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if len(gotRequest.Questions) != len(p.Request.Questions) {
		t.Errorf("server saw %d questions, want %d", len(gotRequest.Questions), len(p.Request.Questions))
	}

	decision, err := p.Decide(result.Response, []string{"-l"}, resolve.DecideOptions{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Verdict != resolve.VerdictAct {
		t.Fatalf("verdict = %q (%s)", decision.Verdict, decision.Reason)
	}
	if got := strings.Join(decision.Argv, " "); got != "ls -l ." {
		t.Errorf("argv = %q, want %q", got, "ls -l .")
	}
	if result.Response.Usage.InputTokens != 900 {
		t.Errorf("usage did not survive the round trip: %+v", result.Response.Usage)
	}
}

// testDocs mirrors the catalog test: the real documentation of the programs
// whose flags are discovered, so no process is spawned here either.
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
