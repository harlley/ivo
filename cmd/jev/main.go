// Command jev turns a phrase in natural language into a shell command.
//
//	jev "list all files in this directory"
//
// The command is never written by a language model. jev asks a System One
// model (TypeSafe's jev) a set of typed questions about a fixed catalog of
// commands and a fixed set of options, and this program assembles the argv
// from the answers.
//
// The command runs once the gates pass: the model was confident, the
// guardrails were satisfied, and the catalog entry is read-only. Pass
// --dry-run to see the resolved command without running it.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/harlleyoliveira/jev-cli/internal/catalog"
	"github.com/harlleyoliveira/jev-cli/internal/config"
	"github.com/harlleyoliveira/jev-cli/internal/env"
	"github.com/harlleyoliveira/jev-cli/internal/resolve"
	"github.com/harlleyoliveira/jev-cli/internal/run"
	"github.com/harlleyoliveira/jev-cli/internal/typesafe"
	"github.com/harlleyoliveira/jev-cli/internal/ui"
)

// version is the released version, also settable at build time with
// -ldflags "-X main.version=...".
var version = "0.1.0"

// Exit codes. 0-4 are jev-cli's own; a non-zero exit from an executed command
// is propagated unchanged.
const (
	exitOK         = 0
	exitError      = 1
	exitUnresolved = 2 // nothing was resolved: ambiguous or unsupported
	exitBlocked    = 3 // a guardrail stopped it
	exitNoKey      = 4 // no usable API key
)

func main() { os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr)) }

// realMain takes its streams explicitly so the whole pipeline can be tested
// end to end, including the argument parsing and the output.
func realMain(args []string, stdout, stderr io.Writer) int {
	opts, phrase, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "jev: %v\n", err)
		fmt.Fprintln(stderr, "see `jev --help`")
		return exitUnresolved
	}
	if opts.help {
		printHelp(stdout)
		return exitOK
	}
	if opts.version {
		fmt.Fprintf(stdout, "jev %s\n", version)
		return exitOK
	}

	cfg, cfgPath, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "jev: config: %v\n", err)
		return exitError
	}
	applyFlags(&cfg, opts)

	color := !cfg.NoColor && os.Getenv("NO_COLOR") == "" && ui.ColorSupported(stdout)
	printer := ui.New(color, stdout, stderr)
	for _, w := range cfg.Warnings() {
		printer.Warn("warning: %s", w)
	}

	systemEnv, err := env.Probe(env.ProbeOptions{
		Request:    phrase,
		MaxEntries: cfg.MaxEntries,
		Binaries:   catalog.Binaries(),
	})
	if err != nil {
		printer.Error("could not inspect the environment: %v", err)
		return exitError
	}

	if opts.listCommands {
		printCommands(printer, systemEnv)
		return exitOK
	}
	if strings.TrimSpace(phrase) == "" {
		printer.Error("missing the phrase, for example: jev \"list all files in this directory\"")
		return exitUnresolved
	}

	apiKey := cfg.APIKey
	if opts.apiKeyFile != "" {
		data, err := os.ReadFile(opts.apiKeyFile)
		if err != nil {
			printer.Error("could not read --api-key-file: %v", err)
			return exitError
		}
		apiKey = strings.TrimSpace(string(data))
	}
	if apiKey == "" {
		printer.Error("no API key. Set TYPESAFE_API_KEY, or write {\"api_key\": \"...\"} to %s", cfgPath)
		return exitNoKey
	}

	forced := map[string]string{}
	if opts.path != "" {
		forced["target_path"] = opts.path
	}
	if opts.pattern != "" {
		forced["name_pattern"] = opts.pattern
	}
	if opts.term != "" {
		forced["search_terms"] = opts.term
	}

	plan, err := resolve.Build(systemEnv, resolve.Options{Model: cfg.Model, Forced: forced})
	if err != nil {
		printer.Error("%v", err)
		return exitError
	}

	if opts.explain {
		printer.Title("request sent")
		if pretty, err := json.MarshalIndent(plan.Request, "", "  "); err == nil {
			printer.Say("%s", pretty)
		}
	}

	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	client := typesafe.NewClient(apiKey, typesafe.WithBaseURL(cfg.BaseURL))
	result, err := client.SystemOne(ctx, plan.Request)
	if err != nil {
		if typesafe.IsAuthError(err) {
			printer.Error("the API rejected the API key. Check %s or TYPESAFE_API_KEY.", cfgPath)
			return exitNoKey
		}
		printer.Error("%v", err)
		return exitError
	}

	decision, err := plan.Decide(result.Response, resolve.DecideOptions{
		MinConfidence:        cfg.MinConfidence,
		ClarityThreshold:     cfg.ClarityThreshold,
		DestructiveThreshold: cfg.DestructiveThreshold,
		InjectionThreshold:   cfg.InjectionThreshold,
		SeverityThreshold:    cfg.SeverityThreshold,
		AllowWrite:           cfg.AllowWrite,
	})
	if err != nil {
		printer.Error("%v", err)
		return exitError
	}

	if opts.explain {
		printer.Title("raw response")
		printer.Say("%s", string(result.Raw))
	}

	allow := catalog.Allowlist(plan.Specs, systemEnv)

	// A dry run stops here: the user sees the command and decides.
	if decision.Verdict != resolve.VerdictAct {
		return renderNoCommand(printer, opts, plan, decision, result)
	}
	if opts.dryRun {
		return renderDryRun(printer, opts, plan, decision, result, allow)
	}

	outcome, err := run.Do(ctx, decision.Argv, run.Options{
		Execute:        true,
		Allow:          allow,
		Capture:        opts.json,
		MaxOutputBytes: run.DefaultMaxOutputBytes,
		Stdin:          os.Stdin,
		Stdout:         stdout,
		Stderr:         stderr,
	})
	if err != nil {
		printer.Error("%v", err)
		return exitError
	}
	if opts.json {
		emitJSON(printer, plan, decision, result, &outcome)
	}
	if outcome.ExitCode != 0 {
		return outcome.ExitCode
	}
	return exitOK
}

