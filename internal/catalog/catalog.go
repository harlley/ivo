// Package catalog is the heart of jev-cli: the closed vocabulary of commands
// the model may choose from.
//
// The model never writes a command. It answers questions about *which* catalog
// entry and *which* of a fixed set of options, and this package turns those
// answers into an argv slice. Because every token that reaches the command
// line is authored here or confirmed by env, and because we never invoke a
// shell, there is no path from the model's output to an arbitrary command.
package catalog

import (
	"fmt"
	"sort"
	"strings"

	"github.com/harlleyoliveira/jev-cli/internal/env"
	"github.com/harlleyoliveira/jev-cli/internal/typesafe"
)

// Decision thresholds for turning an answer into a token.
const (
	// FlagThreshold is the probability above which a yes/no flag question is
	// read as a yes. The documentation uses 0.5 for noul decisions.
	FlagThreshold = 0.5
	// StatedThreshold is the probability above which we accept that the user
	// said something about an optional argument. Below it we omit the argument
	// and let the program's own default stand, which is the documented way to
	// avoid a Choice confidently inventing a value the user never mentioned.
	StatedThreshold = 0.5
)

// NoneKey is the escape hatch every selection question carries, so the model
// can say "none of these" instead of ranking the least-bad option first.
const (
	NoneKey  = "none_of_these"
	NoneDesc = "None of these is what the request means."
)

// Value is one option of a selection question. Key is what the model answers
// and is deliberately the literal string itself, so nothing has to map a label
// back to an argument afterwards. Argv is what that option contributes to the
// command line — possibly nothing, for "no filter" style options.
type Value struct {
	Key  string
	Desc string
	Argv []string
}

// Slot is one decision the model makes about a command: a yes/no flag, or a
// selection from a closed set of values.
type Slot struct {
	// ID names the placeholder in the command template.
	ID string
	// QID is the question id sent to the API. Empty means "<command>.<ID>".
	// Shared slots use a bare, command-independent QID so the question is
	// asked exactly once even when several commands use it.
	QID string
	// Question is the instructions for a selection question, or for a flag
	// question when TrueArgvByCmd is set.
	Question string
	// Yes/No describe what a true and a false mean, for flag questions.
	Yes string
	No  string

	// TrueArgvByCmd holds the tokens a true flag contributes, keyed by command
	// id; the "*" key applies to every command. A slot is a flag slot when
	// this map is non-nil.
	TrueArgvByCmd map[string][]string

	// Options is the closed set for a selection slot. OptionsFor builds it at
	// runtime from the environment, which is how filesystem-derived choices
	// get in. A slot is a selection slot when either is set.
	Options    []Value
	OptionsFor func(*env.Env) []Value

	// Stated gates a slot behind a "did the user say anything about this?"
	// question. It applies to flags as well as to selections: it is what
	// separates "the user wants hidden files" from "the user said nothing".
	// It adds a "<qid>?" noul question, and Topic describes what that question
	// is about.
	Stated bool
	Topic  string
	// Default is the option key used when an optional slot is not stated, or
	// when the answer is the escape hatch.
	Default string

	// EmptyFallback is used when the chosen option contributes no tokens but
	// the command still needs an argument (e.g. find needs a path).
	EmptyFallback []string

	// Requires names another slot's placeholder. If that slot contributed no
	// tokens, this one is dropped too — this is how `-name` and its pattern
	// stay together instead of leaving a dangling flag.
	Requires string

	// Group makes flags mutually exclusive. Within a group only the highest
	// probability winner survives, so "sort by time" and "sort by size" can
	// never both land on the command line.
	Group string
}

// IsFlag reports whether the slot is answered by a yes/no question.
func (s Slot) IsFlag() bool { return s.TrueArgvByCmd != nil }

// QuestionID returns the fully qualified question id for a command.
func (s Slot) QuestionID(cmdID string) string {
	if s.QID != "" {
		return s.QID
	}
	return cmdID + "." + s.ID
}

// Placeholder is the template token this slot fills.
func (s Slot) Placeholder() string { return s.ID }

func (s Slot) trueArgv(cmdID string) []string {
	if v, ok := s.TrueArgvByCmd[cmdID]; ok {
		return v
	}
	return s.TrueArgvByCmd["*"]
}

