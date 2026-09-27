// Package resolve builds the single System One request that turns a natural
// language phrase plus the probed environment into a decision, and reads the
// answers back into a verdict.
//
// All questions go in one request. The model evaluates them in parallel, so
// speculative questions about commands that lose cost tokens but not latency,
// and the code below simply ignores the answers whose command did not win.
package resolve

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/harlleyoliveira/jev-cli/internal/discover"

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

// Plan is everything needed to ask, plus everything needed to interpret the
// answer afterwards.
type Plan struct {
	Env      *env.Env
	Tools    []catalog.Tool
	Bindings map[string]catalog.Binding
	// Request is the tool stage, assembled once the words have been read.
	Request typesafe.SystemOneRequest
	// Labels maps a question id to option key -> human description, so the CLI
	// can explain a low-confidence answer.
	Labels map[string]map[string]string
	// Words are the request's content words. They are the first filter: one
	// question each asks whether a word names a program, which is how the tool
	// space stops being a list written in code.
	Words []string
}

// Build prepares the plan. The tool question is deliberately not assembled yet:
// a word of the request may name a program this machine has, and that program
// becomes one of the options, so the question cannot be written until the words
// have been read.
func Build(e *env.Env) (*Plan, error) {
	tools := catalog.Available(catalog.All(), e)
	if len(tools) == 0 {
		return nil, fmt.Errorf("no tool is available in this environment")
	}
	plan := &Plan{
		Env:      e,
		Tools:    tools,
		Bindings: catalog.Bindings(tools, e),
		Words:    discover.WordCandidates(e.Request),
	}
	// Assembled once with the shaped tools, and again in Evaluate when the word
	// filter has had its say. Assembling is pure work on the catalog, so doing
	// it twice costs nothing and keeps a plan valid on its own.
	if err := plan.assemble(); err != nil {
		return nil, err
	}
	return plan, nil
}

// assemble writes the tool stage: the tool question, the operands it needs, and
// the guardrails.
func (p *Plan) assemble() error {
	questions := map[string]any{}
	labels := map[string]map[string]string{}

	choice := catalog.ToolQuestion(p.Tools)
	questions["intent"] = choice
	labels["intent"] = criteriaLabels(choice)

	// The catalog owns the question set. The plan and the filler therefore
	// cannot disagree about which questions have to be answered, which is a bug
	// that has happened once already.
	for qid, question := range catalog.Questions(p.Tools, p.Bindings, p.Env) {
		questions[qid] = question
		labels[qid] = criteriaLabels(question)
	}
	for qid, question := range catalog.GuardrailQuestions() {
		questions[qid] = question
	}

	request := typesafe.SystemOneRequest{State: buildState(p.Env), Model: typesafe.DefaultModel, Questions: questions}
	if err := request.Validate(); err != nil {
		return err
	}
	p.Request = request
	p.Labels = labels
	return nil
}

// wordRequest asks about every content word at once. The words that come back
// above the line are looked up on this machine, and the ones that exist become
// tools.
func (p *Plan) wordRequest() (typesafe.SystemOneRequest, bool) {
	if len(p.Words) == 0 {
		return typesafe.SystemOneRequest{}, false
	}
	questions := map[string]any{}
	for i, word := range p.Words {
		questions[catalog.WordQuestionID(i)] = catalog.WordQuestion(word)
	}
	return typesafe.SystemOneRequest{
		State:     buildState(p.Env),
		Model:     typesafe.DefaultModel,
		Questions: questions,
	}, true
}

