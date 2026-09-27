package resolve_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/harlleyoliveira/jev-cli/internal/catalog"
	"github.com/harlleyoliveira/jev-cli/internal/env"
	"github.com/harlleyoliveira/jev-cli/internal/resolve"
	"github.com/harlleyoliveira/jev-cli/internal/typesafe"
)

func testEnv() *env.Env {
	bins := map[string]bool{}
	for _, b := range catalog.Binaries() {
		bins[b] = true
	}
	return &env.Env{
		Request: "list all files in this directory",
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
		Candidates: env.Candidates{Terms: []string{"TODO"}, Patterns: []string{"*.go"}},
	}
}

func plan(t *testing.T, opts resolve.Options) *resolve.Plan {
	t.Helper()
	p, err := resolve.Build(testEnv(), opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return p
}

func TestBuildProducesAValidRequest(t *testing.T) {
	p := plan(t, resolve.Options{})
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
	p := plan(t, resolve.Options{})

	cases := []struct {
		name     string
		answers  map[string]typesafe.Answer
		opts     resolve.DecideOptions
		verdict  resolve.Verdict
		wantArgv string
		// wantSuggestion marks the verdicts that should still carry the
		// tool's best reading of the phrase, for display only.
		wantSuggestion bool
	}{
		{
			name: "a clear read-only request acts",
			answers: map[string]typesafe.Answer{
				"intent":                 choice("list_directory", 0.94),
				"guardrail.intent_clear": {Type: typesafe.KindNoul, Noul: 0.95},
			},
			verdict:  resolve.VerdictAct,
			wantArgv: "ls .",
		},
		{
			name: "guardrail flags add their tokens",
			answers: map[string]typesafe.Answer{
				"intent":                    choice("list_directory", 0.94),
				"guardrail.intent_clear":    {Type: typesafe.KindNoul, Noul: 0.95},
				"list_directory.ls_hidden":  {Type: typesafe.KindNoul, Noul: 0.9},
				"list_directory.ls_hidden?": {Type: typesafe.KindNoul, Noul: 0.9},
				"list_directory.ls_long":    {Type: typesafe.KindNoul, Noul: 0.9},
				"list_directory.ls_long?":   {Type: typesafe.KindNoul, Noul: 0.9},
			},
			verdict:  resolve.VerdictAct,
			wantArgv: "ls -a -l .",
		},
		{
			name: "low confidence asks instead of acting",
			answers: map[string]typesafe.Answer{
				"intent":                 choice("list_directory", 0.42),
				"guardrail.intent_clear": {Type: typesafe.KindNoul, Noul: 0.95},
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
				"guardrail.intent_clear": {Type: typesafe.KindNoul, Noul: 0.95},
			},
			verdict:        resolve.VerdictUnsupported,
			wantSuggestion: true,
		},
		{
			name: "a request to change something is refused",
			answers: map[string]typesafe.Answer{
				"intent":                        choice("list_directory", 0.94),
				"guardrail.intent_clear":        {Type: typesafe.KindNoul, Noul: 0.95},
				"guardrail.destructive_request": {Type: typesafe.KindNoul, Noul: 0.80},
			},
			verdict: resolve.VerdictUnsupported,
		},
		{
			name: "a request that tries to escape the catalog is blocked",
			answers: map[string]typesafe.Answer{
				"intent":                 choice("list_directory", 0.94),
				"guardrail.intent_clear": {Type: typesafe.KindNoul, Noul: 0.95},
				"guardrail.injection":    {Type: typesafe.KindNoul, Noul: 0.93},
			},
			verdict: resolve.VerdictBlocked,
		},
		{
			name: "an ambiguous request asks back",
			answers: map[string]typesafe.Answer{
				"intent":                 choice("list_directory", 0.94),
				"guardrail.intent_clear": {Type: typesafe.KindNoul, Noul: 0.10},
			},
			verdict:        resolve.VerdictAsk,
			wantSuggestion: true,
		},
		{
			name: "severe outcomes stop everything",
			answers: map[string]typesafe.Answer{
				"intent":                 choice("list_directory", 0.94),
				"guardrail.intent_clear": {Type: typesafe.KindNoul, Noul: 0.95},
				"guardrail.severity":     {Type: typesafe.KindScore, Score: 3.2},
			},
			verdict: resolve.VerdictBlocked,
		},
		{
			name: "an unresolvable target asks back",
			answers: map[string]typesafe.Answer{
				"intent":                 choice("show_file", 0.94),
				"guardrail.intent_clear": {Type: typesafe.KindNoul, Noul: 0.95},
				"target_path":            choice(catalog.NoneKey, 0.9),
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
			decision, err := p.Decide(&typesafe.SystemOneResponse{Answers: answers}, tc.opts)
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
				// A gated verdict may carry the best reading of the phrase,
				// but only for display: it is never executed, because every
				// caller executes on VerdictAct alone.
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
	p := plan(t, resolve.Options{})
	answers := autoAnswers(p.Request)
	answers["intent"] = typesafe.Answer{
		Type: typesafe.KindChoice, Choice: "list_directory", Confidence: 0.45,
		Probabilities: map[string]float64{
			"list_directory": 0.45,
			"find_files":     0.38,
			"search_text":    0.12,
			catalog.NoneKey:  0.05,
		},
	}
	decision, err := p.Decide(&typesafe.SystemOneResponse{Answers: answers}, resolve.DecideOptions{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(decision.Alternatives) < 2 {
		t.Fatalf("alternatives = %v, want the runners-up", decision.Alternatives)
	}
	if decision.Alternatives[0].Key != "find_files" {
		t.Errorf("first alternative = %q, want find_files", decision.Alternatives[0].Key)
	}
	if decision.Alternatives[0].Description == "" {
		t.Error("alternatives should carry the catalog's own description")
	}
	for _, alt := range decision.Alternatives {
		if alt.Key == catalog.NoneKey {
			t.Error("the escape hatch is not an alternative command")
		}
	}
}

func TestForcedPathSkipsTheQuestionEntirely(t *testing.T) {
	p := plan(t, resolve.Options{Forced: map[string]string{"target_path": "/etc/hosts"}})
	if _, asked := p.Request.Questions["target_path"]; asked {
		t.Fatal("a forced value must not be asked of the model")
	}
	answers := autoAnswers(p.Request)
	answers["intent"] = choice("list_directory", 0.94)
	answers["guardrail.intent_clear"] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0.95}

	decision, err := p.Decide(&typesafe.SystemOneResponse{Answers: answers}, resolve.DecideOptions{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := strings.Join(decision.Argv, " "); got != "ls /etc/hosts" {
		t.Errorf("argv = %q", got)
	}
}

func TestEndToEndThroughTheHTTPClient(t *testing.T) {
	p := plan(t, resolve.Options{})

	answers := autoAnswers(p.Request)
	answers["intent"] = typesafe.Answer{
		Type: typesafe.KindChoice, Choice: "list_directory", Confidence: 0.93,
		Probabilities: map[string]float64{"list_directory": 0.93, "find_files": 0.05, catalog.NoneKey: 0.02},
	}
	answers["target_path"] = choice(".", 0.99)
	answers["list_directory.ls_long"] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0.88}
	answers["list_directory.ls_long?"] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0.9}
	answers["guardrail.injection"] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0.01}
	answers["guardrail.destructive_request"] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0.02}
	answers["guardrail.intent_clear"] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0.97}
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

	decision, err := p.Decide(result.Response, resolve.DecideOptions{})
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

// ---------------------------------------------------------------------------
// The objective's own invariants, asserted directly.
// ---------------------------------------------------------------------------

// TestTheRequestUsesOnlyTheThreePrimitives pins down "using only jev's
// primitives, with no text generation": the wire body has exactly the three
// top-level fields the API defines, and every question is one of noul, choice
// or score with instructions attached. There is no field anywhere in which a
// caller could ask the model to produce a string.
func TestTheRequestUsesOnlyTheThreePrimitives(t *testing.T) {
	p := plan(t, resolve.Options{})

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
			t.Errorf("unexpected top-level field %q, the endpoint takes exactly state, model and questions", key)
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

// TestSilenceDoesNotAddAFlag is the regression test for the first thing the
// real model got wrong. Asked "list all files in this directory", it
// answered the hidden-files flag at p=0.52, just over the 0.5 line, and the
// command came back as `ls -a .`. The request says nothing about hidden
// entries, so the gate must drop the flag even though the flag question itself
// leans yes.
func TestSilenceDoesNotAddAFlag(t *testing.T) {
	p := plan(t, resolve.Options{})
	answers := autoAnswers(p.Request)
	answers["intent"] = choice("list_directory", 1.0)
	answers["target_path"] = choice(".", 1.0)
	answers["guardrail.intent_clear"] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0.96}
	// The real observed readings: the flag leans yes, the gate says the
	// request never brought it up.
	answers["list_directory.ls_hidden"] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0.52}
	answers["list_directory.ls_hidden?"] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0.18}

	decision, err := p.Decide(&typesafe.SystemOneResponse{Answers: answers}, resolve.DecideOptions{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := strings.Join(decision.Argv, " "); got != "ls ." {
		t.Errorf("argv = %q, want %q: silence must not add a flag", got, "ls .")
	}
	for _, note := range decision.Notes {
		if note.Slot == "ls_hidden" && !strings.Contains(note.Detail, "not mentioned") {
			t.Errorf("the note should say the request was silent, got %q", note.Detail)
		}
	}

	// And when the user does bring it up, the flag lands.
	answers["list_directory.ls_hidden?"] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0.93}
	decision, err = p.Decide(&typesafe.SystemOneResponse{Answers: answers}, resolve.DecideOptions{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := strings.Join(decision.Argv, " "); got != "ls -a ." {
		t.Errorf("argv = %q, want %q once the request mentions hidden entries", got, "ls -a .")
	}
}

// TestContentSearchCanBeLimitedToMatchingFiles covers the other real gap: the
// phrase "search for TODO in the go files" ran `rg -e TODO .`, silently
// searching every file instead of the Go files that were asked for.
func TestContentSearchCanBeLimitedToMatchingFiles(t *testing.T) {
	p := plan(t, resolve.Options{})
	answers := autoAnswers(p.Request)
	answers["intent"] = choice("search_text", 0.95)
	answers["search_terms"] = choice("TODO", 1.0)
	answers["target_path"] = choice(".", 0.9)
	answers["guardrail.intent_clear"] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0.96}
	answers["search_text.search_glob"] = choice("*.go", 0.93)
	answers["search_text.search_glob?"] = typesafe.Answer{Type: typesafe.KindNoul, Noul: 0.91}

	decision, err := p.Decide(&typesafe.SystemOneResponse{Answers: answers}, resolve.DecideOptions{})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := strings.Join(decision.Argv, " "); got != "rg -g *.go -e TODO ." {
		t.Errorf("argv = %q, want the file filter to be applied", got)
	}
}

// TestThePlanAsksEveryQuestionAssemblyNeeds closes the gap that let a shared
// question id silently drop a whole question: two commands shared QID
// "target_path" while only one of them was gated, so the gate was never asked
// and assembly failed at run time. The plan and the assembler must agree.
func TestThePlanAsksEveryQuestionAssemblyNeeds(t *testing.T) {
	p := plan(t, resolve.Options{})

	for _, cmd := range p.Commands {
		spec, ok := p.Specs[cmd.ID]
		if !ok {
			t.Fatalf("%s: no spec", cmd.ID)
		}
		for _, slot := range spec.Slots {
			qid := slot.QuestionID(cmd.ID)
			if _, asked := p.Request.Questions[qid]; !asked {
				t.Errorf("%s: the plan never asks %q", cmd.ID, qid)
			}
			if slot.Stated {
				if _, asked := p.Request.Questions[qid+"?"]; !asked {
					t.Errorf("%s: the plan never asks the gate %q", cmd.ID, qid+"?")
				}
			}
		}
	}
}
