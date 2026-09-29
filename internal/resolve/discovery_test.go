package resolve_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/harlley/ivo/internal/catalog"
	"github.com/harlley/ivo/internal/discover"
	"github.com/harlley/ivo/internal/model"
	"github.com/harlley/ivo/internal/resolve"
)

func TestDiscoveryFindsAnInstalledProgramNotNamedInTheRequest(t *testing.T) {
	e := testEnv(t)
	e.Request = "how much memory this computer has available?"
	e.Commands = []string{"ls", "vm_stat"}
	e.Docs["vm_stat"] = discover.Docs{Program: "vm_stat", Summary: "Show virtual memory statistics."}
	p, err := resolve.Build(e)
	if err != nil {
		t.Fatal(err)
	}
	result := evaluate(t, p, nomination("vm_stat"), map[string]model.Answer{"intent": choice("vm_stat", .99), "guardrail.tool_is_clear": noul(.99), "operand?": noul(0)},
		map[string]model.Answer{catalog.VerifyQuestionID: noul(.99)}, map[string]model.Answer{catalog.SideEffectQuestionID: noul(.01)})
	if result.Decision.Verdict != resolve.VerdictAct || strings.Join(result.Decision.Argv, " ") != "vm_stat" {
		t.Fatalf("%+v", result.Decision)
	}
	if len(result.Programs) != 1 || result.Programs[0] != "vm_stat" {
		t.Fatal(result.Programs)
	}
}

