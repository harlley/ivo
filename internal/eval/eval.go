// Package eval measures how well the real model resolves a phrase into a
// command.
//
// The deterministic test suite covers everything downstream of "the model
// answered X". It cannot cover whether the model answers X. An eval is the
// opposite kind of check: it runs the real model over a fixed set of phrases
// and compares the decision against what a correct resolution looks like. It is
// slower, it costs tokens, and it is the only thing that catches a threshold
// that stopped fitting the model.
//
// Nothing here executes a command. An eval judges the decision, never the
// effect.
package eval

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/harlley/ivo/internal/model"
)

// Verdict names, mirroring resolve.Verdict without importing it, so a case file
// reads on its own.
const (
	VerdictAct         = "act"
	VerdictAsk         = "ask"
	VerdictUnsupported = "unsupported"
	VerdictBlocked     = "blocked"
)

// Case is one phrase and what a correct resolution of it looks like. Every
// field except Phrase is optional; an empty field places no requirement.
type Case struct {
	Phrase string `json:"phrase"`
	// Why records what the case is protecting, so a future reader knows what
	// breaking it would mean.
	Why string `json:"why,omitempty"`
	// Verdict is the single accepted verdict.
	Verdict string `json:"verdict,omitempty"`
	// VerdictAny lists accepted verdicts when more than one is defensible.
	VerdictAny []string `json:"verdict_any,omitempty"`
	// Command is the catalog entry that must win.
	Command string `json:"command,omitempty"`
	// Argv must match exactly, when set.
	Argv []string `json:"argv,omitempty"`
	// ArgvAny lists complete accepted invocations, allowing equivalent forms.
	ArgvAny [][]string `json:"argv_any,omitempty"`
	// Has requires every listed token to be present.
	Has []string `json:"argv_has,omitempty"`
	// NotHas requires every listed token to be absent.
	NotHas []string `json:"argv_not_has,omitempty"`
	// MinConfidence is the least intent confidence accepted for an acting case.
	MinConfidence float64 `json:"min_confidence,omitempty"`
}

// Outcome is what the pipeline decided for one phrase.
type Outcome struct {
	Verdict    string
	Command    string
	Argv       []string
	Confidence float64
	Reason     string
}

// Check returns an empty string when the outcome satisfies the case, or a
// sentence explaining the first thing that did not.
func (c Case) Check(o Outcome) string {
	accepted := c.VerdictAny
	if len(accepted) == 0 && c.Verdict != "" {
		accepted = []string{c.Verdict}
	}
	if len(accepted) > 0 {
		found := false
		for _, v := range accepted {
			if o.Verdict == v {
				found = true
				break
			}
		}
		if !found {
			return fmt.Sprintf("verdict is %q, want %s (%s)", o.Verdict, strings.Join(accepted, " or "), o.Reason)
		}
	}
	if c.Command != "" && o.Command != c.Command {
		return fmt.Sprintf("command is %q, want %q", o.Command, c.Command)
	}
	if len(c.Argv) > 0 && !sameArgv(o.Argv, c.Argv) {
		return fmt.Sprintf("argv is %v, want %v", o.Argv, c.Argv)
	}
	if len(c.ArgvAny) > 0 {
		matched := false
		for _, argv := range c.ArgvAny {
			if sameArgv(o.Argv, argv) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Sprintf("argv is %v, want one of %v", o.Argv, c.ArgvAny)
		}
	}
	for _, tok := range c.Has {
		if !contains(o.Argv, tok) {
			return fmt.Sprintf("argv %v is missing %q", o.Argv, tok)
		}
	}
	for _, tok := range c.NotHas {
		if contains(o.Argv, tok) {
			return fmt.Sprintf("argv %v should not contain %q", o.Argv, tok)
		}
	}
	if c.MinConfidence > 0 && o.Verdict == VerdictAct && o.Confidence < c.MinConfidence {
		return fmt.Sprintf("confidence %.2f is below the %.2f this case expects", o.Confidence, c.MinConfidence)
	}
	return ""
}

