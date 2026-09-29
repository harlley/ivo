package resolve

import (
	"fmt"
	"sort"

	"github.com/harlley/ivo/internal/catalog"
	"github.com/harlley/ivo/internal/discover"
	"github.com/harlley/ivo/internal/model"
)

// Each question searches part of the installed command list by purpose. All
// batches travel in one request; only the finalists need their documentation
// read. The user does not have to know or mention a program's name.
//
// The list that travels is not the whole machine any more. Asking about two
// thousand commands cost about nineteen thousand tokens on every invocation, so
// the commands are asked about in tiers: the priority list, then the programs
// whose own summary matches the words of the request, and the whole list only
// when both come up empty. See the note in internal/discover/priority.go for why
// a list of names is a hint and not a catalogue.
const commandBatchSize = 128
const maxDiscoveredPrograms = 12

// maxShortlist caps the second tier. Together with the priority list it keeps
// the first request near two to four thousand tokens instead of nineteen.
const maxShortlist = 60

// maxDiscoveryCandidates caps the whole narrow list, which fits in a single
// batch and therefore asks its question once.
const maxDiscoveryCandidates = 120

// discoveryRequest asks which installed command fits the request, in tiers.
func (p *Plan) discoveryRequest() (model.Request, bool) {
	return p.discoveryRequestFor(p.discoveryCandidates(), true)
}

// fullDiscoveryRequest is the last resort: the whole command list, which is what
// this stage used to ask every single time. It carries no summaries, because at
// this size the summaries would cost more than the names they describe.
func (p *Plan) fullDiscoveryRequest() (model.Request, bool) {
	return p.discoveryRequestFor(p.Env.Commands, false)
}

func (p *Plan) discoveryRequestFor(candidates []string, withSummaries bool) (model.Request, bool) {
	var index discover.NameIndex
	if withSummaries {
		index = discover.LoadNameIndex()
	}

	questions := map[string]any{}
	criteria := map[string]any{}
	flush := func() {
		if len(criteria) == 0 {
			return
		}
		criteria[catalog.NoneKey] = "None of these programs can fulfill the request."
		questions[fmt.Sprintf("discover.%d", len(questions))] = model.Choice(
			map[string]any{
				"question": "Which installed program is best suited to fulfill the user's request, with appropriate arguments?",
				"focus":    "Match the purpose of the request to what the program does, even when the user never names it. These are executable names, not words to match literally. Choose none if no program in this group fits.",
			}, criteria)
		criteria = map[string]any{}
	}
	for _, name := range candidates {
		if name == catalog.NoneKey {
			continue
		}
		if _, exists := p.Bindings[name]; exists {
			continue
		}
		criteria[name] = nil
		// The summary is the machine's own line about the command, which is what
		// lets the model choose from purpose rather than from memory.
		if desc := index[name]; desc != "" {
			criteria[name] = desc
		}
		if docs, ok := p.Env.Docs[name]; ok && docs.Summary != "" {
			criteria[name] = docs.Summary
		}
		if len(criteria) == commandBatchSize {
			flush()
		}
	}
	flush()
	request := model.Request{
		State: p.Request.State, Model: p.Request.Model, Questions: questions,
	}
	return request, len(questions) > 0
}

