package resolve_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/harlley/ivo/internal/env"
	"github.com/harlley/ivo/internal/model"
	"github.com/harlley/ivo/internal/resolve"
	"github.com/harlley/ivo/internal/typesafe"
)

type step struct {
	ids          []string
	bytes        int
	inputTokens  int
	outputTokens int
}

type recorder struct {
	client  *typesafe.Client
	steps   []step
	answers []map[string]model.Answer
}

func (r *recorder) Ask(ctx context.Context, request model.Request) (*model.Result, error) {
	result, err := r.client.SystemOne(ctx, request)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(request.Questions))
	for id := range request.Questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	body, _ := json.Marshal(request)
	r.steps = append(r.steps, step{ids, len(body),
		result.Response.Usage.InputTokens, result.Response.Usage.OutputTokens})
	r.answers = append(r.answers, result.Response.Answers)
	return result, nil
}

// TestUsageBreakdown prints what each stage of a call costs, which is the
// measurement the tiers were built from: the discovery request used to be three
// quarters of a call, and the only way to know whether a change helped is to
// count the tokens per stage.
//
// It talks to the real API, so it is opt-in: IVO_USAGE=1 and a key, and
// `go test ./...` never spends anything.
func TestUsageBreakdown(t *testing.T) {
	if os.Getenv("IVO_USAGE") != "1" {
		t.Skip("set IVO_USAGE=1 to measure what a call costs")
	}
	key := typesafe.NewClient(mustKey(t))
	for _, phrase := range []string{
		"how much memory this computer has available?",
		"list all files in this directory",
		"use git to show the last 2 commits as a graph",
	} {
		rec := &recorder{client: key}
		probed, err := env.Probe(env.ProbeOptions{Request: phrase, MaxEntries: 120, DiscoverCommands: true})
		if err != nil {
			t.Fatal(err)
		}
		plan, err := resolve.Build(probed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := plan.Evaluate(context.Background(), rec, resolve.DecideOptions{}); err != nil {
			t.Fatal(err)
		}
		t.Logf("--- %s", phrase)
		for i, answers := range rec.answers {
			for id, answer := range answers {
				if strings.HasPrefix(id, "discover.") && i > 0 {
					continue
				}
				t.Logf("    req%d %s: choice=%q noul=%.2f conf=%.2f", i+1, id, answer.Choice, answer.Noul, answer.Confidence)
			}
		}
		total := 0
		for i, s := range rec.steps {
			total += s.inputTokens
			ids := fmt.Sprint(s.ids)
			if len(ids) > 78 {
				ids = ids[:78] + "..."
			}
			t.Logf("  %2d. %6.1f kB  in=%-7d %s", i+1, float64(s.bytes)/1024, s.inputTokens, ids)
		}
		t.Logf("  total de entrada: %d tokens", total)
	}
}

func mustKey(t *testing.T) string {
	t.Helper()
	key := ""
	if v := os.Getenv("TYPESAFE_API_KEY"); v != "" {
		key = v
	}
	if key == "" {
		t.Skip("no TYPESAFE_API_KEY")
	}
	return key
}
