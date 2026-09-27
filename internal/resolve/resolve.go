// Package resolve builds the single System One request that turns a natural
// language phrase plus the probed environment into a decision, and reads the
// answers back into a verdict.
//
// All questions go in one request. The model evaluates them in parallel, so
// speculative questions about commands that lose cost tokens but not latency,
// and the code below simply ignores the answers whose command did not win.
package resolve

import (
	"fmt"
	"strings"

	"github.com/harlleyoliveira/jev-cli/internal/catalog"
	"github.com/harlleyoliveira/jev-cli/internal/env"
	"github.com/harlleyoliveira/jev-cli/internal/typesafe"
)

// Thresholds. These are starting points from the TypeSafe documentation, not
// laws of nature: measure them against your own traffic before loosening them.
// Everything here is configurable, and the CLI prints what it used.
const (
	// DefaultMinConfidence gates the command choice. Below it we ask instead
	// of acting.
	DefaultMinConfidence = 0.5

	// DefaultClarityThreshold is how sure the model must be that the request
	// is unambiguous. Low clarity sends the request back to the user.
	DefaultClarityThreshold = 0.35

	// DefaultDestructiveThreshold is the review line for "this request wants
	// to change something". This catalog is read-only, so anything above it is
	// refused rather than executed.
	DefaultDestructiveThreshold = 0.35

	// DefaultInjectionThreshold is the action line for a request that tries to
	// escape the command set.
	DefaultInjectionThreshold = 0.70

	// DefaultSeverityThreshold is the severity level at which we stop.
	DefaultSeverityThreshold = 2.0

	// noneActionThreshold is how much probability the "none of these" option
	// needs before we call the request unsupported even though some command
	// technically ranked first.
	noneActionThreshold = 0.4
)

// Options configures Build.
type Options struct {
	Model string
	// Forced maps a slot id to a literal value supplied by the human on the
	// command line, replacing the model's decision for that slot.
	Forced map[string]string
}

// Plan is everything needed to ask, plus everything needed to interpret the
// answer afterwards.
type Plan struct {
	Env      *env.Env
	Commands []catalog.Command
	Specs    map[string]catalog.Spec
	Request  typesafe.SystemOneRequest
	// Labels maps a question id to option key -> human description, so the
	// CLI can explain a low-confidence answer.
	Labels map[string]map[string]string
	Forced map[string]string
}

// Build probes nothing (env is already probed) and assembles the request.
func Build(e *env.Env, opts Options) (*Plan, error) {
	commands := catalog.Available(catalog.All(), e)
	if len(commands) == 0 {
		return nil, fmt.Errorf("no catalog command is available in this environment")
	}
	specs := catalog.Specs(commands, e)

	questions := map[string]any{}
	labels := map[string]map[string]string{}

	intent := catalog.IntentQuestion(commands)
	questions["intent"] = intent
	labels["intent"] = criteriaLabels(intent)

	for _, cmd := range commands {
		spec, ok := specs[cmd.ID]
		if !ok {
			continue
		}
		for _, slot := range spec.Slots {
			if _, isForced := opts.Forced[slot.ID]; isForced && !slot.IsFlag() {
				// The human already supplied this literal; there is nothing to
				// decide, and the option could not have been in the closed set.
				continue
			}
			qid := slot.QuestionID(cmd.ID)
			if _, seen := questions[qid]; seen {
				continue
			}
			question := slot.AsQuestion(e)
			questions[qid] = question
			labels[qid] = criteriaLabels(question)
			if slot.Stated {
				questions[qid+"?"] = slot.StatedQuestion()
			}
		}
	}
	for qid, question := range catalog.GuardrailQuestions() {
		questions[qid] = question
	}

	model := opts.Model
	if model == "" {
		model = typesafe.DefaultModel
	}
	req := typesafe.SystemOneRequest{State: buildState(e), Model: model, Questions: questions}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	return &Plan{
		Env:      e,
		Commands: commands,
		Specs:    specs,
		Request:  req,
		Labels:   labels,
		Forced:   opts.Forced,
	}, nil
}