// discoveryCandidates is who gets asked about, in tiers: the priority list this
// machine is likely to be asked for, then the commands whose own documentation
// matches the words of the request. An empty result means neither tier had
// anything to go on, and the caller falls back to the whole list.
func (p *Plan) discoveryCandidates() []string {
	if len(p.Env.Commands) <= maxDiscoveryCandidates {
		// The whole list already fits in one batch, so there is nothing to save
		// by narrowing it, and offering all of it keeps recall complete.
		p.Narrowed = false
		return p.Env.Commands
	}

	out := make([]string, 0, maxDiscoveryCandidates)
	seen := map[string]bool{}
	add := func(name string) {
		if name == "" || name == catalog.NoneKey || seen[name] {
			return
		}
		if _, isTool := p.Bindings[name]; isTool {
			return
		}
		if !p.Env.HasCommand(name) {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	// The commands the sentence names come first, because a word that is already
	// a command on this machine is the strongest signal there is. They are
	// candidates and not a decision: the question that follows still asks which
	// program fits, so a sentence that names one program and means another is
	// still answered by the program it meant.
	for _, name := range p.Named {
		add(name)
	}
	for _, name := range discover.Priority() {
		add(name)
	}
	for _, name := range discover.LoadNameIndex().Shortlist(p.Env.Request, p.Env.HasCommand, maxShortlist) {
		add(name)
	}
	if len(out) == 0 {
		// Neither tier had anything to go on: the machine has none of the
		// priority commands and none of its manuals match the words of the
		// request. The whole list is asked, which is what this stage used to do
		// every time.
		p.Narrowed = false
		return p.Env.Commands
	}
	p.Narrowed = true
	if len(out) > maxDiscoveryCandidates {
		out = out[:maxDiscoveryCandidates]
	}
	return out
}

// Reject invented names and names returned for the wrong batch before looking
// up any documentation. A nomination is only a candidate: normal tool choice,
// argument validation, verification and confirmation still follow it.
//
// The second result reports that no batch named anything this machine has, which
// is what makes the expensive tier worth paying for. A weak nomination is not a
// refusal: the model named a program, and its confidence about a list of a
// hundred names is a worse signal than the tool question that follows, where the
// program arrives with its own documentation and the escape hatch is on the
// table. Treating the two as one is what made "how much memory is available"
// answer vm_stat at 0.44 and then pay nineteen thousand tokens to ask again.
func (p *Plan) readDiscovery(request model.Request, response *model.Response) ([]string, bool) {
	type candidate struct {
		name       string
		confidence float64
	}
	var candidates []candidate
	for id, question := range request.Questions {
		choice, ok := question.(model.ChoiceQuestion)
		if !ok {
			continue
		}
		answer, ok := response.Answers[id]
		if !ok || answer.Choice == catalog.NoneKey {
			continue
		}
		if _, offered := choice.Criteria[answer.Choice]; !offered {
			continue
		}
		candidates = append(candidates, candidate{answer.Choice, answer.Confidence})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].confidence == candidates[j].confidence {
			return candidates[i].name < candidates[j].name
		}
		return candidates[i].confidence > candidates[j].confidence
	})
	var added []string
	for _, candidate := range candidates {
		if len(added) == maxDiscoveredPrograms {
			break
		}
		if p.addProgram(candidate.name) {
			added = append(added, candidate.name)
		}
	}
	return added, len(candidates) == 0
}

func (p *Plan) addProgram(name string) bool {
	return p.addProgramWith(name, "")
}

// addProgramWith is addProgram with a description already in hand. The name
// index carries one line per command, which is exactly the line the tool
// question needs, so a whole page does not have to be read to ask whether the
// command fits: the page is read for the program that wins.
func (p *Plan) addProgramWith(name, summary string) bool {
	if name == "" || !p.Env.HasCommand(name) {
		return false
	}
	if _, exists := p.Bindings[name]; exists {
		return false
	}
	program := discover.Program{Name: name, ReadOnly: discover.IsReadOnly(name)}
	switch docs, ok := p.Env.Docs[name]; {
	case ok:
		program.Summary, program.Detail = docs.Summary, docs.Detail
	case summary != "":
		program.Summary = summary
	default:
		// A candidate is not authorization to execute it, even with --help.
		// Read cached documentation or its manual without starting the program.
		docs := discover.Inspect(name)
		if p.Env.Docs == nil {
			p.Env.Docs = map[string]discover.Docs{}
		}
		p.Env.Docs[name] = docs
		program.Summary, program.Detail = docs.Summary, docs.Detail
	}
	tool := catalog.NamedProgram(program)
	binding, ok := tool.Bind(p.Env)
	if !ok {
		return false
	}
	p.Tools = append(p.Tools, tool)
	p.Bindings[name] = binding
	return true
}
