// Package typesafe is a small, dependency-free client for the TypeSafe AI
// "System One" evaluation endpoint.
//
// A System One model (jev) does not generate text. It evaluates a `state`
// against a map of typed `questions` and returns structured `answers` that
// ordinary code can branch on: a choice from a closed set, a score on a
// rubric, or the probability that a yes/no statement is true.
//
//	POST https://api.typesafe.ai/v1/systemone
//	Authorization: Bearer <API_KEY>
package typesafe

import (
	"fmt"
	"math"
	"strings"
)

const (
	// DefaultBaseURL is the TypeSafe API host.
	DefaultBaseURL = "https://api.typesafe.ai"
	// DefaultModel is the flagship System One model alias.
	DefaultModel = "jev-latest"
	// SystemOnePath is the evaluation endpoint path.
	SystemOnePath = "/v1/systemone"
	// EnvAPIKey is the environment variable holding the API key.
	EnvAPIKey = "TYPESAFE_API_KEY"
)

// MaxChoiceOptions is the documented ceiling on options in a single Choice
// question. Exceeding it is a 422 from the API, so we enforce it client-side.
const MaxChoiceOptions = 255

// MaxScoreLevels is the documented ceiling on levels in a single Score.
const MaxScoreLevels = 10

// Kind is the type discriminator of a System One question.
type Kind string

// The three System One question types.
const (
	KindNoul   Kind = "noul"
	KindChoice Kind = "choice"
	KindScore  Kind = "score"
)

// Instructions may be a plain string, or a structured object/array. Structure
// is the documented way to hand a question the data it should refer to: put
// the question in one field and the data in the others, then point at the data
// by name in backticks.
type Instructions = any

// Question is a typed question. The concrete types are NoulQuestion,
// ChoiceQuestion and ScoreQuestion; each marshals to the wire shape the API
// expects, with `type`, `instructions` and `criteria`.
type Question interface {
	// Kind reports the wire value of the question's `type` field.
	Kind() Kind
	// Validate reports whether the question is well formed enough to send.
	Validate() error
}

// NoulCriteria describes what a yes and a no mean. Both sides are optional but
// strongly recommended: they are how you separate a true signal from a
// plausible-looking one.
type NoulCriteria struct {
	True  Instructions `json:"true,omitempty"`
	False Instructions `json:"false,omitempty"`
}

// NoulQuestion asks a yes/no question and returns the probability of yes.
type NoulQuestion struct {
	Type         Kind          `json:"type"`
	Instructions Instructions  `json:"instructions"`
	Criteria     *NoulCriteria `json:"criteria,omitempty"`
}

// Noul builds a yes/no question.
func Noul(instructions Instructions, criteria *NoulCriteria) NoulQuestion {
	return NoulQuestion{Type: KindNoul, Instructions: instructions, Criteria: criteria}
}

// Kind implements Question.
func (NoulQuestion) Kind() Kind { return KindNoul }

// Validate implements Question.
func (q NoulQuestion) Validate() error {
	if isEmpty(q.Instructions) {
		return fmt.Errorf("noul question: instructions are required")
	}
	return nil
}

// ChoiceQuestion picks one option out of a closed set. Criteria maps each
// option to a description of that option; a nil description is allowed when
// the option name is self-explanatory.
type ChoiceQuestion struct {
	Type         Kind           `json:"type"`
	Instructions Instructions   `json:"instructions"`
	Criteria     map[string]any `json:"criteria"`
}

// Choice builds a question that selects one option from a closed set.
func Choice(instructions Instructions, criteria map[string]any) ChoiceQuestion {
	return ChoiceQuestion{Type: KindChoice, Instructions: instructions, Criteria: criteria}
}

// Kind implements Question.
func (ChoiceQuestion) Kind() Kind { return KindChoice }

// Validate implements Question.
func (q ChoiceQuestion) Validate() error {
	if isEmpty(q.Instructions) {
		return fmt.Errorf("choice question: instructions are required")
	}
	if len(q.Criteria) < 2 {
		return fmt.Errorf("choice question: at least 2 options are required, got %d", len(q.Criteria))
	}
	if len(q.Criteria) > MaxChoiceOptions {
		return fmt.Errorf("choice question: %d options exceeds the documented limit of %d", len(q.Criteria), MaxChoiceOptions)
	}
	return nil
}

// ScoreQuestion rates the state along an ordered list of levels. The first
// level is the lowest.
type ScoreQuestion struct {
	Type         Kind         `json:"type"`
	Instructions Instructions `json:"instructions"`
	Criteria     []any        `json:"criteria"`
}