// buildState sends a filtered view of the machine. Counting is done here, in
// code, and never asked of the model; the listing is capped and ordered so we
// do not hand the model more context than the questions need.
func buildState(e *env.Env) map[string]any {
	directory := map[string]any{
		"entries":     env.EntryNames(e.Entries),
		"entry_count": e.EntryCount,
		"truncated":   e.Truncated,
	}
	if e.EntriesError != "" {
		directory["error"] = e.EntriesError
	}

	git := map[string]any{"is_repository": false}
	if e.Git.IsRepo {
		git = map[string]any{
			"is_repository":           true,
			"root":                    e.Git.Root,
			"branch":                  e.Git.Branch,
			"has_uncommitted_changes": e.Git.Dirty,
		}
	}

	return map[string]any{
		"request": e.Request,
		"environment": map[string]any{
			"os":    e.OS,
			"cwd":   e.CWD,
			"shell": e.Shell,
		},
		"directory": directory,
		"git":       git,
		"named_in_request": map[string]any{
			"paths":    nonNil(e.Candidates.Paths),
			"patterns": nonNil(e.Candidates.Patterns),
			"terms":    nonNil(e.Candidates.Terms),
		},
	}
}

// nonNil keeps empty lists as [] rather than null, so the state reads the same
// whether or not the request named anything.
func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

// Verdict is what jev-cli decided to do about a phrase.
type Verdict string

// The four possible verdicts.
const (
	// VerdictAct means a command was resolved and may be shown or run.
	VerdictAct Verdict = "act"
	// VerdictAsk means the model was not certain enough, or a required value
	// was not resolvable. Nothing runs; the user is asked.
	VerdictAsk Verdict = "ask"
	// VerdictUnsupported means the request is understood but is outside the
	// closed vocabulary.
	VerdictUnsupported Verdict = "unsupported"
	// VerdictBlocked means a guardrail stopped it.
	VerdictBlocked Verdict = "blocked"
)

// Decision is the interpreted outcome of one evaluation.
type Decision struct {
	Verdict Verdict
	Reason  string

	Command *catalog.Command
	Argv    []string
	Notes   []catalog.Note
	Missing []string

	Intent       typesafe.Answer
	Alternatives []typesafe.RankedOption
	Guardrails   map[string]typesafe.Answer
	Severity     float64
}

// DecideOptions are the gates. Zero values fall back to the documented
// defaults.
type DecideOptions struct {
	MinConfidence        float64
	ClarityThreshold     float64
	DestructiveThreshold float64
	InjectionThreshold   float64
	SeverityThreshold    float64
	AllowWrite           bool
}

func (o DecideOptions) withDefaults() DecideOptions {
	if o.MinConfidence <= 0 {
		o.MinConfidence = DefaultMinConfidence
	}
	if o.ClarityThreshold <= 0 {
		o.ClarityThreshold = DefaultClarityThreshold
	}
	if o.DestructiveThreshold <= 0 {
		o.DestructiveThreshold = DefaultDestructiveThreshold
	}
	if o.InjectionThreshold <= 0 {
		o.InjectionThreshold = DefaultInjectionThreshold
	}
	if o.SeverityThreshold <= 0 {
		o.SeverityThreshold = DefaultSeverityThreshold
	}
	return o
}