func sameArgv(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// Result is one run of one case.
type Result struct {
	Case    Case
	Outcome Outcome
	// Flags and FlagAnswers record what the flag stage decided, for diagnosis.
	Flags       []string
	FlagAnswers map[string]model.Answer
	Offered     [][]string
	Failure     string
	Latency     time.Duration
	Tokens      int
	Model       string
	RequestID   string
}

// Passed reports whether this single run satisfied the case.
func (r Result) Passed() bool { return r.Failure == "" }

// Signature identifies the decision in a way that ignores how long it took and
// what it cost, so repeated runs can be compared for agreement.
func (r Result) Signature() string {
	return r.Outcome.Verdict + "|" + r.Outcome.Command + "|" + strings.Join(r.Outcome.Argv, " ")
}

// Report aggregates the runs of every case.
type Report struct {
	Results []Result
	// Runs is how many times each case was evaluated.
	Runs int
}

// Passed counts runs that satisfied their case.
func (r Report) Passed() int {
	n := 0
	for _, res := range r.Results {
		if res.Passed() {
			n++
		}
	}
	return n
}

// Failures returns the runs that did not satisfy their case.
func (r Report) Failures() []Result {
	var out []Result
	for _, res := range r.Results {
		if !res.Passed() {
			out = append(out, res)
		}
	}
	return out
}

// Agreement counts cases whose repeated runs all produced the same decision.
// The model is meant to be self-consistent, so a case that flips between two
// answers is a case whose threshold is too close to call.
func (r Report) Agreement() (agree, total int) {
	bySignature := map[string]map[string]bool{}
	for _, res := range r.Results {
		key := res.Case.Phrase
		if bySignature[key] == nil {
			bySignature[key] = map[string]bool{}
		}
		bySignature[key][res.Signature()] = true
	}
	for _, signatures := range bySignature {
		total++
		if len(signatures) == 1 {
			agree++
		}
	}
	return agree, total
}

// Tokens totals the token cost of the whole run.
func (r Report) Tokens() int {
	n := 0
	for _, res := range r.Results {
		n += res.Tokens
	}
	return n
}

// Latency totals the wall-clock time spent waiting on the API.
func (r Report) Latency() time.Duration {
	var d time.Duration
	for _, res := range r.Results {
		d += res.Latency
	}
	return d
}

// Models lists the distinct models that answered, since an alias can resolve to
// different versions between runs.
func (r Report) Models() []string {
	seen := map[string]bool{}
	var out []string
	for _, res := range r.Results {
		if res.Model != "" && !seen[res.Model] {
			seen[res.Model] = true
			out = append(out, res.Model)
		}
	}
	sort.Strings(out)
	return out
}

// String renders the table a human reads after a run.
func (r Report) String() string {
	var b strings.Builder
	width := 0
	for _, res := range r.Results {
		if len(res.Case.Phrase) > width {
			width = len(res.Case.Phrase)
		}
	}
	if width < 20 {
		width = 20
	}

	for i, res := range r.Results {
		if i > 0 && res.Case.Phrase != r.Results[i-1].Case.Phrase {
			b.WriteString("\n")
		}
		status := "ok"
		if !res.Passed() {
			status = "FAIL"
		}
		decision := res.Outcome.Verdict
		if res.Outcome.Command != "" {
			decision += " " + res.Outcome.Command
		}
		fmt.Fprintf(&b, "%-*s  %-11s %-28s %s\n",
			width, res.Case.Phrase, status, decision, strings.Join(res.Outcome.Argv, " "))
	}
	return b.String()
}

// Expectation renders what the case accepted, so a failure can be read next to
// what actually happened.
func (c Case) Expectation() string {
	var parts []string
	switch {
	case len(c.VerdictAny) > 0:
		parts = append(parts, "verdict "+strings.Join(c.VerdictAny, " or "))
	case c.Verdict != "":
		parts = append(parts, "verdict "+c.Verdict)
	}
	if c.Command != "" {
		parts = append(parts, "command "+c.Command)
	}
	if len(c.Argv) > 0 {
		parts = append(parts, "argv exactly "+strings.Join(c.Argv, " "))
	}
	if len(c.ArgvAny) > 0 {
		parts = append(parts, fmt.Sprintf("argv one of %v", c.ArgvAny))
	}
	if len(c.Has) > 0 {
		parts = append(parts, "argv contains "+strings.Join(c.Has, " "))
	}
	if len(c.NotHas) > 0 {
		parts = append(parts, "argv without "+strings.Join(c.NotHas, " "))
	}
	if c.MinConfidence > 0 {
		parts = append(parts, fmt.Sprintf("confidence >= %.2f", c.MinConfidence))
	}
	return strings.Join(parts, ", ")
}
