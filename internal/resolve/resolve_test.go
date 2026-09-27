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
				"intent":                   choice("list_directory", 0.94),
				"guardrail.intent_clear":   {Type: typesafe.KindNoul, Noul: 0.95},
				"list_directory.ls_hidden": {Type: typesafe.KindNoul, Noul: 0.9},
				"list_directory.ls_long":   {Type: typesafe.KindNoul, Noul: 0.9},
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
			verdict: resolve.VerdictAsk,
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
			verdict: resolve.VerdictUnsupported,
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
			verdict: resolve.VerdictAsk,
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
				"target_path":            choice("no_path_filter", 0.9),
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
			if decision.Verdict != resolve.VerdictAct && len(decision.Argv) != 0 {
				t.Errorf("a non-acting verdict must not carry a command: %v", decision.Argv)
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
