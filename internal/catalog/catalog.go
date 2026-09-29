// Package catalog is the tool-calling layer at the bottom of ivo.
//
// A tool is a name, a description and a list of typed parameters. The model
// fills those parameters: it picks one tool, gives each closed parameter a
// value, and answers yes or no to each flag. Code then binds the filled call to
// a command line.
//
// Two properties fall out of that shape:
//
//   - The model never writes a command. Every token on the command line comes
//     from a parameter value this repository authored, or from a path env
//     confirmed exists.
//   - The shell is an implementation detail of the binding. Nothing above this
//     file, and nothing the model sees, has to be phrased in terms of flags.
package catalog

import (
	"fmt"
	"sort"
	"strings"

	"github.com/harlley/ivo/internal/discover"
	"github.com/harlley/ivo/internal/env"
	"github.com/harlley/ivo/internal/model"
)

// maxDiscoveredFlags caps how many of a program's documented options become
// candidates, and maxOfferedOptions caps how many of those a single question
// offers.
//
// The first cap is a guard against a program with hundreds of options. The
// second is about attention: git log documents a hundred and fifty flags, and
// offering all of them got "suppress progress reporting" for a request about the
// last three commits.
const (
	maxDiscoveredFlags = 160
	maxOfferedOptions  = 40
)

// DocumentedFlagThreshold is the probability a discovered option needs before
// it lands. It is higher than FlagThreshold because dozens of options are asked
// at once: a question answered on a hunch must not be enough to add a flag
// nobody asked for, and with many questions in flight a low bar turns small
// errors into a wrong command line.
const DocumentedFlagThreshold = 0.6

// Decision thresholds for turning an answer into a parameter value.
const (
	// FlagThreshold is the probability above which a yes/no flag question is
	// read as a yes. The documentation uses 0.5 for noul decisions.
	FlagThreshold = 0.5
	// StatedThreshold is the probability above which we accept that the request
	// said something about a parameter. Below it the declared default stands,
	// which is the documented way to avoid a question confidently inventing a
	// value the request never mentioned.
	StatedThreshold = 0.5
)

// NoneKey is the escape hatch every closed parameter carries, so the model can
// say "none of these" instead of ranking the least bad value first.
const (
	NoneKey  = "none_of_these"
	NoneDesc = "None of these is what the request means."
)

// Value is one possible value of a closed parameter. Key is what the model
// answers and is deliberately the literal string itself, so nothing has to map
// a label back to an argument afterwards. Argv is how that value binds to the
// command line, possibly nothing for a "no filter" style value.
type Value struct {
	Key  string
	Desc string
	Argv []string
}

// Kind is how a parameter is filled.
type Kind int

const (
	// FlagParam is answered yes or no. Argv holds the tokens a yes contributes.
	FlagParam Kind = iota
	// ChoiceParam is answered with one value out of a closed set.
	ChoiceParam
)

// Param is one argument of a tool call.
type Param struct {
	Kind Kind
	// Name is the argument name, as a tool call would spell it.
	Name string
	// QID overrides the question id, so a parameter shared by several tools is
	// asked exactly once. Empty means "<tool>.<name>".
	QID string
	// Desc says what the argument means. It is the basis of the question the
	// model is asked, so it is written in the tool's terms, not in terms of the
	// command line.
	Desc string

	// Argv is the binding for a flag: the tokens a yes contributes.
	Argv []string
	// Values is the closed set for a choice. ValuesFor builds it at runtime
	// from the environment, which is how filesystem-derived values get in.
	Values    []Value
	ValuesFor func(*env.Env) []Value
	// Default is the value used when the request says nothing about this
	// parameter. The empty string means the parameter has no default and must
	// be answered.
	Default string

	// Gated asks "did the request say anything about this?" before the
	// parameter itself is used. It applies to flags and choices alike: it is
	// what separates "the request wants hidden files" from "the request said
	// nothing". Without it, a question answered on silence can land just above
	// the threshold and invent a value.
	Gated bool
	// Public marks a parameter that came from a program's own documentation,
	// which is held to a stricter threshold because dozens of them are asked at
	// once.
	Public bool
	// Topic is the subject of the gate question. It defaults to Desc.
	Topic string

	// Question, Yes and No override the generated wording. Leave them empty to
	// derive the wording from Desc.
	Question string
	Yes      string
	No       string
}

// QuestionID returns the id the API is asked under. The tool name namespaces
// every parameter except the shared ones, which set QID explicitly.
func (p Param) QuestionID(tool string) string {
	if p.QID != "" {
		return p.QID
	}
	return tool + "." + p.Name
}