// Decide reads the answers. The order of the checks is the order of the
// questions that matter: first "should anything run at all", then "which
// command", then "did every argument resolve".
func (p *Plan) Decide(res *typesafe.SystemOneResponse, opts DecideOptions) (*Decision, error) {
	opts = opts.withDefaults()

	intent, ok := res.Answers["intent"]
	if !ok {
		return nil, fmt.Errorf("the response did not include the question %q", "intent")
	}

	d := &Decision{
		Intent:     intent,
		Guardrails: map[string]typesafe.Answer{},
	}
	for _, qid := range []string{
		"guardrail.injection",
		"guardrail.destructive_request",
		"guardrail.intent_clear",
		"guardrail.severity",
	} {
		if answer, ok := res.Answers[qid]; ok {
			d.Guardrails[qid] = answer
		}
	}
	d.Severity = d.Guardrails["guardrail.severity"].Score
	d.Alternatives = topAlternatives(intent, p.Labels["intent"], 3)

	// 1. A request that tries to rewrite the rules does not get a command,
	//    whatever else it says.
	if injection := d.Guardrails["guardrail.injection"].Noul; injection >= opts.InjectionThreshold {
		d.Verdict = VerdictBlocked
		d.Reason = fmt.Sprintf(
			"the request looks like an attempt to leave the fixed set of commands (p=%.2f, threshold %.2f)",
			injection, opts.InjectionThreshold)
		return d, nil
	}

	// 2. This catalog only reads. A request that asks for a change is refused
	//    rather than answered with something adjacent.
	if destructive := d.Guardrails["guardrail.destructive_request"].Noul; destructive >= opts.DestructiveThreshold {
		d.Verdict = VerdictUnsupported
		d.Reason = fmt.Sprintf(
			"the request involves changing data or the system (p=%.2f), and this CLI only runs read-only commands",
			destructive)
		return d, nil
	}
	if d.Severity >= opts.SeverityThreshold {
		d.Verdict = VerdictBlocked
		d.Reason = fmt.Sprintf("severidade estimada %.2f atingiu o limite %.2f", d.Severity, opts.SeverityThreshold)
		return d, nil
	}

	// 3. Intent. The escape hatch carries real signal: a first place that is
	//    barely ahead of "none of these" is not a decision.
	//
	//    A gate below this line does not end the work: the command is still
	//    resolved so the CLI can show the user its best reading of the phrase.
	//    Seeing "eu ia rodar git diff -- ." is what makes an ask actionable
	//    instead of a dead end. That suggestion is display-only, every caller
	//    executes on VerdictAct and nothing else.
	noneProbability := intent.Probability(catalog.NoneKey)
	switch {
	case intent.Choice == catalog.NoneKey || noneProbability >= noneActionThreshold:
		d.Verdict = VerdictUnsupported
		d.Reason = fmt.Sprintf("no catalog command matches the request (p=%.2f for \"none of these\")", noneProbability)
	case intent.Confidence < opts.MinConfidence:
		d.Verdict = VerdictAsk
		d.Reason = fmt.Sprintf("not sure which command to use (confidence %.2f, minimum %.2f)",
			intent.Confidence, opts.MinConfidence)
	default:
		if clarity := d.Guardrails["guardrail.intent_clear"].Noul; clarity < opts.ClarityThreshold {
			d.Verdict = VerdictAsk
			d.Reason = fmt.Sprintf("the request is too ambiguous to run without confirmation (clarity p=%.2f)", clarity)
		} else {
			d.Verdict = VerdictAct
		}
	}

	cmd, found := p.command(intent.Choice)
	if !found {
		if d.Verdict == VerdictAct {
			return nil, fmt.Errorf("the model chose %q, which is not in the catalog", intent.Choice)
		}
		// The escape hatch won, so there is no command to offer.
		return d, nil
	}
	if !cmd.ReadOnly && !opts.AllowWrite {
		d.Verdict = VerdictBlocked
		d.Reason = "this command is not read-only; use --allow-write to permit it"
		return d, nil
	}

	assembled, err := catalog.Assemble(*cmd, p.Specs[cmd.ID], res.Answers, p.Env, p.Forced)
	if err != nil {
		if d.Verdict == VerdictAct {
			return nil, err
		}
		// We were only going to show a suggestion; failing to build one is
		// not worth failing the whole command over.
		return d, nil
	}
	d.Command = cmd
	d.Notes = assembled.Notes
	d.Missing = assembled.Missing

	if d.Verdict != VerdictAct {
		// Never hand over a half-resolved command, even as a suggestion.
		if len(assembled.Missing) == 0 {
			d.Argv = assembled.Argv
		}
		return d, nil
	}

	if len(assembled.Missing) > 0 {
		d.Verdict = VerdictAsk
		// A half-resolved command is not a command: dropping the argv makes it
		// impossible for any later code path to run it by accident.
		d.Argv = nil
		d.Reason = "could not determine: " + strings.Join(assembled.Missing, ", ") +
			" (give the value explicitly, for example with --path)"
		return d, nil
	}
	if len(assembled.Argv) == 0 {
		return nil, fmt.Errorf("the resolved command is empty")
	}

	d.Argv = assembled.Argv
	d.Verdict = VerdictAct
	return d, nil
}

func (p *Plan) command(id string) (*catalog.Command, bool) {
	for i := range p.Commands {
		if p.Commands[i].ID == id {
			return &p.Commands[i], true
		}
	}
	return nil, false
}

// topAlternatives returns the runner-up options, everything except the option
// that won, and except the escape hatch, so a low-confidence answer can be
// shown as a choice rather than a guess.
func topAlternatives(answer typesafe.Answer, labels map[string]string, n int) []typesafe.RankedOption {
	ranking := answer.Ranking()
	out := make([]typesafe.RankedOption, 0, n)
	for _, opt := range ranking {
		if opt.Key == answer.Choice || opt.Key == catalog.NoneKey || opt.Probability <= 0 {
			continue
		}
		opt.Description = labels[opt.Key]
		out = append(out, opt)
		if len(out) == n {
			break
		}
	}
	return out
}

func criteriaLabels(q typesafe.Question) map[string]string {
	out := map[string]string{}
	choice, ok := q.(typesafe.ChoiceQuestion)
	if !ok {
		return out
	}
	for key, desc := range choice.Criteria {
		switch v := desc.(type) {
		case string:
			out[key] = v
		case map[string]any:
			if what, ok := v["what"].(string); ok {
				out[key] = what
				continue
			}
			out[key] = fmt.Sprint(v)
		}
	}
	return out
}
