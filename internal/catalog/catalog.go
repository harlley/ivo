// Package catalog is the tool-calling layer at the bottom of jev-cli.
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

	"github.com/harlleyoliveira/jev-cli/internal/env"
	"github.com/harlleyoliveira/jev-cli/internal/typesafe"
)

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
func (p Param) AsQuestion(e *env.Env) typesafe.Question {
	if p.Kind == FlagParam {
		yes, no := p.Yes, p.No
		if yes == "" {
			yes = "The request asks for " + p.Desc + "."
		}
		if no == "" {
			no = "The request asks for the opposite, or says nothing about it."
		}
		return typesafe.Noul(p.question(), &typesafe.NoulCriteria{True: yes, False: no})
	}
	criteria := map[string]any{}
	for _, v := range p.values(e) {
		if v.Desc == "" {
			criteria[v.Key] = nil
			continue
		}
		criteria[v.Key] = v.Desc
	}
	return typesafe.Choice(p.question(), criteria)
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
func (p Param) GateQuestion() typesafe.Question {
	topic := p.Topic
	if topic == "" {
		topic = p.Desc
	}
	return typesafe.Noul(
		fmt.Sprintf("Does the request say anything about %s?", topic),
		&typesafe.NoulCriteria{
			True:  fmt.Sprintf("The request speaks to %s, even indirectly.", topic),
			False: fmt.Sprintf("The request says nothing about %s, so the default should stand.", topic),
		},
	)
}

// Binding is a tool's command line template plus the parameters that fill it.
// Placeholders are whole tokens of the form "{param_name}".
type Binding struct {
	Argv   []string
	Params []Param
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
	NotFor   string
	Examples []string

	// ReadOnly records that the tool cannot modify anything. jev-cli enforces
	// this in code; it is never the model's decision.
	ReadOnly bool
	// Needs lists binaries the tool cannot run without.
	Needs []string

	// Bind produces the command line binding, or ok=false when the tool is
	// unavailable here (missing binaries, wrong platform).
	Bind func(*env.Env) (Binding, bool)
}

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
func ToolQuestion(tools []Tool) typesafe.Question {
	criteria := map[string]any{}
	for _, t := range tools {
		desc := map[string]any{"what": t.What}
		if t.NotFor != "" {
			desc["not_for"] = t.NotFor
		}
		if len(t.Examples) > 0 {
			desc["examples"] = t.Examples
		}
		criteria[t.Name] = desc
	}
	criteria[NoneKey] = "The request wants something none of these tools does, including anything that modifies, moves or deletes data, installs software, or reaches the network."
	return typesafe.Choice(
		"Which of the available tools should be called to satisfy the request?",
		criteria,
	)
}

// Questions builds every parameter question for every available tool, in one
// map, deduplicated by question id. Sending them all in a single request is the
// documented pattern: questions run in parallel, so a speculative question
// costs tokens but not latency, and the code ignores the ones whose tool did
// not win.
func Questions(tools []Tool, bindings map[string]Binding, e *env.Env) map[string]typesafe.Question {
	out := map[string]typesafe.Question{}
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
	Answer   typesafe.Answer
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
}

type filled struct {
	param    Param
	qid      string
	tokens   []string
	value    any
	answer   typesafe.Answer
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
func Fill(tool Tool, binding Binding, answers map[string]typesafe.Answer, e *env.Env) (Result, error) {
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
			if answer.Noul >= FlagThreshold {
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

	// Bind: a placeholder contributes zero or more whole tokens, a literal
	// contributes itself.
	argv := make([]string, 0, len(binding.Argv)+4)
	for _, tok := range binding.Argv {
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

	return Result{Call: call, Argv: argv, Notes: notes, Unfilled: unfilled}, nil
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
// make jev-cli exec a program that is not on this list.
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
