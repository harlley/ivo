package eval

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The matcher is the part that decides whether an eval run passed, so it is
// worth testing without any model in the loop.
func TestCaseCheckRules(t *testing.T) {
	cases := []struct {
		name    string
		kase    Case
		outcome Outcome
		want    string // substring of the failure, or "" to expect a pass
	}{
		{
			name:    "a matching verdict and argv pass",
			kase:    Case{Verdict: VerdictAct, Command: "ls", Argv: []string{"ls", "."}},
			outcome: Outcome{Verdict: VerdictAct, Command: "ls", Argv: []string{"ls", "."}},
		},
		{
			name:    "a different verdict fails",
			kase:    Case{Verdict: VerdictAct},
			outcome: Outcome{Verdict: VerdictAsk, Reason: "not sure"},
			want:    "verdict is \"ask\"",
		},
		{
			name:    "verdict_any accepts either",
			kase:    Case{VerdictAny: []string{VerdictAsk, VerdictUnsupported}},
			outcome: Outcome{Verdict: VerdictUnsupported},
		},
		{
			name:    "verdict_any rejects a third option",
			kase:    Case{VerdictAny: []string{VerdictAsk, VerdictUnsupported}},
			outcome: Outcome{Verdict: VerdictAct},
			want:    "want ask or unsupported",
		},
		{
			name:    "a different command fails",
			kase:    Case{Command: "rg"},
			outcome: Outcome{Verdict: VerdictAct, Command: "cat"},
			want:    `command is "cat"`,
		},
		{
			name:    "argv order matters",
			kase:    Case{Argv: []string{"ls", "."}},
			outcome: Outcome{Verdict: VerdictAct, Argv: []string{".", "ls"}},
			want:    "argv is",
		},
		{
			name:    "a missing required token fails",
			kase:    Case{Has: []string{"-a"}},
			outcome: Outcome{Verdict: VerdictAct, Argv: []string{"ls", "."}},
			want:    `missing "-a"`,
		},
		{
			name:    "a forbidden token fails",
			kase:    Case{NotHas: []string{"-a"}},
			outcome: Outcome{Verdict: VerdictAct, Argv: []string{"ls", "-a", "."}},
			want:    "should not contain",
		},
		{
			name:    "confidence below the floor fails",
			kase:    Case{Verdict: VerdictAct, MinConfidence: 0.7},
			outcome: Outcome{Verdict: VerdictAct, Confidence: 0.52},
			want:    "confidence 0.52 is below",
		},
		{
			name:    "the confidence floor only applies to acting",
			kase:    Case{Verdict: VerdictAsk, MinConfidence: 0.7},
			outcome: Outcome{Verdict: VerdictAsk, Confidence: 0.2},
		},
		{
			name:    "an empty case accepts anything",
			kase:    Case{},
			outcome: Outcome{Verdict: VerdictBlocked, Argv: []string{"anything"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.kase.Check(tc.outcome)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("Check() = %q, want a pass", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("Check() = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestReportAggregatesRuns(t *testing.T) {
	report := Report{Runs: 2, Results: []Result{
		{Case: Case{Phrase: "a"}, Model: "jev-1", Latency: time.Second, Tokens: 100},
		{Case: Case{Phrase: "a"}, Model: "jev-1", Latency: time.Second, Tokens: 100},
		{Case: Case{Phrase: "b"}, Model: "jev-1", Latency: time.Second, Tokens: 100,
			Outcome: Outcome{Verdict: VerdictAct, Argv: []string{"ls", "."}}},
		{Case: Case{Phrase: "b"}, Model: "jev-1", Latency: time.Second, Tokens: 100,
			Outcome: Outcome{Verdict: VerdictAct, Argv: []string{"ls", "-a", "."}}},
		// A failing case still cost an API call, so it counts towards both
		// totals. A report that hid the cost of its own failures would make
		// the eval look cheaper than it is.
		{Case: Case{Phrase: "c"}, Model: "jev-2", Failure: "wrong verdict",
			Latency: time.Second, Tokens: 100},
	}}

	if got := report.Passed(); got != 4 {
		t.Errorf("Passed() = %d, want 4", got)
	}
	if got := len(report.Failures()); got != 1 {
		t.Errorf("Failures() = %d, want 1", got)
	}
	if got := report.Tokens(); got != 500 {
		t.Errorf("Tokens() = %d, want 500", got)
	}
	if got := report.Latency(); got != 5*time.Second {
		t.Errorf("Latency() = %v, want 5s", got)
	}
	if got := report.Models(); len(got) != 2 || got[0] != "jev-1" || got[1] != "jev-2" {
		t.Errorf("Models() = %v, want [jev-1 jev-2]", got)
	}
	// "a" agreed with itself, "b" flipped between two decisions, "c" was only
	// run once.
	agree, total := report.Agreement()
	if agree != 2 || total != 3 {
		t.Errorf("Agreement() = %d/%d, want 2/3", agree, total)
	}
}

func TestSignatureSeparatesDecisions(t *testing.T) {
	one := Result{Outcome: Outcome{Verdict: VerdictAct, Command: "ls", Argv: []string{"-a", "."}}}
	two := Result{Outcome: Outcome{Verdict: VerdictAct, Command: "ls", Argv: []string{"-a", "."}}}
	three := Result{Outcome: Outcome{Verdict: VerdictAct, Command: "ls", Argv: []string{"."}}}
	if one.Signature() != two.Signature() {
		t.Error("identical decisions should share a signature")
	}
	if one.Signature() == three.Signature() {
		t.Error("different argv should differ in signature")
	}
}

func TestFixtureIsStableAndCleansUp(t *testing.T) {
	dir, cleanup, err := MakeFixture()
	if err != nil {
		t.Fatalf("MakeFixture: %v", err)
	}
	for _, name := range []string{"src", "internal", "main.go", "README.md", "go.mod"} {
		if _, err := os.Stat(dir + "/" + name); err != nil {
			t.Errorf("fixture is missing %s: %v", name, err)
		}
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("cleanup left %s behind", dir)
	}
}

func TestExpectationIsReadable(t *testing.T) {
	c := Case{
		Verdict: VerdictAct, Command: "rg",
		Has: []string{"-g", "*.go"}, NotHas: []string{"-i"}, MinConfidence: 0.7,
	}
	got := c.Expectation()
	for _, want := range []string{"verdict act", "command rg", "contains -g *.go", "without -i", ">= 0.70"} {
		if !strings.Contains(got, want) {
			t.Errorf("Expectation() = %q, missing %q", got, want)
		}
	}
}

func TestReportTableNamesEveryCase(t *testing.T) {
	report := Report{Runs: 1, Results: []Result{
		{Case: Case{Phrase: "list the files"}, Outcome: Outcome{Verdict: VerdictAct, Command: "ls", Argv: []string{"ls", "."}}},
		{Case: Case{Phrase: "delete everything"}, Outcome: Outcome{Verdict: VerdictUnsupported}, Failure: "verdict is unsupported"},
	}}
	out := report.String()
	for _, want := range []string{"list the files", "ok", "delete everything", "FAIL"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
}

func TestCompleteInvocationsRejectMisleadingPartialMatches(t *testing.T) {
	cases := []struct {
		accepted [][]string
		wrong    []string
	}{
		{[][]string{{"git", "diff"}, {"git", "diff", "--"}}, []string{"git", "diff", "changes"}},
		{[][]string{{"ps", "-A", "-f"}}, []string{"ps", "-A", "-o"}},
		{[][]string{{"tar", "-c", "-f", "out.tar", "main.go"}}, []string{"tar", "-f", "tar", "-c", "-f", "out.tar", "main.go"}},
	}
	for _, tc := range cases {
		c := Case{ArgvAny: tc.accepted}
		if c.Check(Outcome{Argv: tc.wrong}) == "" {
			t.Fatalf("accepted incomplete or extra arguments: %v", tc.wrong)
		}
		for _, argv := range tc.accepted {
			if failure := c.Check(Outcome{Argv: argv}); failure != "" {
				t.Fatal(failure)
			}
		}
	}
}