func TestDiscoverySearchesEveryBatchAndRejectsUnofferedAnswers(t *testing.T) {
	e := testEnv(t)
	e.Commands = nil
	for i := 0; i < 300; i++ {
		e.Commands = append(e.Commands, fmt.Sprintf("program%03d", i))
	}
	p, err := resolve.Build(e)
	if err != nil {
		t.Fatal(err)
	}
	asker := &scriptedAsker{t: t, scripted: []map[string]model.Answer{{"discover.0": choice("program299", .99), "discover.1": choice("invented", .99), "discover.2": choice(catalog.NoneKey, 1)}}}
	result, err := p.Evaluate(context.Background(), asker, resolve.DecideOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision.Verdict != resolve.VerdictUnsupported || len(result.Programs) != 0 {
		t.Fatalf("%+v", result)
	}
	var offered []string
	for _, q := range asker.requests[0].Questions {
		if c, ok := q.(model.ChoiceQuestion); ok {
			for name := range c.Criteria {
				if name != catalog.NoneKey {
					offered = append(offered, name)
				}
			}
		}
	}
	sort.Strings(offered)
	if strings.Join(offered, ",") != strings.Join(e.Commands, ",") {
		t.Fatal("discovery truncated the command list")
	}
}

func TestDiscoveryRejectsInjectionBeforeInspectingPrograms(t *testing.T) {
	result := evaluate(t, plan(t), map[string]model.Answer{"discover.0": choice("ls", .99), "guardrail.injection": noul(.99)})
	if result.Decision.Verdict != resolve.VerdictBlocked || len(result.Programs) != 0 {
		t.Fatalf("%+v", result)
	}
}

// TestDiscoveryHonorsNone is the refusal: no batch named a program this machine
// has, so there is nothing to ask the tool question about.
func TestDiscoveryHonorsNone(t *testing.T) {
	result := evaluate(t, plan(t), map[string]model.Answer{"discover.0": choice(catalog.NoneKey, 1)})
	if result.Decision.Verdict != resolve.VerdictUnsupported || len(result.Programs) != 0 {
		t.Fatalf("%+v", result)
	}
}

// TestAWeakNominationIsNotARefusal is the difference between naming a program
// and being sure of it. The nominated program is carried to the question that
// shows it with its own documentation, and the gate that decides whether to run
// it is there: the same answer at 0.2 is an ask rather than an unsupported,
// because the model did name something this machine has.
//
// Folding the two together is what made "how much memory is available" answer
// vm_stat at 0.44 and then spend nineteen thousand tokens asking about every
// command on the machine.
func TestAWeakNominationIsNotARefusal(t *testing.T) {
	result := evaluate(t, plan(t),
		map[string]model.Answer{"discover.0": choice("ls", .2)},
		map[string]model.Answer{"intent": choice("ls", .2), "guardrail.tool_is_clear": noul(.9), "operand?": noul(0)},
		map[string]model.Answer{"satisfied.0": noul(.99), "options.0": choice(catalog.NoneKey, .9)})

	if result.Decision.Verdict == resolve.VerdictUnsupported {
		t.Fatal("a program the model named was treated as a refusal")
	}
	if len(result.Programs) != 1 || result.Programs[0] != "ls" {
		t.Fatalf("programs = %v, want the nomination carried to the tool question", result.Programs)
	}
	if result.Decision.Verdict != resolve.VerdictAsk {
		t.Errorf("verdict = %q, want ask: the nomination was weak and so was the choice", result.Decision.Verdict)
	}
}

// TestANamedProgramLeadsTheDiscoveryCandidates is the cheapest signal there is:
// a word of the phrase is already a command installed here. It does not decide
// anything, because "search for TODO" uses a word that happens to be a program,
// and "show me the manual for rg" names one program and means another. It leads
// the list the discovery question is asked about, and the question still asks
// which program fits.
func TestANamedProgramLeadsTheDiscoveryCandidates(t *testing.T) {
	e := testEnv(t)
	e.Request = "use git to list the last 3 commits"
	e.Commands = []string{"git"}
	for i := 0; i < 300; i++ {
		e.Commands = append(e.Commands, fmt.Sprintf("zzz%03d", i))
	}
	sort.Strings(e.Commands)

	p, err := resolve.Build(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Named) != 1 || p.Named[0] != "git" {
		t.Fatalf("named = %v, want git", p.Named)
	}
	if len(p.Tools) != 0 {
		t.Fatalf("tools = %v, want none before the question is asked", p.Tools)
	}

	offered := map[string]bool{}
	for _, question := range p.Request.Questions {
		choice, ok := question.(model.ChoiceQuestion)
		if !ok {
			continue
		}
		for name := range choice.Criteria {
			offered[name] = true
		}
	}
	// The order candidates are added in does not survive the JSON object the
	// question is made of, so what the tier has to guarantee is that the named
	// command is inside the cap.
	if !offered["git"] {
		t.Error("the command the request names did not survive the cap")
	}
	if len(offered) >= len(e.Commands) {
		t.Errorf("the whole list of %d commands was offered anyway", len(offered))
	}
}

// TestDiscoveryNarrowsTheListAndKeepsThePriority is the second tier. A machine
// with more commands than fit in one question is asked about the commands it is
// likely to be asked for, and the rest stays reachable through the fallback.
func TestDiscoveryNarrowsTheListAndKeepsThePriority(t *testing.T) {
	e := testEnv(t)
	e.Commands = []string{"ls", "pwd", "rg"}
	for i := 0; i < 300; i++ {
		e.Commands = append(e.Commands, fmt.Sprintf("program%03d", i))
	}
	sort.Strings(e.Commands)

	p, err := resolve.Build(e)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Narrowed {
		t.Fatal("a list of three hundred commands should be asked about in tiers")
	}
	offered := map[string]bool{}
	for _, question := range p.Request.Questions {
		choice, ok := question.(model.ChoiceQuestion)
		if !ok {
			continue
		}
		for name := range choice.Criteria {
			if name != catalog.NoneKey {
				offered[name] = true
			}
		}
	}
	for _, want := range []string{"ls", "pwd", "rg"} {
		if !offered[want] {
			t.Errorf("%s is a command this machine has and the priority list names, and it was not offered", want)
		}
	}
	if len(offered) >= len(e.Commands) {
		t.Errorf("the whole list of %d commands was offered anyway", len(offered))
	}
}

// TestAnEmptyTierFallsBackToTheWholeList: a machine with none of the priority
// commands and none of the words in its manuals is exactly the machine the tier
// cannot help, and the answer is the list it used to send every time.
func TestAnEmptyTierFallsBackToTheWholeList(t *testing.T) {
	e := testEnv(t)
	e.Commands = nil
	for i := 0; i < 300; i++ {
		e.Commands = append(e.Commands, fmt.Sprintf("zzz%03d", i))
	}
	p, err := resolve.Build(e)
	if err != nil {
		t.Fatal(err)
	}
	if p.Narrowed {
		t.Fatal("nothing matched, so the list was not narrowed")
	}
	offered := 0
	for _, question := range p.Request.Questions {
		if choice, ok := question.(model.ChoiceQuestion); ok {
			offered += len(choice.Criteria) - 1
		}
	}
	if offered != len(e.Commands) {
		t.Errorf("offered %d commands, want all %d", offered, len(e.Commands))
	}
}
