package typesafe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSystemOneSendsTheDocumentedRequest(t *testing.T) {
	var gotPath, gotAuth, gotContentType string
	var gotBody SystemOneRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"model": "jev-1.13.0",
			"answers": {"department": {"type":"choice","choice":"billing",
				"probabilities":{"billing":0.88,"technical":0.12,"sales":0.0},"confidence":0.81}},
			"usage": {"input_tokens": 318, "output_tokens": 34}
		}`)
	}))
	defer server.Close()

	client := NewClient("secret-key", WithBaseURL(server.URL))
	result, err := client.SystemOne(context.Background(), SystemOneRequest{
		State: "My payouts have been failing for 3 days.",
		Questions: map[string]any{
			"department": Choice("Which team should handle this?", map[string]any{
				"billing":   "Charges, invoices, payment problems",
				"technical": "Bugs, outages, integrations",
				"sales":     nil,
			}),
		},
	})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}

	if gotPath != SystemOnePath {
		t.Errorf("path = %q, want %q", gotPath, SystemOnePath)
	}
	if gotAuth != "Bearer secret-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if gotBody.Model != DefaultModel {
		t.Errorf("model = %q, want the default %q", gotBody.Model, DefaultModel)
	}
	if _, ok := gotBody.Questions["department"]; !ok {
		t.Errorf("questions lost the department key: %+v", gotBody.Questions)
	}

	answer := result.Response.Answers["department"]
	if answer.Choice != "billing" {
		t.Errorf("choice = %q, want billing", answer.Choice)
	}
	if answer.Confidence != 0.81 {
		t.Errorf("confidence = %v, want 0.81", answer.Confidence)
	}
	if result.Response.Usage.Total() != 352 {
		t.Errorf("usage total = %d, want 352", result.Response.Usage.Total())
	}
	if result.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", result.Attempts)
	}
	if len(result.Raw) == 0 {
		t.Error("raw response body was not kept")
	}
}

func TestSystemOneRetriesDocumentedTransientStatuses(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":"slow down"}`)
			return
		}
		io.WriteString(w, `{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.95}},"usage":{}}`)
	}))
	defer server.Close()

	client := NewClient("k", WithBaseURL(server.URL))
	result, err := client.SystemOne(context.Background(), SystemOneRequest{
		State:     "state",
		Questions: map[string]any{"q": Noul("Is it urgent?", nil)},
	})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	if result.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", result.Attempts)
	}
	if got := result.Response.Answers["q"].Noul; got != 0.95 {
		t.Errorf("noul = %v, want 0.95", got)
	}
}

func TestSystemOneDoesNotRetryTerminalStatuses(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusUnprocessableEntity} {
		var calls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(status)
			io.WriteString(w, `{"error":"nope"}`)
		}))

		client := NewClient("k", WithBaseURL(server.URL))
		_, err := client.SystemOne(context.Background(), SystemOneRequest{
			State:     "state",
			Questions: map[string]any{"q": Noul("Is it urgent?", nil)},
		})
		server.Close()

		if err == nil {
			t.Fatalf("status %d: expected an error", status)
		}
		if calls != 1 {
			t.Errorf("status %d: calls = %d, want exactly 1", status, calls)
		}
		var apiErr *APIError
		if !asAPIError(err, &apiErr) {
			t.Fatalf("status %d: error is not *APIError: %v", status, err)
		}
		if !apiErr.Retryable() == (status == http.StatusTooManyRequests) {
			t.Errorf("status %d: Retryable() = %v", status, apiErr.Retryable())
		}
	}
}

func TestSystemOneRequiresEveryAnswerBack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only one of the two questions is answered.
		io.WriteString(w, `{"model":"jev-1.13.0","answers":{"a":{"type":"noul","noul":0.5}},"usage":{}}`)
	}))
	defer server.Close()

	client := NewClient("k", WithBaseURL(server.URL))
	_, err := client.SystemOne(context.Background(), SystemOneRequest{
		State: "state",
		Questions: map[string]any{
			"a": Noul("A?", nil),
			"b": Noul("B?", nil),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected a missing-answer error, got %v", err)
	}
}

func TestValidateCatchesShapesThatWouldBeA422(t *testing.T) {
	cases := []struct {
		name string
		req  SystemOneRequest
		want string
	}{
		{
			name: "no state",
			req:  SystemOneRequest{Questions: map[string]any{"q": Noul("x?", nil)}},
			want: "state is required",
		},
		{
			name: "no questions",
			req:  SystemOneRequest{State: "s", Questions: map[string]any{}},
			want: "at least one question",
		},
		{
			name: "single option choice",
			req: SystemOneRequest{State: "s", Questions: map[string]any{
				"q": Choice("x?", map[string]any{"only": "the only one"}),
			}},
			want: "at least 2 options",
		},
		{
			name: "noul without instructions",
			req: SystemOneRequest{State: "s", Questions: map[string]any{
				"q": Noul("  ", nil),
			}},
			want: "instructions are required",
		},
		{
			name: "score with one level",
			req: SystemOneRequest{State: "s", Questions: map[string]any{
				"q": Score("x?", []any{"only"}),
			}},
			want: "between 2 and 10 levels",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestChoiceOptionCeilingIsEnforced(t *testing.T) {
	criteria := map[string]any{}
	for i := 0; i <= MaxChoiceOptions; i++ {
		criteria[string(rune('a'+i%26))+string(rune('a'+i/26))] = nil
	}
	q := Choice("which?", criteria)
	if err := q.Validate(); err == nil {
		t.Fatal("expected the 255-option ceiling to be enforced")
	}
}

func TestAnswerRankingIsOrderedAndDeterministic(t *testing.T) {
	answer := Answer{
		Type: KindChoice,
		Probabilities: map[string]float64{
			"billing": 0.35, "returns": 0.61, "shipping": 0.04, "tie_a": 0.0, "tie_b": 0.0,
		},
	}
	ranking := answer.Ranking()
	want := []string{"returns", "billing", "shipping", "tie_a", "tie_b"}
	for i, key := range want {
		if ranking[i].Key != key {
			t.Fatalf("ranking[%d] = %q, want %q (full: %+v)", i, ranking[i].Key, key, ranking)
		}
	}
}

func TestParseRetryAfterHandlesBothForms(t *testing.T) {
	if got := parseRetryAfter("2"); got.Seconds() != 2 {
		t.Errorf("seconds form: got %v", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Errorf("empty: got %v", got)
	}
	if got := parseRetryAfter("not-a-date"); got != 0 {
		t.Errorf("garbage: got %v", got)
	}
	if got := parseRetryAfter("Wed, 21 Oct 2015 07:28:00 GMT"); got != 0 {
		t.Errorf("past date should clamp to zero, got %v", got)
	}
}

// asAPIError is a tiny local helper so the test does not depend on errors.As
// formatting.
func asAPIError(err error, target **APIError) bool {
	e, ok := err.(*APIError)
	if ok {
		*target = e
	}
	return ok
}