// values returns the closed set plus the escape hatch, which is always last.
func (p Param) values(e *env.Env) []Value {
	var values []Value
	if p.ValuesFor != nil {
		values = p.ValuesFor(e)
	} else {
		values = p.Values
	}
	for _, v := range values {
		if v.Key == NoneKey {
			return values
		}
	}
	return append(values, Value{Key: NoneKey, Desc: NoneDesc})
}

// AsQuestion builds the typed question for this parameter.
func (p Param) AsQuestion(e *env.Env) model.Question {
	if p.Kind == FlagParam {
		yes, no := p.Yes, p.No
		if yes == "" {
			yes = "The request asks for " + p.Desc + "."
		}
		if no == "" {
			no = "The request asks for the opposite, or says nothing about it."
		}
		return model.Noul(p.question(), &model.NoulCriteria{True: yes, False: no})
	}
	criteria := map[string]any{}
	for _, v := range p.values(e) {
		if v.Desc == "" {
			criteria[v.Key] = nil
			continue
		}
		criteria[v.Key] = v.Desc
	}
	return model.Choice(p.question(), criteria)
}

// threshold is how sure the model has to be before this parameter lands.
func (p Param) threshold() float64 {
	if p.Public {
		return DocumentedFlagThreshold
	}
	return FlagThreshold
}

func (p Param) question() string {
	if p.Question != "" {
		return p.Question
	}
	if p.Kind == FlagParam {
		return "Does the request ask for " + p.Desc + "?"
	}
	return "Which " + p.Name + " should the tool use? The option names are the values used verbatim."
}

// GateQuestion builds the "did the request say anything about this?" question.
func (p Param) GateQuestion() model.Question {
	topic := p.Topic
	if topic == "" {
		topic = p.Desc
	}
	return model.Noul(
		fmt.Sprintf("Does the request identify %s that must be passed positionally? A reference to the current project or directory identifies . as positional input when the program needs a path to open or act on it; use its documented default otherwise. Action names and output qualifiers are not positional input. A number used as an option value is not a positional operand.", topic),
		&model.NoulCriteria{
			True:  fmt.Sprintf("The request speaks to %s, even indirectly.", topic),
			False: fmt.Sprintf("The request says nothing about %s, so the default should stand.", topic),
		},
	)
}

// Output is how a program's output has to be adapted before it reaches the
// terminal. A few programs format for a terminal rather than for a reader, and
// flattening that is the binding's job, never the model's.
type Output int

const (
	// OutputRaw passes the program's output through untouched.
	OutputRaw Output = iota
	// OutputStripOverstrike drops the backspace overstrikes a terminal
	// formatter writes for bold and underline, which man does.
	OutputStripOverstrike
)

// Binding is a tool's command line template plus the parameters that fill it.
// Placeholders are whole tokens of the form "{param_name}".
type Binding struct {
	Argv   []string
	Params []Param
	Output Output
	// Discover names the program whose documented options this binding offers.
	Discover string
	// Options are the boolean options that program documents, in its own order.
	// They are chosen in a stage of their own, once the tool is known, so the
	// question only ever lists the flags of the tool that won.
	Options []discover.Option
}

// Validate catches catalog mistakes at test time rather than in production:
// unknown placeholders and duplicate parameter names.
func (b Binding) Validate(tool string) error {
	if len(b.Argv) == 0 {
		return fmt.Errorf("%s: empty argv template", tool)
	}
	seen := map[string]bool{}
	for i, param := range b.Params {
		if param.Name == "" {
			return fmt.Errorf("%s: parameter %d has no name", tool, i)
		}
		if seen[param.Name] {
			return fmt.Errorf("%s: duplicate parameter %q", tool, param.Name)
		}
		seen[param.Name] = true
	}
	for _, tok := range b.Argv {
		if tok == FlagsPlaceholder {
			// The options chosen in the flag stage land here, and they are
			// validated against the option list when the call is filled.
			continue
		}
		name, ok := placeholder(tok)
		if !ok {
			continue
		}
		if !seen[name] {
			return fmt.Errorf("%s: argv references unknown parameter {%s}", tool, name)
		}
	}
	return nil
}

func placeholder(tok string) (string, bool) {
	if len(tok) < 3 || tok[0] != '{' || tok[len(tok)-1] != '}' {
		return "", false
	}
	inner := tok[1 : len(tok)-1]
	if strings.ContainsAny(inner, "{} ") {
		return "", false
	}
	return inner, true
}

