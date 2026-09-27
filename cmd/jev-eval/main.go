// Command jev-eval measures how well the real model resolves phrases into
// commands.
//
// It is the counterpart of the deterministic test suite. The tests cover what
// happens once the model has answered; the eval covers whether the model
// answers well, which no fake can tell you.
//
//	go run ./cmd/jev-eval                 # one run over evals/cases.json
//	go run ./cmd/jev-eval -n 3            # three runs, to see agreement
//	go run ./cmd/jev-eval -cases my.json
//
// It needs TYPESAFE_API_KEY. It never executes a command: an eval judges the
// decision, never the effect. Exit code 1 means at least one case failed.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/harlleyoliveira/jev-cli/internal/catalog"
	"github.com/harlleyoliveira/jev-cli/internal/env"
	"github.com/harlleyoliveira/jev-cli/internal/eval"
	"github.com/harlleyoliveira/jev-cli/internal/resolve"
	"github.com/harlleyoliveira/jev-cli/internal/typesafe"
)

// maxEntries matches the CLI, so the eval sees the same state a user would.
const maxEntries = 120

func main() { os.Exit(run()) }

func run() int {
	casesPath := flag.String("cases", "evals/cases.json", "path to the case file")
	runs := flag.Int("n", 1, "how many times to evaluate each case")
	timeout := flag.Duration("timeout", 30*time.Second, "timeout for one API call")
	flag.Parse()

	key := strings.TrimSpace(os.Getenv(typesafe.EnvAPIKey))
	if key == "" {
		fmt.Fprintf(os.Stderr, "jev-eval: set %s first\n", typesafe.EnvAPIKey)
		return 2
	}
	if *runs < 1 {
		fmt.Fprintln(os.Stderr, "jev-eval: -n must be at least 1")
		return 2
	}

	cases, err := loadCases(*casesPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jev-eval: %v\n", err)
		return 2
	}
	if len(cases) == 0 {
		fmt.Fprintln(os.Stderr, "jev-eval: the case file is empty")
		return 2
	}

	// A fixed fixture makes two runs comparable: the model's answer depends on
	// the state it is given, and the state includes the directory listing.
	fixture, cleanup, err := eval.MakeFixture()
	if err != nil {
		fmt.Fprintf(os.Stderr, "jev-eval: could not build the fixture: %v\n", err)
		return 1
	}
	defer cleanup()

	origin, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "jev-eval: %v\n", err)
		return 1
	}
	defer os.Chdir(origin)
	if err := os.Chdir(fixture); err != nil {
		fmt.Fprintf(os.Stderr, "jev-eval: %v\n", err)
		return 1
	}

	client := typesafe.NewClient(key, typesafe.WithBaseURL(baseURL()))
	report := eval.Report{Runs: *runs}

	fmt.Printf("jev-eval: %d cases, %d run(s) each, %s\n\n", len(cases), *runs, baseURL())
	for i := 0; i < *runs; i++ {
		for _, c := range cases {
			report.Results = append(report.Results, evaluate(client, c, *timeout))
		}
	}

	fmt.Println(report.String())

	for _, failure := range report.Failures() {
		fmt.Printf("FAIL %s\n", failure.Case.Phrase)
		if failure.Case.Why != "" {
			fmt.Printf("     why it matters: %s\n", failure.Case.Why)
		}
		fmt.Printf("     expected: %s\n", failure.Case.Expectation())
		fmt.Printf("     got:      %v (%s)\n", failure.Outcome.Argv, failure.Failure)
		fmt.Printf("     flags:    %v %s\n", failure.Flags, formatFlagAnswers(failure.FlagAnswers))
	}

	agree, total := report.Agreement()
	models := report.Models()
	model := "unknown model"
	if len(models) > 0 {
		model = strings.Join(models, ", ")
	}
	fmt.Printf("\n%d/%d runs passed, agreement %d/%d cases, %.1fs, %d tokens, %s\n",
		report.Passed(), len(report.Results), agree, total,
		report.Latency().Seconds(), report.Tokens(), model)

	if len(report.Failures()) > 0 {
		return 1
	}
	return 0
}

// evaluate runs one case through the same pipeline the CLI uses, without ever
// executing the resolved command.
func evaluate(client *typesafe.Client, c eval.Case, timeout time.Duration) eval.Result {
	result := eval.Result{Case: c}

	probed, err := env.Probe(env.ProbeOptions{
		Request:    c.Phrase,
		MaxEntries: maxEntries,
		Binaries:   catalog.Binaries(),
		// The programs to mine: without this the flag stage has nothing to
		// offer, and the eval would silently measure a smaller layer than the
		// CLI runs.
		Document: catalog.Documented(catalog.All()),
		// The commands this machine can run are candidates too.
		DiscoverCommands: true,
	})
	if err != nil {
		result.Failure = err.Error()
		return result
	}
	plan, err := resolve.Build(probed)
	if err != nil {
		result.Failure = err.Error()
		return result
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	evaluation, err := plan.Evaluate(ctx, client, resolve.DecideOptions{})
	if err != nil {
		result.Failure = err.Error()
		return result
	}
	decision := evaluation.Decision

	outcome := eval.Outcome{
		Verdict:    string(decision.Verdict),
		Argv:       decision.Argv,
		Confidence: decision.Intent.Confidence,
		Reason:     decision.Reason,
	}
	if decision.Tool != nil {
		outcome.Command = decision.Tool.Name
	}
	result.Outcome = outcome
	result.Failure = c.Check(outcome)
	result.Latency = evaluation.Latency
	result.Tokens = evaluation.Usage.Total()
	result.Model = evaluation.Model
	result.Flags = evaluation.Flags
	result.FlagAnswers = evaluation.FlagAnswers
	return result
}

// formatFlagAnswers shows what the flag stage decided, which is the first thing
// to look at when a case misses the option it needed.
func formatFlagAnswers(answers map[string]typesafe.Answer) string {
	if len(answers) == 0 {
		return "(no flag stage ran)"
	}
	keys := make([]string, 0, len(answers))
	for key := range answers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var parts []string
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s(%.2f)",
			key, answers[key].Choice, answers[key].Confidence))
	}
	return strings.Join(parts, " ")
}

func loadCases(path string) ([]eval.Case, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Accept either {"cases": [...]} or a bare array.
	var wrapper struct {
		Cases []eval.Case `json:"cases"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && len(wrapper.Cases) > 0 {
		return wrapper.Cases, nil
	}
	var bare []eval.Case
	if err := json.Unmarshal(raw, &bare); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return bare, nil
}

// baseURL mirrors the CLI: an environment variable rather than a flag, so the
// eval can be pointed at a fake server too.
func baseURL() string {
	if v := strings.TrimSpace(os.Getenv("JEV_BASE_URL")); v != "" {
		return v
	}
	return typesafe.DefaultBaseURL
}