func (s Slot) options(e *env.Env) []Value {
	var opts []Value
	if s.OptionsFor != nil {
		opts = s.OptionsFor(e)
	} else {
		opts = s.Options
	}
	// Every selection gets an escape hatch, and it is always last so the
	// option list reads naturally.
	for _, o := range opts {
		if o.Key == NoneKey {
			return opts
		}
	}
	return append(opts, Value{Key: NoneKey, Desc: NoneDesc})
}

// AsQuestion builds the typed question for this slot.
func (s Slot) AsQuestion(e *env.Env) typesafe.Question {
	if s.IsFlag() {
		return typesafe.Noul(s.Question, &typesafe.NoulCriteria{True: s.Yes, False: s.No})
	}
	criteria := map[string]any{}
	for _, o := range s.options(e) {
		if o.Desc == "" {
			criteria[o.Key] = nil
			continue
		}
		criteria[o.Key] = o.Desc
	}
	return typesafe.Choice(s.Question, criteria)
}

// StatedQuestion builds the "did the user say anything about this?" question
// that keeps optional arguments honest.
func (s Slot) StatedQuestion() typesafe.Question {
	topic := s.Topic
	if topic == "" {
		topic = s.ID
	}
	return typesafe.Noul(
		fmt.Sprintf("Does the request say anything about %s?", topic),
		&typesafe.NoulCriteria{
			True:  fmt.Sprintf("The request speaks to %s, even indirectly.", topic),
			False: fmt.Sprintf("The request says nothing about %s, so the default should stand.", topic),
		},
	)
}

// Spec is a command resolved for one environment: the argv template plus the
// slots that fill it. Placeholders are whole tokens of the form "{slot_id}".
type Spec struct {
	Argv  []string
	Slots []Slot
}