// Tool is one entry in the closed vocabulary.
type Tool struct {
	// Name is the value the tool-choice question answers with.
	Name string
	// What, NotFor and Examples describe the tool to the model. They are what
	// keeps neighbouring tools from being confused.
	What     string
	Context  string
	NotFor   string
	Examples []string

	// ReadOnly records that the tool cannot modify anything. ivo enforces
	// this in code; it is never the model's decision.
	ReadOnly bool
	// Needs lists binaries the tool cannot run without.
	Needs []string
	// Document names the programs whose documentation this tool's parameters
	// are built from, so the probe can read them before the binding is built.
	Document []string

	// Bind produces the command line binding, or ok=false when the tool is
	// unavailable here (missing binaries, wrong platform).
	Bind func(*env.Env) (Binding, bool)
}

// Documented returns every program any of these tools builds parameters from.
func Documented(tools []Tool) []string {
	seen := map[string]bool{}
	var out []string
	for _, tool := range tools {
		for _, program := range tool.Document {
			if seen[program] {
				continue
			}
			seen[program] = true
			out = append(out, program)
		}
	}
	sort.Strings(out)
	return out
}

// FlagsPlaceholder is where the options chosen in the flag stage land in a
// binding's template.
const FlagsPlaceholder = "{flags}"

// DocumentedOptions reads the boolean options a program documents about itself.
// This is what removes the need to catalogue a tool's capabilities by hand: the
// flag surface comes from the manual.
func DocumentedOptions(e *env.Env, program string) []discover.Option {
	return discover.Relevant(AllDocumentedOptions(e, program), e.Request, maxOfferedOptions)
}

// AllDocumentedOptions is every option a program documents, with the numbers
// the request mentions bound where they fit, and none of the narrowing.
//
// The narrowing is a trade and it is usually the right one: a question that
// offers a hundred and fifty flags gets distracted answers. This is the other
// side of that trade, and it is asked only when the narrow question has already
// failed: the walk put options in front of the model, the model said the call
// was not the whole answer and that nothing it was shown belonged in it, and
// both answers together mean the option the request needs was never offered.
func AllDocumentedOptions(e *env.Env, program string) []discover.Option {
	docs, ok := e.Documentation(program)
	if !ok {
		return nil
	}
	options := discover.Candidates(docs, e.Request, discover.NamedValues{
		Paths:    e.Candidates.Paths,
		Patterns: e.Candidates.Patterns,
		Terms:    e.Candidates.Terms,
	}, model.MaxChoiceOptions-1)

	return options
}

// subcommandOptions are the commands a program lists, as candidates for the
// walk. A program that offers commands is asked about them before it is asked
// about flags: "git log" is neither an option of git nor a program of its own.
func SubcommandOptionsFor(e *env.Env, program string) []discover.Option {
	return subcommandOptions(e, program)
}

func subcommandOptions(e *env.Env, program string) []discover.Option {
	docs, ok := e.Documentation(program)
	if !ok || len(docs.Subcommands) == 0 {
		return nil
	}
	return discover.SubcommandCandidates(docs.Subcommands, model.MaxChoiceOptions-1)
}

// WordQuestion asks whether one word of the request names a program to run.
//
// This is the first filter, and it is the model's rather than a keyword match,
// because a keyword match cannot tell a verb from a binary. "open the current project
// in zed" contains one program name, and the question is asked once per
// word, in parallel, so the whole filter costs one round trip.
func WordQuestion(word string) model.Question {
	return model.Noul(
		map[string]any{
			"word":     word,
			"question": "Does the request use `word` as the name of a program to run?",
			"focus":    "A verb or a noun of the sentence is not a program name, even when it looks like one.",
		},
		&model.NoulCriteria{
			True:  "`word` names a program the request wants executed.",
			False: "`word` is an ordinary word of the sentence.",
		},
	)
}

// WordQuestionID is the id of the question about the nth word.
func WordQuestionID(nth int) string { return fmt.Sprintf("word.%d", nth) }

// WordThreshold is the probability at which a word counts as naming a program.
const WordThreshold = 0.5