// renderDryRun prints what would run and stops.
func renderDryRun(p *ui.Printer, opts cliOptions, plan *resolve.Plan, d *resolve.Decision, res *typesafe.Result, allow []string) int {
	if opts.json {
		emitJSON(p, plan, d, res, nil)
		return exitOK
	}
	p.Command(ui.ShellQuote(d.Argv))
	p.Field("command", fmt.Sprintf("%s, confidence %.2f", d.Command.ID, d.Intent.Confidence))
	if d.Severity > 0 {
		p.Field("severity", fmt.Sprintf("%.2f", d.Severity))
	}
	printNotes(p, d, opts.explain)
	if opts.explain {
		p.Field("questions", fmt.Sprintf("%d", len(plan.Request.Questions)))
		p.Field("usage", fmt.Sprintf("%d input tokens, %d output tokens", res.Response.Usage.InputTokens, res.Response.Usage.OutputTokens))
		p.Field("latency", fmt.Sprintf("%d ms (%d attempt(s))", res.Latency.Milliseconds(), res.Attempts))
		p.Field("binaries", strings.Join(allow, ", "))
	}
	p.Hint("dry-run: nothing ran. Run again without --dry-run to execute.")
	return exitOK
}

// renderNoCommand explains why nothing will run.
func renderNoCommand(p *ui.Printer, opts cliOptions, plan *resolve.Plan, d *resolve.Decision, res *typesafe.Result) int {
	if opts.json {
		emitJSON(p, plan, d, res, nil)
		switch d.Verdict {
		case resolve.VerdictBlocked:
			return exitBlocked
		default:
			return exitUnresolved
		}
	}

	switch d.Verdict {
	case resolve.VerdictBlocked:
		p.Error("blocked: %s", d.Reason)
	case resolve.VerdictUnsupported:
		p.Warn("I cannot do that: %s", d.Reason)
		p.Hint("this CLI only runs: %s", strings.Join(commandIDs(plan.Commands), ", "))
	case resolve.VerdictAsk:
		p.Warn("not going to guess: %s", d.Reason)
		// A command that was resolved but gated is still worth showing: it is
		// the tool's best reading of the phrase, and seeing it is how the user
		// decides whether to rephrase. It is labelled so it can never be
		// mistaken for something that ran.
		if len(d.Argv) > 0 && d.Command != nil {
			p.Title("suggestion (not executed)")
			p.Command(ui.ShellQuote(d.Argv))
			p.Field("command", d.Command.ID)
			printNotes(p, d, opts.explain)
		}
	default:
		p.Warn("%s", d.Reason)
	}

	if len(d.Alternatives) > 0 {
		p.Title("candidates")
		for _, alt := range d.Alternatives {
			line := fmt.Sprintf("%-24s %.2f", alt.Key, alt.Probability)
			if alt.Description != "" {
				line += "  " + alt.Description
			}
			p.Line("%s", line)
		}
	}
	if len(d.Guardrails) > 0 {
		p.Field("guardrails", fmt.Sprintf("injection %.2f | destructive %.2f | clarity %.2f | severity %.2f",
			d.Guardrails["guardrail.injection"].Noul,
			d.Guardrails["guardrail.destructive_request"].Noul,
			d.Guardrails["guardrail.intent_clear"].Noul,
			d.Severity))
	}
	p.Hint("nothing ran.")

	if d.Verdict == resolve.VerdictBlocked {
		return exitBlocked
	}
	return exitUnresolved
}