// Validate catches catalog mistakes at test time rather than at 2am: unknown
// placeholders, duplicate ids, a Requires that points forwards or nowhere.
func (s Spec) Validate(cmdID string) error {
	if len(s.Argv) == 0 {
		return fmt.Errorf("%s: empty argv template", cmdID)
	}
	seen := map[string]int{}
	for i, slot := range s.Slots {
		if slot.ID == "" {
			return fmt.Errorf("%s: slot %d has no id", cmdID, i)
		}
		if _, dup := seen[slot.ID]; dup {
			return fmt.Errorf("%s: duplicate slot id %q", cmdID, slot.ID)
		}
		seen[slot.ID] = i
	}
	// Requires may point forwards — a flag often depends on a pattern slot
	// declared after it — so the only thing to check is that the target exists
	// somewhere in the spec. Assemble resolves it in two passes.
	for _, slot := range s.Slots {
		if slot.Requires == "" {
			continue
		}
		if _, found := seen[slot.Requires]; !found {
			return fmt.Errorf("%s: slot %q requires %q, which does not exist", cmdID, slot.ID, slot.Requires)
		}
		if slot.IsFlag() && slot.TrueArgvByCmd[cmdID] == nil && slot.TrueArgvByCmd["*"] == nil {
			return fmt.Errorf("%s: flag slot %q has no tokens for this command", cmdID, slot.ID)
		}
	}
	for _, tok := range s.Argv {
		name, ok := placeholder(tok)
		if !ok {
			continue
		}
		if _, found := seen[name]; !found {
			return fmt.Errorf("%s: argv references unknown placeholder {%s}", cmdID, name)
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

// Command is one entry in the closed vocabulary.
type Command struct {
	// ID is the option key the intent question answers with.
	ID string
	// What/NotFor/Examples become the contrastive description of this option,
	// which is what keeps neighbouring commands from being confused.
	What     string
	NotFor   string
	Examples []string

	// ReadOnly records that the command cannot modify anything. jev-cli
	// enforces this in code; it is never the model's decision.
	ReadOnly bool
	// Needs lists binaries this command cannot run without.
	Needs []string

	// Build produces the spec for this environment, or ok=false when the
	// command is unavailable (missing binaries, wrong platform).
	Build func(*env.Env) (Spec, bool)
}

// Available returns the commands that can run in this environment and that
// have a real decision to make here.
func Available(cmds []Command, e *env.Env) []Command {
	out := make([]Command, 0, len(cmds))
	for _, c := range cmds {
		ok := true
		for _, bin := range c.Needs {
			if !e.Has(bin) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		spec, ok := c.Build(e)
		if !ok || !spec.usable(e) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// Specs resolves every command's spec for this environment.
func Specs(cmds []Command, e *env.Env) map[string]Spec {
	out := make(map[string]Spec, len(cmds))
	for _, c := range cmds {
		if spec, ok := c.Build(e); ok {
			out[c.ID] = spec
		}
	}
	return out
}

// usable reports whether every required selection slot has something real to
// choose from. A Choice whose only remaining option is the escape hatch would
// be a 422 from the API and, worse, a question with no answer — so a command
// that cannot be decided here is simply not offered.
func (s Spec) usable(e *env.Env) bool {
	for _, slot := range s.Slots {
		if slot.IsFlag() {
			continue
		}
		if len(slot.options(e)) < 2 {
			return false
		}
	}
	return true
}

// IntentQuestion builds the question that picks the command. Its options are
// exactly the available command ids, plus an escape hatch, so the answer can
// be used as a key without any translation.
func IntentQuestion(cmds []Command) typesafe.Question {
	criteria := map[string]any{}
	for _, c := range cmds {
		desc := map[string]any{"what": c.What}
		if c.NotFor != "" {
			desc["not_for"] = c.NotFor
		}
		if len(c.Examples) > 0 {
			desc["examples"] = c.Examples
		}
		criteria[c.ID] = desc
	}
	criteria[NoneKey] = "The request wants something none of these commands does — including anything that modifies, moves or deletes data, installs software, or reaches the network."
	return typesafe.Choice(
		"Which of the available commands should run on this machine to satisfy the request?",
		criteria,
	)
}

// Questions builds every slot question for every available command, in one
// map, deduplicated by question id. Sending them all in a single request is
// the documented pattern: questions run in parallel, so a speculative question
// costs tokens but not latency, and the code ignores the ones whose command
// did not win.
func Questions(cmds []Command, specs map[string]Spec, e *env.Env) map[string]typesafe.Question {
	out := map[string]typesafe.Question{}
	for _, c := range cmds {
		spec, ok := specs[c.ID]
		if !ok {
			continue
		}
		for _, slot := range spec.Slots {
			qid := slot.QuestionID(c.ID)
			if _, seen := out[qid]; seen {
				continue
			}
			out[qid] = slot.AsQuestion(e)
			if slot.Stated {
				out[qid+"?"] = slot.StatedQuestion()
			}
		}
	}
	return out
}

// Note records one decision the assembler made, so the CLI can explain itself.
type Note struct {
	Question string
	Slot     string
	Detail   string
	Answer   typesafe.Answer
	Answered bool
}

// Assembled is the outcome of turning answers into a command line.
type Assembled struct {
	Argv  []string
	Notes []Note
	// Missing lists required slots that did not resolve. When it is non-empty
	// there is no usable command.
	Missing []string
}

type evaluated struct {
	slot       Slot
	qid        string
	tokens     []string
	answer     typesafe.Answer
	answered   bool
	weight     float64
	suppressed string
	// unstated records that the user said nothing about this slot, so the
	// note can say so instead of pretending the model decided.
	unstated bool
}

// Assemble reads only the answers belonging to the winning command and expands
// the template. Every token comes from a Value that this repository authored,
// or from a path that env confirmed exists.
func Assemble(cmd Command, spec Spec, answers map[string]typesafe.Answer, e *env.Env, forced map[string]string) (Assembled, error) {
	if err := spec.Validate(cmd.ID); err != nil {
		return Assembled{}, err
	}

	evaluatedSlots := make([]evaluated, 0, len(spec.Slots))
	produced := map[string]bool{}

	for _, slot := range spec.Slots {
		qid := slot.QuestionID(cmd.ID)
		ev := evaluated{slot: slot, qid: qid}

		// A literal supplied by the human on the command line replaces the
		// model's decision for this slot entirely. The model was never asked,
		// so there is no answer to find, and the option could not have been in
		// the closed set anyway.
		if literal, isForced := forced[slot.ID]; isForced && !slot.IsFlag() {
			ev.tokens = []string{literal}
			ev.answer = typesafe.Answer{Type: typesafe.KindChoice, Choice: literal, Confidence: 1}
			ev.answered = true
			ev.weight = 1
			evaluatedSlots = append(evaluatedSlots, ev)
			continue
		}

		answer, ok := answers[qid]
		if !ok {
			// A required slot with no answer is a hole we cannot paper over.
			if !slot.Stated {
				return Assembled{}, fmt.Errorf("catalog: no answer for %q", qid)
			}
		}
		ev.answer = answer
		ev.answered = ok

		// The "did the user say anything about this?" gate comes first, for
		// flags and selections alike. Without it, a flag question answered on
		// silence lands just above the threshold and adds a flag nobody asked
		// for (the real model answers 0.52 for "liste todos os arquivos desse
		// diretório" — enough to add -a). With it, silence means the declared
		// default stands and the flag is left off.
		if slot.Stated {
			statedQID := qid + "?"
			stated, ok := answers[statedQID]
			if !ok {
				return Assembled{}, fmt.Errorf("catalog: no answer for %q", statedQID)
			}
			if stated.Noul < StatedThreshold {
				ev.weight = stated.Noul
				ev.unstated = true
				if !slot.IsFlag() {
					if val, found := findValue(slot.options(e), slot.Default); found {
						ev.tokens = val.Argv
					}
				}
				evaluatedSlots = append(evaluatedSlots, ev)
				continue
			}
		}

		switch {
		case slot.IsFlag():
			if !ok {
				return Assembled{}, fmt.Errorf("catalog: no answer for flag %q", qid)
			}
			ev.weight = answer.Noul
			if answer.Noul >= FlagThreshold {
				ev.tokens = slot.trueArgv(cmd.ID)
			}

		case slot.Stated:
			ev.weight = answer.Confidence
			val, found := findValue(slot.options(e), answer.Choice)
			if !found {
				return Assembled{}, unknownOption(qid, answer.Choice)
			}
			if val.Key == NoneKey {
				ev.tokens = nil
				break
			}
			ev.tokens = val.Argv

		default:
			if !ok {
				return Assembled{}, fmt.Errorf("catalog: no answer for %q", qid)
			}
			ev.weight = answer.Confidence
			val, found := findValue(slot.options(e), answer.Choice)
			if !found {
				return Assembled{}, unknownOption(qid, answer.Choice)
			}
			if val.Key == NoneKey {
				ev.tokens = nil
				break
			}
			ev.tokens = val.Argv
		}

		evaluatedSlots = append(evaluatedSlots, ev)
	}

	// Resolve flag groups: within a group, only the strongest winner keeps its
	// tokens. This is what stops `ls -t -S` from ever being built.
	for i := range evaluatedSlots {
		g := evaluatedSlots[i].slot.Group
		if g == "" || len(evaluatedSlots[i].tokens) == 0 {
			continue
		}
		for j := range evaluatedSlots {
			if i == j || evaluatedSlots[j].slot.Group != g || len(evaluatedSlots[j].tokens) == 0 {
				continue
			}
			winner, loser := i, j
			if evaluatedSlots[j].weight > evaluatedSlots[i].weight {
				winner, loser = j, i
			}
			evaluatedSlots[loser].tokens = nil
			evaluatedSlots[loser].suppressed = evaluatedSlots[winner].slot.ID
			if loser == i {
				break
			}
		}
	}

	// Two passes, because Requires points forwards as often as backwards: a
	// flag may depend on a pattern slot declared after it. Pass one decides
	// what each placeholder contributes on its own; pass two drops the slots
	// whose dependency contributed nothing.
	for i := range evaluatedSlots {
		ev := &evaluatedSlots[i]
		if len(ev.tokens) == 0 && len(ev.slot.EmptyFallback) > 0 && ev.slot.Requires == "" {
			ev.tokens = ev.slot.EmptyFallback
		}
		produced[ev.slot.Placeholder()] = len(ev.tokens) > 0
	}
	for i := range evaluatedSlots {
		ev := &evaluatedSlots[i]
		if ev.slot.Requires == "" {
			continue
		}
		if !produced[ev.slot.Requires] {
			ev.tokens = nil
		}
		produced[ev.slot.Placeholder()] = len(ev.tokens) > 0
	}

	var missing []string
	notes := make([]Note, 0, len(evaluatedSlots))
	for _, ev := range evaluatedSlots {
		note := Note{
			Question: ev.qid,
			Slot:     ev.slot.ID,
			Answer:   ev.answer,
			Answered: ev.answered,
			Detail:   describe(ev),
		}
		notes = append(notes, note)
		if len(ev.tokens) == 0 && !ev.slot.Stated && !ev.slot.IsFlag() {
			missing = append(missing, ev.slot.ID)
		}
	}

	// Expand the template: a placeholder contributes zero or more whole
	// tokens, a literal contributes itself.
	argv := make([]string, 0, len(spec.Argv)+4)
	tokensFor := map[string][]string{}
	for _, ev := range evaluatedSlots {
		tokensFor[ev.slot.Placeholder()] = ev.tokens
	}
	for _, tok := range spec.Argv {
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
				return Assembled{}, fmt.Errorf("catalog: refusing token with NUL byte from {%s}", name)
			}
			argv = append(argv, t)
		}
	}

	return Assembled{Argv: argv, Notes: notes, Missing: missing}, nil
}

func describe(ev evaluated) string {
	switch {
	case ev.suppressed != "":
		return fmt.Sprintf("omitido: conflita com %s", ev.suppressed)
	case ev.unstated && ev.slot.IsFlag():
		if len(ev.tokens) > 0 {
			return fmt.Sprintf("incluído: padrão (não mencionado, p=%.2f)", ev.weight)
		}
		return fmt.Sprintf("omitido: não mencionado (p=%.2f)", ev.weight)
	case ev.unstated:
		if len(ev.tokens) == 0 {
			return fmt.Sprintf("omitido: não mencionado (p=%.2f)", ev.weight)
		}
		return fmt.Sprintf("padrão %s: não mencionado (p=%.2f)", strings.Join(ev.tokens, " "), ev.weight)
	case ev.slot.IsFlag():
		if len(ev.tokens) > 0 {
			return fmt.Sprintf("incluído (p=%.2f)", ev.answer.Noul)
		}
		return fmt.Sprintf("omitido (p=%.2f)", ev.answer.Noul)
	case len(ev.tokens) == 0:
		if ev.answer.Choice == NoneKey {
			return "não resolvido: nenhuma opção serve"
		}
		return "nenhum valor"
	default:
		return fmt.Sprintf("%s (confiança %.2f)", strings.Join(ev.tokens, " "), ev.answer.Confidence)
	}
}

// Allowlist returns every program name that any available spec can place in
// argv[0]. It is the last line of defence: even a bug in this catalog cannot
// make jev-cli exec a program that is not on this list.
func Allowlist(specs map[string]Spec, e *env.Env) []string {
	set := map[string]bool{}
	add := func(tok string) {
		if tok == "" || strings.HasPrefix(tok, "-") || strings.ContainsRune(tok, 0) {
			return
		}
		set[tok] = true
	}
	for _, spec := range specs {
		if len(spec.Argv) > 0 {
			if _, isPlaceholder := placeholder(spec.Argv[0]); !isPlaceholder {
				add(spec.Argv[0])
			}
		}
		for _, slot := range spec.Slots {
			for _, v := range slot.options(e) {
				if len(v.Argv) > 0 {
					add(v.Argv[0])
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// findValue looks up an option in a list. A nil env is fine: the answer keys
// are all we need here, and the descriptions are not used.
func findValue(opts []Value, key string) (Value, bool) {
	for _, o := range opts {
		if o.Key == key {
			return o, true
		}
	}
	return Value{}, false
}

// unknownOption is the loud failure for an answer the API should never have
// produced. Papering over it with a default would hide a contract violation,
// which is exactly the kind of silent wrongness this design exists to avoid.
func unknownOption(qid, key string) error {
	return fmt.Errorf("catalog: %q answered with %q, which is not one of its options", qid, key)
}

// Statuses is a stable ordering of note slots, for display.
func (a Assembled) Statuses() []string {
	out := make([]string, 0, len(a.Notes))
	for _, n := range a.Notes {
		out = append(out, n.Slot)
	}
	sort.Strings(out)
	return out
}