// NamedInRequest returns the commands the request names, in the order they
// appear.
//
// This is the cheapest tier of discovery and it needs no model at all: a word of
// the phrase is already the name of something installed here, so there is
// nothing to look up. "use git to list the last 3 commits" and "open the project
// in zed" both arrive with their answer in the sentence, and asking the model
// about two thousand commands to be told what the sentence already said was most
// of what a call used to cost.
//
// A word that happens to be a command name and is not meant as one costs
// nothing here: the tool question still asks the model which program fits, and
// the escape hatch is still on the table.
func NamedInRequest(e *env.Env) []string {
	if len(e.Commands) == 0 {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, field := range strings.Fields(strings.ToLower(e.Request)) {
		word := strings.Trim(field, "\"'`.,;:!?()[]{}<>*")
		if word == "" || seen[word] || !e.HasCommand(word) {
			continue
		}
		seen[word] = true
		out = append(out, word)
		if len(out) == maxNamedInRequest {
			break
		}
	}
	return out
}

// maxNamedInRequest caps the first tier, which is about the size of the tool
// question rather than about correctness: a sentence that names ten commands is
// a sentence this layer is about to refuse anyway.
const maxNamedInRequest = 8

// NamedProgram turns a command this machine can run into a tool. Nothing about
// it is written here: its description comes from its own documentation, and its
// options are read from there when the walk reaches it.
func NamedProgram(program discover.Program) Tool {
	what := program.Summary
	if what == "" {
		what = "The " + program.Name + " command, as installed on this machine."
	}
	return Tool{
		Name:     program.Name,
		What:     what,
		Context:  program.Detail,
		ReadOnly: program.ReadOnly,
		Needs:    []string{program.Name},
		Bind: func(e *env.Env) (Binding, bool) {
			target := targetParam()
			// The operand is optional here, because a program's arguments are
			// its own business: when the request names no path, the program is
			// called without one.
			//
			target.QID = "operand"
			target.Topic = "an explicit input value: a file or directory, text or a pattern to search for, a program whose documentation is requested, or another positional operand"
			target.Gated = true
			target.Default = "no_argument"
			target.ValuesFor = func(e *env.Env) []Value {
				values := []Value{{
					Key:  "no_argument",
					Desc: "No positional operand: the program uses its documented default scope. Do not choose this when a directory or other input must be supplied explicitly.",
				}}
				return append(values, targetValues(e)...)
			}
			return Binding{
				Argv:     []string{program.Name, FlagsPlaceholder, "{target}"},
				Params:   []Param{target},
				Discover: program.Name,
			}, true
		},
	}
}

// OptionQuestion asks which option to add to the call built so far.
//
// It is a Choice over the options the program documents, minus the ones already
// chosen, rather than one yes/no question per option. Two reasons, both learned
// by measuring: asking about every flag of every tool at once drowns the
// decision in noise, and choosing from a list is the shape the model is good
// at, because it only has to compare the request against documented purposes.
func OptionQuestion(options []discover.Option, already []string, round int) model.Question {
	criteria := map[string]any{}
	for _, option := range options {
		if len(option.Argv) == 0 || option.ConflictsWith(already) {
			continue
		}
		key := option.Key()
		desc := option.Desc
		if len(option.Flags) > 1 {
			desc = strings.Join(option.Flags, ", ") + ": " + desc
		}
		criteria[key] = desc
	}
	// What is already chosen is part of the question, so the model can avoid
	// adding an option that a chosen one already covers. Manuals describe
	// overlapping options without saying so: ls documents -a and -A separately
	// and both include the dot files.
	instructions := map[string]any{
		"question": "Which of these options should be added to the call so far?",
	}
	// The escape hatch is not decoration here: it is how the model says that
	// nothing left is needed, which is one of the two ways the walk stops.
	criteria[NoneKey] = "None of these: the call needs no further option."
	if len(already) > 0 {
		chosen := make([]string, 0, len(already))
		for _, flag := range already {
			chosen = append(chosen, describeOption(options, flag))
		}
		instructions["already_chosen"] = chosen
		instructions["note"] = "Do not add an option that one of `already_chosen` already covers."
	}
	if round > 0 {
		instructions["question"] = "The call so far still does not satisfy the request. Which of these further options should be added?"
	}
	return model.Choice(instructions, criteria)
}

// MissingOptionQuestion is OptionQuestion asked after the last check said the
// call is not the whole answer. The options are the same; what changes is that
// the model is told what the check found, which is the difference between
// guessing at a next flag and being asked for the missing one.
func MissingOptionQuestion(options []discover.Option, already []string, round int) model.Question {
	question, ok := OptionQuestion(options, already, round).(model.ChoiceQuestion)
	if !ok {
		return question
	}
	instructions, ok := question.Instructions.(map[string]any)
	if !ok {
		return question
	}
	instructions["question"] = "The last check said this call is not yet the whole answer. Which of these options does the request still need?"
	instructions["focus"] = "Choose the option that supplies the part the request names and this call does not have yet. Choose the escape hatch only when the call already has everything the request asks for."
	return question
}

// describeOption renders one option the way the question refers to it.
func describeOption(options []discover.Option, key string) string {
	for _, option := range options {
		if option.Key() == key {
			return key + " (" + discover.FirstSentence(option.Desc) + ")"
		}
	}
	return key
}

// SatisfiedQuestion asks whether the call built so far already answers the
// request. It is the stopping condition of the walk: without it the model would
// keep adding options, and with it the walk ends as soon as the call is enough.
//
// The candidate is passed as structured data and referred to by name, so the
// question itself stays short and stable across rounds.
// satisfactionCriteria is the judgment both the walk and the last check make.
//
// It is deliberately strict, and it was not always: the earlier wording asked
// whether the call "already does what the request asks for, as it stands", and
// that reads as a question about whether the call is a reasonable start. It
// answered yes for `git log -n 3` on a request that also asked for one line per
// commit, so the walk stopped with the request half answered. Every part of the
// request has to be named as something that would make the answer no.
func satisfactionCriteria() *model.NoulCriteria {
	return &model.NoulCriteria{
		True:  "`candidate` performs the requested action or reports the requested information, including all explicit quantities and qualifiers. Documented default behavior counts; printing the requested information in the program's native output is sufficient.",
		False: "`candidate` leaves out something the request names, such as a count, an order, a direction, a unit, or a second thing it asks for.",
	}
}

// SatisfiedQuestion asks whether the call built so far is already the whole
// answer, which is what stops the walk.
func SatisfiedQuestion(candidate string) model.Question {
	return model.Noul(
		map[string]any{
			"candidate": candidate,
			"question":  "Is `candidate` a complete command-line response to the request?",
			"focus":     "For an information request, printing the relevant measurements or records in the program's native format is sufficient. Do not require a prose answer, a derived summary, or units the user did not specify. Use the selected program's documentation and default behavior. For recursive searches for regular files, a name pattern alone does not exclude directories; require the documented file-type restriction. Reject missing explicit counts, filters, ordering, actions or other qualifiers. Distinguish options that enable an output from options that only modify an already enabled output. A modifier does not enable the output it modifies. Reject missing required option values and extra operands that narrow or change the requested task. Ordinary words describing the action are not file names, revisions, patterns or option values.",
		},
		satisfactionCriteria(),
	)
}

// VerifyQuestion asks the same question about the call the walk ended with.
//
// The walk can end without any round saying the call was enough: it runs out of
// rounds, or the option question comes back with nothing worth adding, and the
// last flag that was added was never judged. This is that judgment, asked once
// about the final call, and it is the difference between showing a command and
// being sure of it.
func VerifyQuestion(candidate string) model.Question {
	return model.Noul(
		map[string]any{
			"candidate": candidate,
			"question":  "Is `candidate` a complete command-line response to the request?",
			"focus":     "For an information request, printing the relevant measurements or records in the program's native format is sufficient. Do not require a prose answer, a derived summary, or units the user did not specify. Use the selected program's documentation and default behavior. For recursive searches for regular files, a name pattern alone does not exclude directories; require the documented file-type restriction. Reject missing explicit counts, filters, ordering, actions or other qualifiers. Distinguish options that enable an output from options that only modify an already enabled output. A modifier does not enable the output it modifies. Reject missing required option values and extra operands that narrow or change the requested task. Ordinary words describing the action are not file names, revisions, patterns or option values.",
		},
		satisfactionCriteria(),
	)
}

// SideEffectQuestion asks whether the call that was built would change anything.
//
// This is the gate a program nobody has classified has to pass. It is asked
// about the resolved call rather than about the request, which is what makes it
// usable for a tool space that is not written in code: a request to open an
// editor in a project reads like a change, while the call zed . does not.
func SideEffectQuestion(candidate string) model.Question {
	return model.Noul(
		map[string]any{
			"candidate": candidate,
			"question":  "Would running `candidate` change anything on this machine, or reach the network?",
			"focus":     "Judge the call itself: reading, listing, searching and opening something are not changes.",
		},
		&model.NoulCriteria{
			True:  "`candidate` deletes, moves, overwrites or creates data, installs or removes software, changes permissions, kills a process, or sends data over the network.",
			False: "`candidate` only reads or displays something, or opens an application, and leaves the machine as it was.",
		},
	)
}

// SideEffectQuestionID is the id of that question, and SideEffectThreshold is
// the probability above which the call counts as changing something.
const (
	SideEffectQuestionID = "side_effect"
	SideEffectThreshold  = 0.5
)

// SatisfiedThreshold is the probability at which the call built so far counts
// as answering the request.
const SatisfiedThreshold = 0.5

// SatisfiedQuestionID and OptionQuestionID are the ids of one round's two
// questions.
func SatisfiedQuestionID(round int) string { return fmt.Sprintf("satisfied.%d", round) }
func OptionQuestionID(round int) string    { return fmt.Sprintf("options.%d", round) }

// VerifyQuestionID is the last check, asked when no round confirmed the call.
const VerifyQuestionID = "verify"

// Available returns the tools that can run here and that have a real decision
// to make in this environment.
func Available(tools []Tool, e *env.Env) []Tool {
	out := make([]Tool, 0, len(tools))
	for _, t := range tools {
		ok := true
		for _, bin := range t.Needs {
			if !e.Has(bin) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		binding, ok := t.Bind(e)
		if !ok || !binding.usable(e) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// Bindings resolves every tool's binding for this environment.
func Bindings(tools []Tool, e *env.Env) map[string]Binding {
	out := make(map[string]Binding, len(tools))
	for _, t := range tools {
		if binding, ok := t.Bind(e); ok {
			out[t.Name] = binding
		}
	}
	return out
}

// usable reports whether every closed parameter has something real to choose
// from. A Choice whose only remaining value is the escape hatch would be a 422
// from the API and, worse, a question with no answer, so a tool that cannot be
// filled here is simply not offered.
func (b Binding) usable(e *env.Env) bool {
	for _, param := range b.Params {
		if param.Kind == FlagParam {
			continue
		}
		if len(param.values(e)) < 2 {
			return false
		}
	}
	return true
}

// ToolQuestion builds the question that picks the tool. Its options are exactly
// the available tool names, plus an escape hatch, so the answer can be used as
// a key without any translation.
func ToolQuestion(tools []Tool) model.Question {
	criteria := map[string]any{}
	for _, t := range tools {
		desc := map[string]any{"what": t.What}
		if t.Context != "" {
			desc["documentation"] = t.Context
		}
		if t.NotFor != "" {
			desc["not_for"] = t.NotFor
		}
		if len(t.Examples) > 0 {
			desc["examples"] = t.Examples
		}
		criteria[t.Name] = desc
	}
	criteria[NoneKey] = "No tool in this list fits the request, or the request wants something no tool here does."
	return model.Choice(
		"Which installed program most directly satisfies all qualifiers in the user's request? Use each program's documented purpose. Prefer a specialized command that prints the requested information directly over a broad report, an interactive monitor, or an interpreter that would need new code. If the request explicitly names a program, prefer it when appropriate. Choose the best fit even when several programs have overlapping capabilities.",
		criteria,
	)
}

// Questions builds every parameter question for every available tool, in one
// map, deduplicated by question id. Sending them all in a single request is the
// documented pattern: questions run in parallel, so a speculative question
// costs tokens but not latency, and the code ignores the ones whose tool did
// not win.
func Questions(tools []Tool, bindings map[string]Binding, e *env.Env) map[string]model.Question {
	out := map[string]model.Question{}
	for _, t := range tools {
		binding, ok := bindings[t.Name]
		if !ok {
			continue
		}
		for _, param := range binding.Params {
			qid := param.QuestionID(t.Name)
			if _, seen := out[qid]; seen {
				continue
			}
			out[qid] = param.AsQuestion(e)
			if param.Gated {
				out[qid+"?"] = param.GateQuestion()
			}
		}
	}
	return out
}

// Note records one parameter decision, so the CLI can explain itself.
type Note struct {
	Question string
	Param    string
	Detail   string
	Answer   model.Answer
	Answered bool
}

// Call is the typed result of the layer: which tool, with which arguments.
// The command line is one binding of it, not the thing itself.
type Call struct {
	Tool string
	// Args maps a parameter name to its value: a bool for a flag, the chosen
	// key for a closed parameter.
	Args map[string]any
}

// Result is the outcome of filling one tool call.
type Result struct {
	Call     Call
	Argv     []string
	Notes    []Note
	Unfilled []string
	// Output is how the bound program's output has to be adapted.
	Output Output
}

type filled struct {
	param    Param
	qid      string
	tokens   []string
	value    any
	answer   model.Answer
	answered bool
	weight   float64
	// resolved records that the parameter got a real value. It is deliberately
	// not the same as "produced tokens": a value may legitimately mean "no
	// filter" and contribute nothing, while the escape hatch is not a value at
	// all.
	resolved bool
	// unstated records that the request said nothing about this parameter, so
	// the note can say so instead of pretending the model decided.
	unstated bool
}

// Fill reads only the answers belonging to the chosen tool and binds them to a
// command line. Every token comes from a value this repository authored, or
// from a path env confirmed exists.
func Fill(tool Tool, binding Binding, answers map[string]model.Answer, flags []string, e *env.Env) (Result, error) {
	if err := binding.Validate(tool.Name); err != nil {
		return Result{}, err
	}

	call := Call{Tool: tool.Name, Args: map[string]any{}}
	filledParams := make([]filled, 0, len(binding.Params))

	for _, param := range binding.Params {
		qid := param.QuestionID(tool.Name)
		f := filled{param: param, qid: qid}

		answer, ok := answers[qid]
		if !ok && !param.Gated && param.Kind != FlagParam {
			// A required parameter with no answer is a hole we cannot paper
			// over.
			return Result{}, fmt.Errorf("catalog: no answer for %q", qid)
		}
		f.answer = answer
		f.answered = ok

		// The gate comes first, for flags and choices alike.
		if param.Gated {
			gate, ok := answers[qid+"?"]
			if !ok {
				return Result{}, fmt.Errorf("catalog: no answer for %q", qid+"?")
			}
			if gate.Noul < StatedThreshold {
				f.weight = gate.Noul
				f.unstated = true
				f.resolved = true
				if param.Kind == FlagParam {
					f.value = false
				} else {
					if v, found := findValue(param.values(e), param.Default); found {
						f.tokens = v.Argv
						f.value = v.Key
					}
				}
				filledParams = append(filledParams, f)
				continue
			}
		}

		switch param.Kind {
		case FlagParam:
			if !ok {
				return Result{}, fmt.Errorf("catalog: no answer for flag %q", qid)
			}
			f.resolved = true
			f.weight = answer.Noul
			if answer.Noul >= param.threshold() {
				f.tokens = param.Argv
				f.value = true
			} else {
				f.value = false
			}

		case ChoiceParam:
			if !ok {
				return Result{}, fmt.Errorf("catalog: no answer for %q", qid)
			}
			f.weight = answer.Confidence
			v, found := findValue(param.values(e), answer.Choice)
			if !found {
				return Result{}, unknownValue(qid, answer.Choice)
			}
			if v.Key != NoneKey {
				f.resolved = true
				f.tokens = v.Argv
				f.value = v.Key
			}
		}

		filledParams = append(filledParams, f)
	}

	var unfilled []string
	notes := make([]Note, 0, len(filledParams))
	tokensFor := make(map[string][]string, len(filledParams))
	for _, f := range filledParams {
		tokensFor[f.param.Name] = f.tokens
		notes = append(notes, Note{
			Question: f.qid,
			Param:    f.param.Name,
			Answer:   f.answer,
			Answered: f.answered,
			Detail:   describe(f),
		})
		if !f.resolved {
			unfilled = append(unfilled, f.param.Name)
			continue
		}
		call.Args[f.param.Name] = f.value
	}

	// The options chosen in the flag stage are validated against what the
	// program documents, which is the closed set for this position. The tokens a
	// chosen answer stands for are looked up in that set, so an answer can only
	// ever mean what the program documented.
	//
	// The set here is the whole documented list and not the narrowed one the
	// question offers. Narrowing is about attention, which is the question's
	// problem, and it has no business rejecting an option: it once did, and an
	// option the walk had just offered came back as "not an option git
	// documents".
	answerTokens := map[string][]string{}
	answerOptions := map[string]discover.Option{}
	addCandidates := func(program string) {
		for _, option := range AllDocumentedOptions(e, program) {
			answerTokens[option.Key()] = option.Argv
			answerOptions[option.Key()] = option
		}
		for _, sub := range subcommandOptions(e, program) {
			answerTokens[sub.Key()] = sub.Argv
		}
	}
	addCandidates(binding.Discover)
	// A chosen command brings its own options with it, which is the whole point
	// of the level: "git log" documents flags that "git" does not.
	for _, key := range flags {
		if _, isSubcommand := answerTokens[key]; isSubcommand && !strings.HasPrefix(key, "-") {
			addCandidates(binding.Discover + " " + key)
		}
	}

	var chosenFlags, predicates []string
	for i, key := range flags {
		if option, ok := answerOptions[key]; ok && option.ConflictsWith(flags[:i]) {
			return Result{}, fmt.Errorf("catalog: conflicting repeated option %q", key)
		}
		tokens, ok := answerTokens[key]
		if !ok {
			return Result{}, fmt.Errorf("catalog: %q is not an option %s documents", key, binding.Discover)
		}
		if option, ok := answerOptions[key]; ok && option.AfterOperand {
			predicates = append(predicates, tokens...)
		} else {
			chosenFlags = append(chosenFlags, tokens...)
		}
	}

	// Bind: a placeholder contributes zero or more whole tokens, a literal
	// contributes itself.
	argv := make([]string, 0, len(binding.Argv)+4)
	for _, tok := range binding.Argv {
		if tok == FlagsPlaceholder {
			argv = append(argv, chosenFlags...)
			continue
		}
		name, isPlaceholder := placeholder(tok)
		if !isPlaceholder {
			if tok != "" {
				argv = append(argv, tok)
			}
			continue
		}
		for _, t := range tokensFor[name] {
			if t == "" {
				continue
			}
			if strings.ContainsRune(t, 0) {
				return Result{}, fmt.Errorf("catalog: refusing token with NUL byte from {%s}", name)
			}
			argv = append(argv, t)
		}
	}

	argv = append(argv, predicates...)
	return Result{Call: call, Argv: argv, Notes: notes, Unfilled: unfilled, Output: binding.Output}, nil
}

func describe(f filled) string {
	switch {
	case f.unstated && f.param.Kind == FlagParam:
		return fmt.Sprintf("omitted: not mentioned (p=%.2f)", f.weight)
	case f.unstated:
		if len(f.tokens) == 0 {
			return fmt.Sprintf("omitted: not mentioned (p=%.2f)", f.weight)
		}
		return fmt.Sprintf("default %s: not mentioned (p=%.2f)", strings.Join(f.tokens, " "), f.weight)
	case f.param.Kind == FlagParam:
		if len(f.tokens) > 0 {
			return fmt.Sprintf("included (p=%.2f)", f.answer.Noul)
		}
		return fmt.Sprintf("omitted (p=%.2f)", f.answer.Noul)
	case len(f.tokens) == 0:
		if f.answer.Choice == NoneKey {
			return "unfilled: none of the values fits"
		}
		return "no value"
	default:
		return fmt.Sprintf("%s (confidence %.2f)", strings.Join(f.tokens, " "), f.answer.Confidence)
	}
}

// Programs returns the program names a binding can place in argv[0].
//
// Only argv[0] matters for execution permission, so this is exact rather than
// generous. When the template's first token is a placeholder, the programs are
// the first tokens of that parameter's values, and nothing else in the binding
// is a candidate for the program position.
func Programs(binding Binding, e *env.Env) []string {
	var out []string
	if len(binding.Argv) == 0 {
		return out
	}
	name, isPlaceholder := placeholder(binding.Argv[0])
	if !isPlaceholder {
		if isProgramToken(binding.Argv[0]) {
			return []string{binding.Argv[0]}
		}
		return out
	}
	for _, param := range binding.Params {
		if param.Name != name {
			continue
		}
		for _, v := range param.values(e) {
			if len(v.Argv) > 0 && isProgramToken(v.Argv[0]) {
				out = append(out, v.Argv[0])
			}
		}
	}
	return out
}

func isProgramToken(tok string) bool {
	return tok != "" && !strings.HasPrefix(tok, "-") && !strings.ContainsRune(tok, 0)
}

// Allowlist returns every program name that any available binding can place in
// argv[0]. It is the last line of defence: even a bug in this catalog cannot
// make ivo exec a program that is not on this list.
func Allowlist(bindings map[string]Binding, e *env.Env) []string {
	set := map[string]bool{}
	for _, binding := range bindings {
		for _, program := range Programs(binding, e) {
			set[program] = true
		}
	}
	out := make([]string, 0, len(set))
	for program := range set {
		out = append(out, program)
	}
	sort.Strings(out)
	return out
}

// unknownValue is the loud failure for an answer the API should never have
// produced. Papering over it with a default would hide a contract violation,
// which is exactly the kind of silent wrongness this design exists to avoid.
func unknownValue(qid, key string) error {
	return fmt.Errorf("catalog: %q answered with %q, which is not one of its values", qid, key)
}

// findValue looks up a value in a list. The env may be nil: the keys are all we
// need here, and the descriptions are not used.
func findValue(values []Value, key string) (Value, bool) {
	for _, v := range values {
		if v.Key == key {
			return v, true
		}
	}
	return Value{}, false
}