// printNotes shows every decision that shaped the command line. Being able to
// read *why* the command looks the way it does is the point of the tool, so
// the default view keeps all of them and --explain adds the raw exchange.
func printNotes(p *ui.Printer, d *resolve.Decision, explain bool) {
	for _, note := range d.Notes {
		if !note.Answered && !explain {
			continue
		}
		p.Field(note.Slot, note.Detail)
	}
}

func printCommands(p *ui.Printer, e *env.Env) {
	available := catalog.Available(catalog.All(), e)
	ids := map[string]bool{}
	for _, c := range available {
		ids[c.ID] = true
	}
	p.Title("catalog (closed vocabulary)")
	for _, c := range catalog.All() {
		mark := "  "
		if !ids[c.ID] {
			mark = "| "
		}
		kind := "read"
		if !c.ReadOnly {
			kind = "write"
		}
		p.Line("%s%-24s [%s]", mark, c.ID, kind)
		p.Line("    %s", c.What)
		if c.NotFor != "" {
			p.Line("    not for: %s", c.NotFor)
		}
		if !ids[c.ID] {
			missing := make([]string, 0, len(c.Needs))
			for _, bin := range c.Needs {
				if !e.Has(bin) {
					missing = append(missing, bin)
				}
			}
			if len(missing) > 0 {
				p.Line("    unavailable: missing %s", strings.Join(missing, ", "))
			} else {
				p.Line("    unavailable in this invocation: there are no candidates to choose from (it depends on the phrase)")
			}
		}
	}
	p.Hint("- = unavailable here; availability depends on the environment and the phrase")
}

func commandIDs(cmds []catalog.Command) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c.ID)
	}
	return out
}

// ---------------------------------------------------------------------------
// JSON output
// ---------------------------------------------------------------------------

type jsonOption struct {
	Key         string  `json:"key"`
	Probability float64 `json:"probability"`
	Description string  `json:"description,omitempty"`
}

type jsonNote struct {
	Slot   string `json:"slot"`
	Detail string `json:"detail"`
}

type jsonDecision struct {
	Request      string             `json:"request"`
	Model        string             `json:"model"`
	Verdict      string             `json:"verdict"`
	Reason       string             `json:"reason,omitempty"`
	Command      string             `json:"command,omitempty"`
	Argv         []string           `json:"argv,omitempty"`
	CommandLine  string             `json:"command_line,omitempty"`
	Confidence   float64            `json:"intent_confidence"`
	Severity     float64            `json:"severity"`
	Guardrails   map[string]float64 `json:"guardrails,omitempty"`
	Alternatives []jsonOption       `json:"alternatives,omitempty"`
	Decisions    []jsonNote         `json:"decisions,omitempty"`
	Executed     bool               `json:"executed"`
	ExitCode     *int               `json:"exit_code,omitempty"`
	Stdout       string             `json:"stdout,omitempty"`
	Stderr       string             `json:"stderr,omitempty"`
	Truncated    bool               `json:"output_truncated,omitempty"`
	Usage        typesafe.Usage     `json:"usage"`
	LatencyMS    int64              `json:"latency_ms"`
	Attempts     int                `json:"attempts"`
}

