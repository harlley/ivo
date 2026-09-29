// Package resolve discovers installed programs by purpose, reads their
// documentation, and resolves a validated command through typed questions.
package resolve

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/harlley/ivo/internal/discover"

	"github.com/harlley/ivo/internal/catalog"
	"github.com/harlley/ivo/internal/env"
	"github.com/harlley/ivo/internal/model"
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
	// Request begins as discovery, then becomes tool selection and argument resolution.
	Request model.Request
	// Labels maps a question id to option key -> human description, so the CLI
	// can explain a low-confidence answer.
	Labels map[string]map[string]string
	// Narrowed records that discovery asked about a tier of commands rather than
	// the whole list, which is what makes a fallback to the whole list possible
	// when the tiers come up empty.
	Narrowed bool
	// Named are the commands the request itself names, which discovery is asked
	// about first because a word that is already a command here is the strongest
	// signal available.
	Named []string
}

// Build prepares discovery over installed executables, with no predefined tools.
func Build(e *env.Env) (*Plan, error) {
	if len(e.Commands) == 0 {
		return nil, fmt.Errorf("no executable programs found on PATH")
	}
	plan := &Plan{Env: e, Bindings: map[string]catalog.Binding{}}
	plan.Request = model.Request{State: buildState(e)}

	// The commands the sentence names are the strongest signal there is, and the
	// ones this machine's own documentation matches against the request are the
	// next. Both are candidates for the discovery question below, which still
	// asks which program fits: a word that is also a command name is often not
	// naming a tool ("search for TODO"), and a sentence that names one program
	// may mean another.
	plan.Named = catalog.NamedInRequest(e)

	request, ok := plan.discoveryRequest()
	if !ok {
		return nil, fmt.Errorf("no executable programs available for discovery")
	}
	for id, question := range catalog.GuardrailQuestions() {
		request.Questions[id] = question
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	plan.Request = request
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

	// Operands are resolved only after choosing the program: the same word can
	// be a pattern, a path, or a manual-page name depending on that choice.
	for qid, question := range catalog.GuardrailQuestions() {
		questions[qid] = question
	}

	request := model.Request{State: buildState(p.Env), Questions: questions}
	if err := request.Validate(); err != nil {
		return err
	}
	p.Request = request
	p.Labels = labels
	return nil
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

// maxPreDiscovered caps the cheapest tier, which is about the size of the tool
// question: enough candidates to catch a sentence that names one program and
// means another, and few enough that the question stays small.
// MaxOptionRounds bounds the walk over a tool's options. Each round costs one
// request, so the bound is what keeps a call from turning into a conversation.
//
// A round asks two questions at once: whether the call so far is already the
// whole answer, and which option to add if it is not. That is what makes a round
// a check as well as an addition, since the yes it can give is about what the
// round before it added. It is not enough on its own: a walk can also end by
// running out of rounds or by finding nothing worth adding, and then the call it
// ends with is read once more by VerifyQuestion before anything is offered.
const MaxOptionRounds = 4

// Asker is the model adapter used by the resolution pipeline.
type Asker = model.Adapter

// askFunc sends one request and hands back just the response.
type askFunc func(model.Request) (*model.Response, error)

// Evaluation is the outcome of a whole call: every stage, what it cost, and what
// was decided.
type Evaluation struct {
	Decision *Decision
	Usage    model.Usage
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
	FlagAnswers map[string]model.Answer
	// Programs are the installed commands nominated by discovery.
	Programs []string
	// DiscoveryAnswers records the search by purpose over installed programs.
	DiscoveryAnswers map[string]model.Answer
	// Offered records the candidate keys each round put in front of the model,
	// which is the first thing to look at when a call misses the option it
	// needed: the model can only answer with what it was shown.
	Offered [][]string
	// SideEffect is the judgment about a call built from a tool that is not
	// read-only, and CallIsSafe is what it decided.
	SideEffect *model.Answer
	CallIsSafe bool
	// Verify is the judgment about the final call, asked only when no round of
	// the walk confirmed it, and Verified is whether anything did. An
	// unverified call is never handed to the caller as a command to run.
	Verify   *model.Answer
	Verified bool
}

// Evaluate runs the stages a call needs. The tool is chosen first, and only
// then are its own documented options walked, one round at a time: each round
// asks whether the call built so far already answers the request, and if not,
// which option to add next. Both questions ride in the same request, so a round
// costs one round trip whatever the answer turns out to be.
func (p *Plan) Evaluate(ctx context.Context, client Asker, opts DecideOptions) (*Evaluation, error) {
	evaluation := &Evaluation{}

	// One place counts what every request costs.
	ask := func(request model.Request) (*model.Response, error) {
		result, err := client.Ask(ctx, request)
		if err != nil {
			return nil, err
		}
		evaluation.Usage.InputTokens += result.Response.Usage.InputTokens
		evaluation.Usage.OutputTokens += result.Response.Usage.OutputTokens
		evaluation.Latency += result.Latency
		evaluation.Stages++
		if evaluation.Model == "" {
			evaluation.Model = result.Response.Model
		}
		return result.Response, nil
	}

	// The first request is discovery: which installed command fits the request.
	// It is asked about a tier of the machine's commands rather than about all of
	// them, and the whole list is the fallback when the tier names nothing.
	ok, err := p.prepareDiscovery()
	if err != nil {
		return nil, err
	}
	if !ok {
		return p.unsupported(evaluation, opts)
	}

	response, err := ask(p.Request)
	if err != nil {
		return nil, err
	}

	// Check injection before inspecting any candidate. Discovery only nominates
	// installed programs; normal tool selection and argument validation follow.
	if p.blockedByInjection(response) {
		decision, err := p.Decide(response, nil, false, false, opts)
		evaluation.Decision = decision
		return evaluation, err
	}

	evaluation.DiscoveryAnswers = response.Answers
	programs, refused := p.readDiscovery(p.Request, response)
	evaluation.Programs = programs

	// The tiers asked about a list this machine is likely to be asked for. When
	// no batch named anything it has, the whole list is asked once, which is what
	// this stage used to do every time.
	if len(p.Tools) == 0 && refused && p.Narrowed {
		if full, ok := p.fullDiscoveryRequest(); ok {
			fullResponse, err := ask(full)
			if err != nil {
				return nil, err
			}
			evaluation.DiscoveryAnswers = fullResponse.Answers
			evaluation.Programs, _ = p.readDiscovery(full, fullResponse)
		}
	}
	if len(p.Tools) == 0 {
		return p.unsupported(evaluation, opts)
	}

	if err := p.assemble(); err != nil {
		return nil, err
	}
	response, err = ask(p.Request)
	if err != nil {
		return nil, err
	}
	first := &model.Result{Response: response}

	toolName := chosenTool(first.Response)
	if tool, ok := p.tool(toolName); ok {
		// The chosen program's own documentation goes into the state, because
		// every question after this one is about its arguments. The cheap tier
		// built the tool from the one line the name index holds, which is enough
		// to ask whether the program fits and not enough to choose its flags, so
		// the page is read here: once, for the winner.
		purpose, detail := tool.What, tool.Context
		if docs, ok := p.Env.Documentation(tool.Name); ok {
			if docs.Summary != "" {
				purpose = docs.Summary
			}
			if docs.Detail != "" {
				detail = docs.Detail
			}
		}
		state := buildState(p.Env)
		state["selected_program"] = map[string]any{"name": tool.Name, "purpose": purpose, "documentation": detail}
		p.Request.State = state
		questions := map[string]any{}
		for id, question := range catalog.Questions([]catalog.Tool{*tool}, p.Bindings, p.Env) {
			questions[id] = question
			p.Labels[id] = criteriaLabels(question)
		}
		if len(questions) > 0 {
			operands, err := ask(model.Request{State: state, Model: p.Request.Model, Questions: questions})
			if err != nil {
				return nil, err
			}
			for id, answer := range operands.Answers {
				first.Response.Answers[id] = answer
			}
		}
	}
	verified := false
	if toolName != "" {
		walk, err := p.walkOptions(toolName, first.Response.Answers, ask)
		if err != nil {
			return nil, err
		}
		evaluation.Rounds = walk.Rounds
		evaluation.Flags = walk.Flags
		evaluation.FlagAnswers = walk.Answers
		evaluation.Offered = walk.Offered
		verified = walk.Verified

		// The walk can end without a round confirming the call, and then the
		// last flag that was added was never judged. This is that judgment,
		// asked once about the call that is about to be shown: the difference
		// between showing a command and being sure of it.
		if !verified {
			if binding, ok := p.Bindings[toolName]; ok {
				asked, err := ask(model.Request{
					State: p.Request.State,
					Model: p.Request.Model,
					Questions: map[string]any{
						catalog.VerifyQuestionID: catalog.VerifyQuestion(
							p.candidate(toolName, binding, first.Response.Answers, walk.Flags)),
					},
				})
				if err != nil {
					return nil, err
				}
				if answer, ok := asked.Answers[catalog.VerifyQuestionID]; ok {
					evaluation.Verify = &answer
					verified = answer.Noul >= catalog.SatisfiedThreshold
				}
			}

			// The check said the call is missing something and the walk still had
			// options to offer, so the missing one is asked for by name and the
			// call is read once more. This is what a walk that stopped one flag
			// short needs, and it is bounded to one repair: a loop that keeps
			// asking until the answer improves is how a call becomes a
			// conversation.
			if !verified && len(walk.Remaining) > 0 {
				round := walk.Rounds
				evaluation.Offered = append(evaluation.Offered, optionKeys(walk.Remaining))
				asked, err := ask(model.Request{
					State: p.Request.State,
					Model: p.Request.Model,
					Questions: map[string]any{
						catalog.OptionQuestionID(round): catalog.MissingOptionQuestion(walk.Remaining, walk.Flags, round),
					},
				})
				if err != nil {
					return nil, err
				}
				if evaluation.FlagAnswers == nil {
					evaluation.FlagAnswers = map[string]model.Answer{}
				}
				for id, answer := range asked.Answers {
					evaluation.FlagAnswers[id] = answer
				}
				pick, ok := nextOption(asked.Answers, walk.Remaining, walk.Flags, round)
				if answer := asked.Answers[catalog.OptionQuestionID(round)]; ok && answer.Confidence >= catalog.DocumentedFlagThreshold {
					evaluation.Flags = append(evaluation.Flags, pick)
					evaluation.Rounds = round + 1
					if binding, ok := p.Bindings[toolName]; ok {
						again, err := ask(model.Request{
							State: p.Request.State,
							Model: p.Request.Model,
							Questions: map[string]any{
								catalog.VerifyQuestionID: catalog.VerifyQuestion(
									p.candidate(toolName, binding, first.Response.Answers, evaluation.Flags)),
							},
						})
						if err != nil {
							return nil, err
						}
						if answer, ok := again.Answers[catalog.VerifyQuestionID]; ok {
							evaluation.Verify = &answer
							verified = answer.Noul >= catalog.SatisfiedThreshold
						}
					}
				}
			}
		}
	}
	evaluation.Verified = verified

	// A tool nobody classified has to pass one more question, asked about the
	// call that was built rather than about the request. Being told twice
	// already is a reason not to ask.
	if tool, ok := p.tool(chosenTool(first.Response)); ok && !tool.ReadOnly {
		if binding, ok := p.Bindings[tool.Name]; ok {
			asked, err := ask(model.Request{
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

	decision, err := p.Decide(first.Response, evaluation.Flags, evaluation.CallIsSafe, evaluation.Verified, opts)
	if err != nil {
		return nil, err
	}
	evaluation.Decision = decision
	return evaluation, nil
}

// optionWalk is what one walk of the option rounds produced.
type optionWalk struct {
	Rounds  int
	Flags   []string
	Answers map[string]model.Answer
	Offered [][]string
	// Verified reports that a round said the call was already the whole answer.
	// A walk can end without one: it runs out of rounds, or the option question
	// comes back with nothing worth adding. Those two endings used to be
	// indistinguishable from success, and the last flag that was added was
	// never judged.
	Verified bool
	// Remaining are the options still on the table when the walk ended, which is
	// what a call that failed the last check gets asked about once more.
	Remaining []discover.Option
}

// walkOptions is the recursion: list the options that are left, ask whether the
// call so far already answers the whole request, and add one more if it does
// not. It stops at the first yes, at the escape hatch, or at the round limit.
func (p *Plan) walkOptions(toolName string, operands map[string]model.Answer, ask askFunc) (optionWalk, error) {
	var walk optionWalk
	binding, ok := p.Bindings[toolName]
	if !ok {
		return walk, nil
	}
	// The winner's options are read now, and only the winner's: for a program
	// this machine happens to have, that is the first time its documentation is
	// needed at all.
	//
	// A program that lists its own commands is asked about those first, and the
	// list is replaced by the chosen command's options as soon as one is picked:
	// that is the recursion, bounded by the same round limit as everything else.
	options := catalog.DocumentedOptions(p.Env, binding.Discover)
	subcommands := catalog.SubcommandOptionsFor(p.Env, binding.Discover)
	// program follows the walk down: the walk starts at the program the tool
	// names and moves to "git log" when that command is chosen, and everything
	// read afterwards is read from there.
	program := binding.Discover
	if len(options) == 0 && len(subcommands) == 0 {
		// Missing documentation is not evidence that the command is complete.
		// The caller verifies the assembled call even when there is no option.
		return optionWalk{}, nil
	}

	var flags []string
	answers := map[string]model.Answer{}
	var roundKeys [][]string
	widened := false

	for round := 0; round < MaxOptionRounds; round++ {
		offered := options
		if len(subcommands) > 0 {
			offered = subcommands
		}
		if state, ok := p.Request.State.(map[string]any); ok {
			details := map[string]string{}
			for _, option := range catalog.AllDocumentedOptions(p.Env, program) {
				for _, flag := range flags {
					if option.Key() == flag {
						details[flag] = option.Desc
					}
				}
			}
			state["chosen_option_documentation"] = details
		}
		roundKeys = append(roundKeys, optionKeys(offered))
		request := model.Request{
			State: p.Request.State,
			Model: p.Request.Model,
			Questions: map[string]any{
				catalog.SatisfiedQuestionID(round): catalog.SatisfiedQuestion(p.candidate(toolName, binding, operands, flags)),
				catalog.OptionQuestionID(round):    catalog.OptionQuestion(offered, flags, round),
			},
		}
		if question, ok := request.Questions[catalog.OptionQuestionID(round)].(model.ChoiceQuestion); ok && len(question.Criteria) < 2 {
			delete(request.Questions, catalog.OptionQuestionID(round))
		}
		response, err := ask(request)
		if err != nil {
			return optionWalk{Rounds: round, Flags: flags, Answers: answers, Offered: roundKeys}, err
		}
		rounds := round + 1

		for id, answer := range response.Answers {
			answers[id] = answer
		}

		// The call is enough: the option answer, if any, is ignored, which is
		// the point of asking both in one request.
		if satisfied(response, round) {
			return optionWalk{Rounds: rounds, Flags: flags, Answers: answers, Offered: roundKeys, Verified: true,
				Remaining: remaining(options, flags)}, nil
		}

		// A weak choice can indicate that narrowing omitted the needed option.
		// Widen once before committing to it; final verification remains required.
		answer := response.Answers[catalog.OptionQuestionID(round)]
		if !widened && len(subcommands) == 0 && answer.Choice != catalog.NoneKey && answer.Confidence < catalog.DocumentedFlagThreshold {
			if wide := catalog.AllDocumentedOptions(p.Env, program); len(wide) > len(offered) {
				options, widened = wide, true
				continue
			}
		}
		next, ok := nextOption(response.Answers, offered, flags, round)
		if !ok {
			// "The call is not the whole answer" and "nothing you showed me
			// belongs in it" is a contradiction, and it is about the list rather
			// than the call: the option that is missing was never offered. The
			// narrowing is what dropped it, so the whole list is offered once,
			// and the rounds that are left are spent on it.
			if !widened {
				// The program the walk is down to, not the program it started
				// from: after "git log" was chosen, git's own list is empty and
				// the options that matter are the command's.
				if wide := catalog.AllDocumentedOptions(p.Env, program); len(wide) > len(offered) {
					options, subcommands, widened = wide, nil, true
					continue
				}
			}
			return optionWalk{Rounds: rounds, Flags: flags, Answers: answers, Offered: roundKeys,
				Remaining: remaining(options, flags)}, nil
		}
		flags = append(flags, next)

		// Picking a command replaces the list with that command's own options,
		// which is where the recursion stops: one level of commands is what
		// programs document.
		if len(subcommands) > 0 {
			if deeper := catalog.DocumentedOptions(p.Env, program+" "+next); len(deeper) > 0 {
				options, subcommands, program = deeper, nil, program+" "+next
				if docs, ok := p.Env.Documentation(program); ok {
					if state, ok := p.Request.State.(map[string]any); ok {
						state["selected_program"] = map[string]any{"name": program, "purpose": docs.Summary, "documentation": docs.Detail}
					}
				}
			}
		}
	}
	return optionWalk{Rounds: MaxOptionRounds, Flags: flags, Answers: answers, Offered: roundKeys,
		Remaining: remaining(options, flags)}, nil
}

// remaining is the options still on the table: the ones the walk was offering,
// minus the ones already chosen. It is what the call that failed the last check
// gets asked about, so a walk that stopped one flag short can be finished.
func remaining(options []discover.Option, flags []string) []discover.Option {
	out := make([]discover.Option, 0, len(options))
	for _, option := range options {
		if len(option.Argv) == 0 || option.ConflictsWith(flags) {
			continue
		}
		out = append(out, option)
	}
	return out
}

// satisfied reports whether a round said the call built so far is already the
// whole answer. A round that did not answer is not a yes: silence is not
// agreement about a command line.
func satisfied(response *model.Response, round int) bool {
	answer, ok := response.Answers[catalog.SatisfiedQuestionID(round)]
	return ok && answer.Noul >= catalog.SatisfiedThreshold
}

// prepareDiscovery builds the discovery request, guardrails included, and
// leaves it in the plan. It reports false when the machine has nothing to ask
// about at all.
func (p *Plan) prepareDiscovery() (bool, error) {
	request, ok := p.discoveryRequest()
	if !ok {
		return false, nil
	}
	for id, question := range catalog.GuardrailQuestions() {
		request.Questions[id] = question
	}
	if err := request.Validate(); err != nil {
		return false, err
	}
	p.Request = request
	return true, nil
}

// blockedByInjection reports whether the request was an attempt to leave the
// fixed set of tools, and answers the tool question with the escape hatch so the
// decision is refused without a command.
func (p *Plan) blockedByInjection(response *model.Response) bool {
	answer, ok := response.Answers["guardrail.injection"]
	if !ok || answer.Noul < DefaultInjectionThreshold {
		return false
	}
	response.Answers["intent"] = model.Answer{Type: model.KindChoice, Choice: catalog.NoneKey}
	return true
}

// unsupported is the decision for a request no installed program was nominated
// for: nothing runs, and the CLI explains that the vocabulary is closed.
func (p *Plan) unsupported(evaluation *Evaluation, opts DecideOptions) (*Evaluation, error) {
	answers := evaluation.DiscoveryAnswers
	if answers == nil {
		answers = map[string]model.Answer{}
	}
	answers["intent"] = model.Answer{
		Type: model.KindChoice, Choice: catalog.NoneKey, Confidence: 1,
		Probabilities: map[string]float64{catalog.NoneKey: 1},
	}
	decision, err := p.Decide(&model.Response{Answers: answers}, nil, false, false, opts)
	evaluation.Decision = decision
	return evaluation, err
}

// runDiscovery asks which installed command fits the request, in tiers, and
// fills the tools with the nominees. When the tiers come up empty it asks about
// the whole list once, which is what this stage used to do every time.
func (p *Plan) runDiscovery(ask askFunc, evaluation *Evaluation) error {
	request, ok := p.discoveryRequest()
	if !ok {
		return nil
	}
	for id, question := range catalog.GuardrailQuestions() {
		request.Questions[id] = question
	}
	if err := request.Validate(); err != nil {
		return err
	}
	p.Request = request

	response, err := ask(request)
	if err != nil {
		return err
	}
	evaluation.DiscoveryAnswers = response.Answers
	programs, refused := p.readDiscovery(request, response)
	evaluation.Programs = programs

	if len(p.Tools) == 0 && refused && p.Narrowed {
		full, ok := p.fullDiscoveryRequest()
		if !ok {
			return nil
		}
		response, err := ask(full)
		if err != nil {
			return err
		}
		evaluation.DiscoveryAnswers = response.Answers
		evaluation.Programs, _ = p.readDiscovery(full, response)
	}
	return nil
}

// optionKeys is the list of answers a round offered, for diagnosis.
func optionKeys(options []discover.Option) []string {
	out := make([]string, 0, len(options))
	for _, option := range options {
		out = append(out, option.Key())
	}
	return out
}

// candidate renders the call built so far, so the model can judge it. Rendering
// is best effort: an operand that is not filled yet simply leaves the call
// described by its program and options.
func (p *Plan) candidate(toolName string, binding catalog.Binding, operands map[string]model.Answer, flags []string) string {
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
func nextOption(answers map[string]model.Answer, options []discover.Option, flags []string, round int) (string, bool) {
	answer, ok := answers[catalog.OptionQuestionID(round)]
	if !ok || answer.Choice == catalog.NoneKey {
		return "", false
	}
	// The answer names the option as it was offered, which is the key and not
	// the bare flag: a value option is offered as "-n 3", because the number
	// was already read out of the request and is part of what is being chosen.
	documented := false
	for _, option := range options {
		if option.Key() == answer.Choice && !option.ConflictsWith(flags) {
			documented = true
			break
		}
	}
	if !documented {
		return "", false
	}
	return answer.Choice, true
}

// chosenTool reads the tool the first stage picked, or "" when the answer was
// the escape hatch or an id the catalog does not know.
func chosenTool(response *model.Response) string {
	answer, ok := response.Answers["intent"]
	if !ok || answer.Choice == catalog.NoneKey {
		return ""
	}
	return answer.Choice
}

// Verdict is what ivo decided to do about a phrase.
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

	Intent       model.Answer
	Alternatives []model.RankedOption
	Guardrails   map[string]model.Answer
	Severity     float64
	// Caution is what the person should know before approving the call. It is
	// empty when there is nothing to say.
	Caution string
	// Options are the answers the walk settled on, in the order it chose them.
	Options []string
}

// DecideOptions are the gates. Zero values fall back to the documented
// defaults.
type DecideOptions struct {
	MinConfidence        float64
	ClarityThreshold     float64
	DestructiveThreshold float64
	InjectionThreshold   float64
	SeverityThreshold    float64
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
// command", then "did every argument resolve", and last "is this call actually
// the whole answer".
//
// callIsSafe and verified are inputs from the questions asked about the call
// rather than about the request. They are parameters and not fields of
// DecideOptions because a caller has to state them: a default would make
// forgetting one mean trusting the call.
func (p *Plan) Decide(res *model.Response, flags []string, callIsSafe, verified bool, opts DecideOptions) (*Decision, error) {
	opts = opts.withDefaults()

	intent, ok := res.Answers["intent"]
	if !ok {
		return nil, fmt.Errorf("the response did not include the question %q", "intent")
	}

	d := &Decision{
		Intent:     intent,
		Guardrails: map[string]model.Answer{},
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
			"the request looks like an attempt to override execution rules (p=%.2f, threshold %.2f)",
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
	// Consent is the gate, so nothing is refused for being writable. The
	// read-only table and the side effect question are what tell the person what
	// they are about to approve, and only a call that was judged to change
	// something is labelled: "not in the read-only set" is a fact about the
	// program rather than about the call, and the person is already being shown
	// the call.
	switch {
	case !tool.ReadOnly && !callIsSafe:
		d.Caution = fmt.Sprintf("%s is not read-only and this call was judged to change something", tool.Name)
	case d.Severity >= opts.SeverityThreshold:
		d.Caution = fmt.Sprintf("this call scored %.2f on potential harm", d.Severity)
	}

	// With a tool in hand: a request for a change answered by a tool that cannot
	// make one is refused rather than approximated.
	if tool.ReadOnly {
		if destructive := d.Guardrails["guardrail.destructive_request"].Noul; destructive >= opts.DestructiveThreshold {
			d.Verdict = VerdictUnsupported
			d.Reason = fmt.Sprintf(
				"the request asks for a change (p=%.2f) and the tool that fits only reads", destructive)
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
	d.Options = flags
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

	// The last check. A call nobody confirmed answers the whole request is not
	// offered as a command to run: it is shown, with the reason, and the person
	// decides. Consent is the gate, so an unverified call is not refused, it is
	// declared.
	if reason := p.unverified(verified, filled.Argv); reason != "" {
		d.Argv = filled.Argv
		d.Verdict = VerdictAsk
		d.Reason = reason
		return d, nil
	}

	d.Argv = filled.Argv
	d.Verdict = VerdictAct
	return d, nil
}

// unverified explains why a resolved call must not be handed over as an answer,
// or "" when it can be. One of the checks needs no model at all: a request that
// names a number and a command that has none cannot be the answer, whatever any
// question said.
func (p *Plan) unverified(verified bool, argv []string) string {
	numbers := discover.Numbers(p.Env.Request)
	if len(numbers) > 0 && !mentionsAny(argv, numbers) {
		return fmt.Sprintf(
			"the request names %s and the command has no such value, so it cannot be doing what was asked",
			strings.Join(numbers, ", "))
	}
	if !verified {
		return "nothing confirmed that the command answers the whole request, and something it names may be missing"
	}
	return ""
}

// mentionsAny reports whether any token carries one of the values. The value is
// looked for inside the token, because a number usually arrives as part of a
// path or a pattern rather than standing alone.
func mentionsAny(argv []string, values []string) bool {
	for _, token := range argv {
		for _, value := range values {
			if strings.Contains(token, value) {
				return true
			}
		}
	}
	return false
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
func topAlternatives(answer model.Answer, labels map[string]string, n int) []model.RankedOption {
	ranking := answer.Ranking()
	out := make([]model.RankedOption, 0, n)
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

func criteriaLabels(q model.Question) map[string]string {
	out := map[string]string{}
	choice, ok := q.(model.ChoiceQuestion)
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