// readWords turns the answer to the word filter into tools. A word the model
// calls a program still has to exist on this machine, so the model can pick a
// command but never invent one.
func (p *Plan) readWords(response *typesafe.SystemOneResponse) []string {
	var named []string
	for i, word := range p.Words {
		answer, ok := response.Answers[catalog.WordQuestionID(i)]
		if !ok || answer.Noul < catalog.WordThreshold {
			continue
		}
		name := p.Env.CommandName(word)
		if name == "" {
			continue
		}
		if _, seen := p.Bindings[name]; seen {
			continue
		}
		described := discover.Describe([]discover.Program{{Name: name, ReadOnly: discover.IsReadOnly(name)}})
		if len(described) == 0 {
			continue
		}
		tool := catalog.NamedProgram(described[0])
		binding, ok := tool.Bind(p.Env)
		if !ok {
			continue
		}
		p.Tools = append(p.Tools, tool)
		p.Bindings[name] = binding
		named = append(named, name)
	}
	return named
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

	return map[string]any{
		"request": e.Request,
		"environment": map[string]any{
			"os":    e.OS,
			"cwd":   e.CWD,
			"shell": e.Shell,
		},
		"directory": directory,
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

// maxOptionRounds bounds the walk over a tool's options. Each round costs one
// request, so the bound is what keeps a call from turning into a conversation.
//
// The last round exists to verify rather than to add: without it a call that
// needs two options ends on an option nobody checked, which is how ls once came
// back as -a -A -l for a request that wanted two of the three.
const maxOptionRounds = 4

// Asker sends one request. *typesafe.Client satisfies it, and so does a script
// in a test, which is how the whole walk is covered without a network.
type Asker interface {
	SystemOne(ctx context.Context, request typesafe.SystemOneRequest) (*typesafe.Result, error)
}

// askFunc sends one request and hands back just the response.
type askFunc func(typesafe.SystemOneRequest) (*typesafe.SystemOneResponse, error)

// Evaluation is the outcome of a whole call: every stage, what it cost, and what
// was decided.
type Evaluation struct {
	Decision *Decision
	Usage    typesafe.Usage
	Latency  time.Duration
	// Stages is how many requests the call needed.
	Stages int
	// Model is the model that answered.
	Model string
	// Flags are the options the walk settled on, and Rounds is how many
	// requests it took to settle.
	Flags  []string
	Rounds int
	// FlagAnswers is what each round answered, kept for diagnosis: when a call
	// misses the option it needed, this is where to look.
	FlagAnswers map[string]typesafe.Answer
	// Programs are the commands the word filter found on this machine, and
	// WordAnswers is what it answered.
	Programs    []string
	WordAnswers map[string]typesafe.Answer
	// SideEffect is the judgment about a call built from a tool that is not
	// read-only, and CallIsSafe is what it decided.
	SideEffect *typesafe.Answer
	CallIsSafe bool
}

// Evaluate runs the stages a call needs. The tool is chosen first, and only
// then are its own documented options walked, one round at a time: each round
// asks whether the call built so far already answers the request, and if not,
// which option to add next. Both questions ride in the same request, so a round
// costs one round trip whatever the answer turns out to be.
func (p *Plan) Evaluate(ctx context.Context, client Asker, opts DecideOptions) (*Evaluation, error) {
	evaluation := &Evaluation{}

	// Stage one: which words of the request name a program? Until that is
	// answered the tool question cannot be written, because a named program is
	// one of its options.
	if request, ok := p.wordRequest(); ok {
		response, err := client.SystemOne(ctx, request)
		if err != nil {
			return nil, err
		}
		evaluation.Usage = response.Response.Usage
		evaluation.Latency = response.Latency
		evaluation.Stages = 1
		evaluation.Model = response.Response.Model
		evaluation.WordAnswers = response.Response.Answers
		evaluation.Programs = p.readWords(response.Response)
	}

	if err := p.assemble(); err != nil {
		return nil, err
	}
	first, err := client.SystemOne(ctx, p.Request)
	if err != nil {
		return nil, err
	}
	evaluation.Usage.InputTokens += first.Response.Usage.InputTokens
	evaluation.Usage.OutputTokens += first.Response.Usage.OutputTokens
	evaluation.Latency += first.Latency
	evaluation.Stages++
	if evaluation.Model == "" {
		evaluation.Model = first.Response.Model
	}

	// One place counts what every further request costs.
	ask := func(request typesafe.SystemOneRequest) (*typesafe.SystemOneResponse, error) {
		result, err := client.SystemOne(ctx, request)
		if err != nil {
			return nil, err
		}
		evaluation.Usage.InputTokens += result.Response.Usage.InputTokens
		evaluation.Usage.OutputTokens += result.Response.Usage.OutputTokens
		evaluation.Latency += result.Latency
		evaluation.Stages++
		return result.Response, nil
	}

	toolName := chosenTool(first.Response)
	if toolName != "" {
		rounds, flags, answers, err := p.walkOptions(toolName, first.Response.Answers, ask)
		if err != nil {
			return nil, err
		}
		evaluation.Rounds = rounds
		evaluation.Flags = flags
		evaluation.FlagAnswers = answers
	}

	// A tool nobody classified has to pass one more question, asked about the
	// call that was built rather than about the request. Being told twice
	// already is a reason not to ask.
	if tool, ok := p.tool(chosenTool(first.Response)); ok && !tool.ReadOnly && !opts.AllowWrite {
		if binding, ok := p.Bindings[tool.Name]; ok {
			asked, err := ask(typesafe.SystemOneRequest{
				State: p.Request.State,
				Model: p.Request.Model,
				Questions: map[string]any{
					catalog.SideEffectQuestionID: catalog.SideEffectQuestion(p.candidate(tool.Name, binding, first.Response.Answers, evaluation.Flags)),
				},
			})
			if err != nil {
				return nil, err
			}
			if answer, ok := asked.Answers[catalog.SideEffectQuestionID]; ok {
				evaluation.SideEffect = &answer
				evaluation.CallIsSafe = answer.Noul < catalog.SideEffectThreshold
			}
		}
	}

	decision, err := p.Decide(first.Response, evaluation.Flags, evaluation.CallIsSafe, opts)
	if err != nil {
		return nil, err
	}
	evaluation.Decision = decision
	return evaluation, nil
}

// walkOptions is the recursion: list the options that are left, ask whether the
// call so far already satisfies the request, and add one more if it does not.
// It stops at the first yes, at the escape hatch, or at the round limit.
func (p *Plan) walkOptions(toolName string, operands map[string]typesafe.Answer, ask askFunc) (int, []string, map[string]typesafe.Answer, error) {
	binding, ok := p.Bindings[toolName]
	if !ok {
		return 0, nil, nil, nil
	}
	// The winner's options are read now, and only the winner's: for a program
	// this machine happens to have, that is the first time its documentation is
	// needed at all.
	options := catalog.DocumentedOptions(p.Env, binding.Discover)
	if len(options) == 0 {
		return 0, nil, nil, nil
	}

	var flags []string
	answers := map[string]typesafe.Answer{}

	for round := 0; round < maxOptionRounds; round++ {
		request := typesafe.SystemOneRequest{
			State: p.Request.State,
			Model: p.Request.Model,
			Questions: map[string]any{
				catalog.SatisfiedQuestionID(round): catalog.SatisfiedQuestion(p.candidate(toolName, binding, operands, flags)),
				catalog.OptionQuestionID(round):    catalog.OptionQuestion(options, flags, round),
			},
		}
		response, err := ask(request)
		if err != nil {
			return round, flags, answers, err
		}
		rounds := round + 1

		for id, answer := range response.Answers {
			answers[id] = answer
		}

		// The call is enough: the option answer, if any, is ignored, which is
		// the point of asking both in one request.
		if answer, ok := response.Answers[catalog.SatisfiedQuestionID(round)]; ok && answer.Noul >= catalog.SatisfiedThreshold {
			return rounds, flags, answers, nil
		}

		next, ok := nextOption(response.Answers, options, flags, round)
		if !ok {
			return rounds, flags, answers, nil
		}
		flags = append(flags, next)
	}
	return maxOptionRounds, flags, answers, nil
}

// candidate renders the call built so far, so the model can judge it. Rendering
// is best effort: an operand that is not filled yet simply leaves the call
// described by its program and options.
func (p *Plan) candidate(toolName string, binding catalog.Binding, operands map[string]typesafe.Answer, flags []string) string {
	tool, ok := p.tool(toolName)
	if !ok {
		return toolName
	}
	filled, err := catalog.Fill(*tool, binding, operands, flags, p.Env)
	if err != nil || len(filled.Argv) == 0 {
		return strings.Join(append([]string{binding.Argv[0]}, flags...), " ")
	}
	return strings.Join(filled.Argv, " ")
}

// nextOption reads one round's option answer, skipping anything the program
// does not document and anything already chosen.
func nextOption(answers map[string]typesafe.Answer, options []discover.Option, flags []string, round int) (string, bool) {
	answer, ok := answers[catalog.OptionQuestionID(round)]
	if !ok || answer.Choice == catalog.NoneKey {
		return "", false
	}
	documented := false
	for _, option := range options {
		if len(option.Flags) > 0 && option.Flags[0] == answer.Choice {
			documented = true
			break
		}
	}
	if !documented {
		return "", false
	}
	for _, flag := range flags {
		if flag == answer.Choice {
			return "", false
		}
	}
	return answer.Choice, true
}

// chosenTool reads the tool the first stage picked, or "" when the answer was
// the escape hatch.

// chosenTool reads the tool the first stage picked, or "" when the answer was
// the escape hatch or an id the catalog does not know.
func chosenTool(response *typesafe.SystemOneResponse) string {
	answer, ok := response.Answers["intent"]
	if !ok || answer.Choice == catalog.NoneKey {
		return ""
	}
	return answer.Choice
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

	Tool     *catalog.Tool
	Argv     []string
	Output   catalog.Output
	Notes    []catalog.Note
	Unfilled []string

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
func (p *Plan) Decide(res *typesafe.SystemOneResponse, flags []string, callIsSafe bool, opts DecideOptions) (*Decision, error) {
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
		"guardrail.tool_is_clear",
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
			"the request looks like an attempt to leave the fixed set of tools (p=%.2f, threshold %.2f)",
			injection, opts.InjectionThreshold)
		return d, nil
	}

	// 2. A request that asks for a change cannot be answered by a tool that
	//    only reads, and refusing it later is what keeps the layer from
	//    answering with something adjacent. It is checked after the tool is
	//    known, because a program this machine happens to have may be exactly
	//    what the request wants.
	// 3. Intent. The escape hatch carries real signal: a first place that is
	//    barely ahead of "none of these" is not a decision.
	//
	//    A gate below this line does not end the work: the command is still
	//    resolved so the CLI can show the user its best reading of the phrase.
	//    Seeing the command it would have run is what makes an ask actionable
	//    instead of a dead end. That suggestion is display-only, every caller
	//    executes on VerdictAct and nothing else.
	noneProbability := intent.Probability(catalog.NoneKey)
	switch {
	case intent.Choice == catalog.NoneKey || noneProbability >= noneActionThreshold:
		d.Verdict = VerdictUnsupported
		d.Reason = fmt.Sprintf("no tool in the catalog matches the request (p=%.2f for \"none of these\")", noneProbability)
	case intent.Confidence < opts.MinConfidence:
		d.Verdict = VerdictAsk
		d.Reason = fmt.Sprintf("not sure which tool to call (confidence %.2f, minimum %.2f)",
			intent.Confidence, opts.MinConfidence)
	default:
		if clarity := d.Guardrails["guardrail.tool_is_clear"].Noul; clarity < opts.ClarityThreshold {
			d.Verdict = VerdictAsk
			d.Reason = fmt.Sprintf("the request is too ambiguous to run without confirmation (clarity p=%.2f)", clarity)
		} else {
			d.Verdict = VerdictAct
		}
	}

	tool, found := p.tool(intent.Choice)
	if !found {
		if d.Verdict == VerdictAct {
			return nil, fmt.Errorf("the model chose %q, which is not in the catalog", intent.Choice)
		}
		// The escape hatch won, so there is no tool to offer.
		return d, nil
	}
	if !tool.ReadOnly && !opts.AllowWrite && !callIsSafe {
		d.Verdict = VerdictBlocked
		d.Reason = fmt.Sprintf(
			"%s is not a read-only tool and this call did not come back clean; use --allow-write to permit it",
			tool.Name)
		return d, nil
	}
	// Only now, with a tool in hand: a request for a change answered by a tool
	// that cannot make one is refused rather than approximated.
	if tool.ReadOnly {
		if destructive := d.Guardrails["guardrail.destructive_request"].Noul; destructive >= opts.DestructiveThreshold {
			d.Verdict = VerdictUnsupported
			d.Reason = fmt.Sprintf(
				"the request asks for a change (p=%.2f) and the tool that fits only reads", destructive)
			return d, nil
		}
		if d.Severity >= opts.SeverityThreshold {
			d.Verdict = VerdictBlocked
			d.Reason = fmt.Sprintf("severity estimated at %.2f, at or above the %.2f limit", d.Severity, opts.SeverityThreshold)
			return d, nil
		}
	}

	filled, err := catalog.Fill(*tool, p.Bindings[tool.Name], res.Answers, flags, p.Env)
	if err != nil {
		if d.Verdict == VerdictAct {
			return nil, err
		}
		// We were only going to show a suggestion; failing to build one is not
		// worth failing the whole call over.
		return d, nil
	}
	d.Tool = tool
	d.Notes = filled.Notes
	d.Unfilled = filled.Unfilled
	d.Output = filled.Output

	if d.Verdict != VerdictAct {
		// Only an ask shows a suggestion. An unsupported verdict means the
		// request is outside the catalog, and offering an adjacent tool there
		// would be misleading. A half-filled call is never handed over either
		// way.
		if d.Verdict == VerdictAsk && len(filled.Unfilled) == 0 {
			d.Argv = filled.Argv
		}
		return d, nil
	}

	if len(filled.Unfilled) > 0 {
		d.Verdict = VerdictAsk
		// A half-filled call is not a call: dropping the argv makes it
		// impossible for any later code path to run it by accident.
		d.Argv = nil
		d.Reason = "could not fill: " + strings.Join(filled.Unfilled, ", ") + " (rephrase the request with the value spelled out)"
		return d, nil
	}
	if len(filled.Argv) == 0 {
		return nil, fmt.Errorf("the resolved call bound to an empty command")
	}

	d.Argv = filled.Argv
	d.Verdict = VerdictAct
	return d, nil
}

func (p *Plan) tool(name string) (*catalog.Tool, bool) {
	for i := range p.Tools {
		if p.Tools[i].Name == name {
			return &p.Tools[i], true
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