func emitJSON(p *ui.Printer, plan *resolve.Plan, d *resolve.Decision, res *typesafe.Result, outcome *run.Outcome) {
	doc := jsonDecision{
		Request:    plan.Env.Request,
		Model:      res.Response.Model,
		Verdict:    string(d.Verdict),
		Reason:     d.Reason,
		Confidence: d.Intent.Confidence,
		Severity:   d.Severity,
		Guardrails: map[string]float64{
			"injection":   d.Guardrails["guardrail.injection"].Noul,
			"destructive": d.Guardrails["guardrail.destructive_request"].Noul,
			"clarity":     d.Guardrails["guardrail.intent_clear"].Noul,
		},
		Usage:     res.Response.Usage,
		LatencyMS: res.Latency.Milliseconds(),
		Attempts:  res.Attempts,
	}
	if d.Command != nil {
		doc.Command = d.Command.ID
		doc.Argv = d.Argv
		doc.CommandLine = ui.ShellQuote(d.Argv)
	}
	for _, alt := range d.Alternatives {
		doc.Alternatives = append(doc.Alternatives, jsonOption{
			Key: alt.Key, Probability: alt.Probability, Description: alt.Description,
		})
	}
	for _, note := range d.Notes {
		doc.Decisions = append(doc.Decisions, jsonNote{Slot: note.Slot, Detail: note.Detail})
	}
	if outcome != nil {
		doc.Executed = outcome.Ran
		code := outcome.ExitCode
		doc.ExitCode = &code
		doc.Stdout = outcome.Stdout
		doc.Stderr = outcome.Stderr
		doc.Truncated = outcome.Truncated
	}

	encoder := json.NewEncoder(p.Out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(doc); err != nil {
		p.Error("could not serialize the result: %v", err)
	}
}

// ---------------------------------------------------------------------------
// arguments
// ---------------------------------------------------------------------------

type cliOptions struct {
	dryRun       bool
	allowWrite   bool
	json         bool
	explain      bool
	noColor      bool
	help         bool
	version      bool
	listCommands bool

	path    string
	pattern string
	term    string

	model      string
	baseURL    string
	apiKeyFile string

	maxEntries    int
	minConfidence float64
	timeoutSecs   int

	// set tracks which numeric flags the user actually passed, so a zero value
	// never silently overrides the config file.
	set map[string]bool
}

func parseArgs(args []string) (cliOptions, string, error) {
	opts := cliOptions{set: map[string]bool{}}
	var phrase []string
	afterSeparator := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if afterSeparator {
			phrase = append(phrase, arg)
			continue
		}
		if arg == "--" {
			afterSeparator = true
			continue
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			phrase = append(phrase, arg)
			continue
		}

		name, value, hasValue := arg, "", false
		if idx := strings.Index(arg, "="); idx > 1 {
			name, value, hasValue = arg[:idx], arg[idx+1:], true
		}

		var err error
		switch name {
		case "-n", "--dry-run":
			opts.dryRun = true
		case "-x", "--execute":
			// Executing is the default. The flag stays for scripts that want
			// to say so out loud, and it turns a --dry-run earlier in the
			// argument list back off.
			opts.dryRun = false
		case "--allow-write":
			opts.allowWrite = true
		case "-j", "--json":
			opts.json = true
		case "-v", "--explain":
			opts.explain = true
		case "--no-color":
			opts.noColor = true
		case "-h", "--help":
			opts.help = true
		case "-V", "--version":
			opts.version = true
		case "--commands":
			opts.listCommands = true
		case "--path":
			opts.path, err = takeValue(args, &i, name, value, hasValue)
		case "--pattern":
			opts.pattern, err = takeValue(args, &i, name, value, hasValue)
		case "--term":
			opts.term, err = takeValue(args, &i, name, value, hasValue)
		case "-m", "--model":
			opts.model, err = takeValue(args, &i, name, value, hasValue)
		case "--base-url":
			opts.baseURL, err = takeValue(args, &i, name, value, hasValue)
		case "--api-key-file":
			opts.apiKeyFile, err = takeValue(args, &i, name, value, hasValue)
		case "--max-entries":
			var raw string
			if raw, err = takeValue(args, &i, name, value, hasValue); err == nil {
				opts.maxEntries, err = strconv.Atoi(raw)
				opts.set["max-entries"] = true
			}
		case "--min-confidence":
			var raw string
			if raw, err = takeValue(args, &i, name, value, hasValue); err == nil {
				opts.minConfidence, err = strconv.ParseFloat(raw, 64)
				opts.set["min-confidence"] = true
			}
		case "--timeout":
			var raw string
			if raw, err = takeValue(args, &i, name, value, hasValue); err == nil {
				opts.timeoutSecs, err = strconv.Atoi(raw)
				opts.set["timeout"] = true
			}
		default:
			return opts, "", fmt.Errorf("unknown option: %s", name)
		}
		if err != nil {
			return opts, "", err
		}
	}
	return opts, strings.Join(phrase, " "), nil
}

