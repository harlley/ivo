// Package typesafe implements the Jev adapter for TypeSafe System One.
package typesafe

import "github.com/harlley/ivo/internal/model"

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

type Kind = model.Kind
type Instructions = model.Instructions
type Question = model.Question
type NoulCriteria = model.NoulCriteria
type NoulQuestion = model.NoulQuestion
type ChoiceQuestion = model.ChoiceQuestion
type ScoreQuestion = model.ScoreQuestion
type Usage = model.Usage
type Answer = model.Answer
type RankedOption = model.RankedOption
type SystemOneRequest = model.Request
type SystemOneResponse = model.Response
type Result = model.Result

const MaxChoiceOptions = model.MaxChoiceOptions
const MaxScoreLevels = model.MaxScoreLevels
const KindNoul = model.KindNoul
const KindChoice = model.KindChoice
const KindScore = model.KindScore

var Noul = model.Noul
var Choice = model.Choice
var Score = model.Score