// Score builds a question that rates the state on an ordered rubric.
func Score(instructions Instructions, levels []any) ScoreQuestion {
	return ScoreQuestion{Type: KindScore, Instructions: instructions, Criteria: levels}
}

// Kind implements Question.
func (ScoreQuestion) Kind() Kind { return KindScore }

// Validate implements Question.
func (q ScoreQuestion) Validate() error {
	if isEmpty(q.Instructions) {
		return fmt.Errorf("score question: instructions are required")
	}
	if len(q.Criteria) < 2 || len(q.Criteria) > MaxScoreLevels {
		return fmt.Errorf("score question: must have between 2 and %d levels, got %d", MaxScoreLevels, len(q.Criteria))
	}
	return nil
}

// SystemOneRequest is the body of POST /v1/systemone.
type SystemOneRequest struct {
	State     any            `json:"state"`
	Model     string         `json:"model"`
	Questions map[string]any `json:"questions"`
}

// Validate checks the request against the documented constraints.
func (r SystemOneRequest) Validate() error {
	if r.State == nil {
		return fmt.Errorf("request: state is required")
	}
	if len(r.Questions) == 0 {
		return fmt.Errorf("request: at least one question is required")
	}
	for id, q := range r.Questions {
		switch v := q.(type) {
		case Question:
			if err := v.Validate(); err != nil {
				return fmt.Errorf("request: question %q: %w", id, err)
			}
		case nil:
			return fmt.Errorf("request: question %q is nil", id)
		default:
			return fmt.Errorf("request: question %q is not a typesafe.Question (%T)", id, q)
		}
	}
	return nil
}

// Usage reports the token cost of an evaluation.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Total returns the combined token count.
func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

// SystemOneResponse is the body returned by POST /v1/systemone.
type SystemOneResponse struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// Answer is the structured answer to one question. The populated fields depend
// on Type; the unused ones stay at their zero value.
type Answer struct {
	Type Kind `json:"type"`

	// Noul is the probability that the yes/no statement is true, in [0,1].
	Noul float64 `json:"noul,omitempty"`

	// Choice is the highest-probability option.
	Choice string `json:"choice,omitempty"`

	// Score is the probability-weighted rating across the levels. It can land
	// between levels.
	Score float64 `json:"score,omitempty"`

	// Legend maps each level number back to its description (Score only).
	Legend map[string]string `json:"legend,omitempty"`

	// Probabilities is the full distribution: option -> probability for a
	// Choice, level index -> probability for a Score. Values sum to 1.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`

	// Confidence is how certain the model is, derived from the shape of
	// Probabilities. Noul answers do not carry one.
	Confidence float64 `json:"confidence,omitempty"`
}

// Probability returns the probability of an option (Choice) or of a level
// index (Score).
func (a Answer) Probability(key string) float64 { return a.Probabilities[key] }

// LevelProbability returns the probability that a Score landed on a level
// index. Level 0 is the lowest.
func (a Answer) LevelProbability(level int) float64 {
	return a.Probabilities[fmt.Sprintf("%d", level)]
}

// Ranking returns the options of a Choice (or the levels of a Score as
// "0","1",...) ordered from most to least probable.
func (a Answer) Ranking() []RankedOption {
	out := make([]RankedOption, 0, len(a.Probabilities))
	for k, v := range a.Probabilities {
		out = append(out, RankedOption{Key: k, Probability: v})
	}
	sortOptions(out)
	return out
}

// RankedOption is one entry of a probability distribution.
type RankedOption struct {
	Key         string
	Probability float64
	// Description is filled in by callers that keep a label for the key.
	Description string
}

func sortOptions(opts []RankedOption) {
	// Insertion sort: distributions are tiny (a handful of options) and this
	// keeps the ordering deterministic for equal probabilities.
	for i := 1; i < len(opts); i++ {
		for j := i; j > 0; j-- {
			if opts[j].Probability > opts[j-1].Probability ||
				(opts[j].Probability == opts[j-1].Probability && opts[j].Key < opts[j-1].Key) {
				opts[j], opts[j-1] = opts[j-1], opts[j]
			} else {
				break
			}
		}
	}
}

// Round returns the answer's confidence rounded to n decimal places, for
// display.
func (a Answer) Round(n int) float64 {
	f := math.Pow(10, float64(n))
	return math.Round(a.Confidence*f) / f
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return false
}