func takeValue(args []string, i *int, name, inline string, hasInline bool) (string, error) {
	if hasInline {
		return inline, nil
	}
	if *i+1 >= len(args) {
		return "", fmt.Errorf("option %s needs a value", name)
	}
	*i++
	return args[*i], nil
}

func applyFlags(cfg *config.Config, opts cliOptions) {
	if opts.model != "" {
		cfg.Model = opts.model
	}
	if opts.baseURL != "" {
		cfg.BaseURL = opts.baseURL
	}
	if opts.allowWrite {
		cfg.AllowWrite = true
	}
	if opts.noColor {
		cfg.NoColor = true
	}
	if opts.set["max-entries"] {
		cfg.MaxEntries = opts.maxEntries
	}
	if opts.set["min-confidence"] {
		cfg.MinConfidence = opts.minConfidence
	}
	if opts.set["timeout"] {
		cfg.TimeoutSeconds = opts.timeoutSecs
	}
}

func printHelp(w io.Writer) {
	fmt.Fprint(w, helpText)
}

var helpText = `jev: turn a phrase in natural language into a shell command.

USAGE
  jev [options] "phrase in natural language"

The command is never written by a language model. jev answers typed questions
about a fixed catalog of commands and options, and this program assembles the
argv from the answers. It runs once the gates pass; --dry-run shows it without
running anything.

EXAMPLES
  jev "list all files in this directory"
  jev --dry-run "how much space does this directory take"
  jev --path src --pattern '*.go' "find the go files under src"
  jev --explain "show me the last commits"
  jev --commands

EXECUTION
  (default)                run the resolved command
  -n, --dry-run            show the resolved command and stop, running nothing
  -x, --execute            run the resolved command (already the default)
  --allow-write            allow commands that are not read-only
  -j, --json               JSON result; the executed command's output is captured

EXPLICIT VALUES
  --path VALUE             use this path, without asking the model
  --pattern VALUE          use this file name pattern
  --term VALUE             use this search text

DIAGNOSTICS
  -v, --explain            show the request, the raw response, notes and usage
  --commands               list the closed catalog and what is available here
  --no-color               no colours
  -V, --version            version
  -h, --help               this help

TUNING
  -m, --model NAME         model (default: ` + typesafe.DefaultModel + `)
  --base-url URL           API host (default: ` + typesafe.DefaultBaseURL + `)
  --api-key-file FILE      read the API key from a file
  --max-entries N          maximum directory entries in the state
  --min-confidence F       minimum confidence to act (default 0.5)
  --timeout S              API call timeout, in seconds

CONFIG
  TYPESAFE_API_KEY         TypeSafe API key
  JEV_MODEL, JEV_BASE_URL, JEV_CONFIG
  file: ~/.config/jev/config.json

EXIT CODES
  0 success, 1 error, 2 unresolved (ambiguous or outside the catalog)
  3 blocked by a guardrail, 4 no API key
  the exit code of the executed command is propagated
`
