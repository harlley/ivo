// Command ivo turns a phrase in natural language into a shell command.
//
//	ivo "list all files in this directory"
//
// The command is never written by a language model. ivo asks a System One
// model adapter (currently TypeSafe's Jev) typed questions about installed programs
// and their documented options, and this program assembles the argv
// from the answers.
//
// The resolved command is verified and shown for confirmation before execution.
// Pass --dry-run to inspect it without running it, or --yolo to skip confirmation.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/harlley/ivo/internal/catalog"
	"github.com/harlley/ivo/internal/discover"
	"github.com/harlley/ivo/internal/env"
	"github.com/harlley/ivo/internal/resolve"
	"github.com/harlley/ivo/internal/run"
	"github.com/harlley/ivo/internal/typesafe"
	"github.com/harlley/ivo/internal/ui"
)

// version is the released version, also settable at build time with
// -ldflags "-X main.version=...".
var version = "0.1.0"

// commit and builtAt are the build identity, set at build time by
// scripts/dev-run.sh. A binary that cannot say which source it came from is
// worse than it sounds: a bug fixed after the last install keeps happening, and
// nothing about the output tells you that you are running the old thing.
var (
	commit  = ""
	builtAt = ""
)

// versionLine names the version and, when the build says so, the source it came
// from. A plain go build leaves the identity out rather than inventing one.
func versionLine() string {
	if commit == "" {
		return "ivo " + version
	}
	identity := commit
	if builtAt != "" {
		identity += ", built " + builtAt
	}
	return fmt.Sprintf("ivo %s (%s)", version, identity)
}

const (
	// maxEntries caps how many directory entries go into the state.
	maxEntries = 120
	// apiTimeout bounds one call to the API.
	apiTimeout = 30 * time.Second
)

// Exit codes. 1-4 are ivo's own; a non-zero exit from an executed command
// is propagated unchanged.
const (
	exitOK         = 0
	exitError      = 1
	exitUnresolved = 2 // nothing was resolved: ambiguous or outside the catalog
	exitBlocked    = 3 // a guardrail stopped it
	exitNoKey      = 4 // no usable API key
)

func main() { os.Exit(realMain(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

// realMain takes its streams explicitly so the whole pipeline can be tested end
// to end, including argument parsing and output.
func realMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts, phrase, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "ivo: %v\n", err)
		fmt.Fprintln(stderr, "see `ivo --help`")
		return exitUnresolved
	}
	if opts.help {
		fmt.Fprint(stdout, helpText)
		return exitOK
	}
	if opts.version {
		fmt.Fprintln(stdout, versionLine())
		return exitOK
	}

	color := os.Getenv("NO_COLOR") == "" && ui.ColorSupported(stdout)
	printer := ui.New(color, stdout, stderr)

	systemEnv, err := env.Probe(env.ProbeOptions{
		Request:          phrase,
		MaxEntries:       maxEntries,
		DiscoverCommands: true,
	})
	if err != nil {
		printer.Error("could not inspect the environment: %v", err)
		return exitError
	}

	// The catalog's own documentation needs neither the model nor a key, and
	// takes the phrase as an optional tool name.
	if opts.tools {
		renderTools(printer, systemEnv, phrase)
		return exitOK
	}

	if strings.TrimSpace(phrase) == "" {
		printer.Error("missing the phrase, for example: ivo \"list all files in this directory\"")
		return exitUnresolved
	}

	apiKey := strings.TrimSpace(os.Getenv(typesafe.EnvAPIKey))
	if apiKey == "" {
		printer.Error("no API key. Set %s, for example:", typesafe.EnvAPIKey)
		printer.Hint(`  export %s="$(security find-generic-password -a "$USER" -s %s -w)"`,
			typesafe.EnvAPIKey, typesafe.EnvAPIKey)
		return exitNoKey
	}

	plan, err := resolve.Build(systemEnv)
	if err != nil {
		printer.Error("%v", err)
		return exitError
	}

	ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
	defer cancel()

	client := typesafe.NewClient(apiKey, typesafe.WithBaseURL(baseURL()))
	evaluation, err := plan.Evaluate(ctx, client, resolve.DecideOptions{})
	if err != nil {
		if typesafe.IsAuthError(err) {
			printer.Error("the API rejected the API key. Check %s.", typesafe.EnvAPIKey)
			return exitNoKey
		}
		printer.Error("%v", err)
		return exitError
	}
	decision := evaluation.Decision

	// Anything but a confident, unblocked verdict stops here and explains
	// itself. This is the only thing standing between a phrase and a command,
	// so it is deliberately the last thing before execution.
	if decision.Verdict != resolve.VerdictAct {
		return renderNoCommand(printer, decision, plan)
	}

	if opts.dryRun {
		renderDryRun(printer, decision)
		return exitOK
	}

	// Nothing runs without being asked for, unless --yolo was passed or the
	// prompt has already been answered.
	if !opts.yolo {
		renderConfirmation(printer, decision)
		if decision.Caution != "" {
			printer.Warn("careful: %s", decision.Caution)
		}
		if !printer.Confirm(stdin, "run it? [Y/n]") {
			printer.Hint("nothing ran. Pass --yolo to run without being asked.")
			return exitUnresolved
		}
	}

	outcome, err := run.Do(ctx, decision.Argv, run.Options{
		Execute: true,
		Allow:   catalog.Allowlist(plan.Bindings, systemEnv),
		Filter:  outputFilter(decision.Output),
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  stderr,
	})
	// The command ran because a person confirmed it, so it is a command worth
	// asking about first next time. A nomination that was never confirmed is not:
	// promoting one would let a wrong answer push a program up the list it was
	// chosen from.
	discover.Promote(decision.Argv[0])
	if err != nil {
		printer.Error("%v", err)
		return exitError
	}
	return outcome.ExitCode
}

// outputFilter maps a binding's declared output adaptation onto the function
// that performs it. The catalog says what the output needs; run knows how.
func outputFilter(kind catalog.Output) func([]byte) []byte {
	if kind == catalog.OutputStripOverstrike {
		return run.StripOverstrike
	}
	return nil
}

// baseURL is the API host. It is an environment variable rather than a flag so
// the CLI surface stays small, and so the tests can point at a fake server.
func baseURL() string {
	if v := strings.TrimSpace(os.Getenv("IVO_BASE_URL")); v != "" {
		return v
	}
	return typesafe.DefaultBaseURL
}

// renderConfirmation shows the call that is about to run and why, so the answer
// is made on evidence rather than on trust.
func renderConfirmation(p *ui.Printer, d *resolve.Decision) {
	p.Command(ui.ShellQuote(d.Argv))
	p.Field("command", fmt.Sprintf("%s, confidence %.2f", d.Tool.Name, d.Intent.Confidence))
	printOptions(p, d)
}

// printOptions shows what the walk chose to add, which is part of what the
// person is approving.
func printOptions(p *ui.Printer, d *resolve.Decision) {
	if len(d.Options) == 0 {
		p.Field("options", "none")
		return
	}
	p.Field("options", strings.Join(d.Options, ", "))
}

// renderDryRun prints what would run and stops.
func renderDryRun(p *ui.Printer, d *resolve.Decision) {
	p.Command(ui.ShellQuote(d.Argv))
	p.Field("command", fmt.Sprintf("%s, confidence %.2f", d.Tool.Name, d.Intent.Confidence))
	p.Field("severity", fmt.Sprintf("%.2f", d.Severity))
	printOptions(p, d)
	printNotes(p, d)
	p.Hint("dry-run: nothing ran. Run it again without --dry-run to be asked, or with --yolo to run straight away.")
}

// renderNoCommand explains why nothing will run.
func renderNoCommand(p *ui.Printer, d *resolve.Decision, plan *resolve.Plan) int {
	switch d.Verdict {
	case resolve.VerdictBlocked:
		p.Error("blocked: %s", d.Reason)
	case resolve.VerdictUnsupported:
		p.Warn("I cannot do that: %s", d.Reason)
		p.Hint("tools considered: %s", strings.Join(toolNames(plan.Tools), ", "))
		p.Hint("see `ivo --tools` for the tool list, or `ivo --tools <name>` for one tool")
	case resolve.VerdictAsk:
		p.Warn("not going to guess: %s", d.Reason)
		// A command that was resolved but gated is still worth showing: it is
		// the tool's best reading of the phrase, and seeing it is how the user
		// decides whether to rephrase. It is labelled so it can never be
		// mistaken for something that ran.
		if len(d.Argv) > 0 && d.Tool != nil {
			p.Title("suggestion (not executed)")
			p.Command(ui.ShellQuote(d.Argv))
			p.Field("command", d.Tool.Name)
			printNotes(p, d)
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
	p.Field("guardrails", fmt.Sprintf("injection %.2f | destructive %.2f | clarity %.2f | severity %.2f",
		d.Guardrails["guardrail.injection"].Noul,
		d.Guardrails["guardrail.destructive_request"].Noul,
		d.Guardrails["guardrail.tool_is_clear"].Noul,
		d.Severity))
	p.Hint("nothing ran.")

	if d.Verdict == resolve.VerdictBlocked {
		return exitBlocked
	}
	return exitUnresolved
}

// printNotes shows every decision that shaped the command line. Being able to
// read why the command looks the way it does is the point of the tool.
func printNotes(p *ui.Printer, d *resolve.Decision) {
	for _, note := range d.Notes {
		p.Field(note.Param, note.Detail)
	}
}

// renderTools prints the catalog. With no argument it lists every tool; with a
// tool name it prints that tool's parameters and the manual page to read next.
func renderTools(p *ui.Printer, e *env.Env, query string) {
	query = strings.TrimSpace(query)
	if query != "" {
		if !e.HasCommand(query) {
			p.Warn("no installed program named %q", query)
			return
		}
		docs := discover.Inspect(query)
		if e.Docs == nil {
			e.Docs = map[string]discover.Docs{}
		}
		e.Docs[query] = docs
		tool := catalog.NamedProgram(discover.Program{Name: query, Summary: docs.Summary, ReadOnly: discover.IsReadOnly(query)})
		renderTool(p, e, tool)
		p.Field("documentation", fmt.Sprintf("%s, %d options, %d subcommands", docs.Source, len(docs.Options), len(docs.Subcommands)))
		return
	}
	p.Title(fmt.Sprintf("installed programs (%d)", len(e.Commands)))
	for _, name := range e.Commands {
		p.Line("  %s", name)
	}
	p.Hint("Programs are selected by purpose; naming one is optional.")
	p.Hint("`ivo --tools <name>` for the installed program's documentation")
}

func renderTool(p *ui.Printer, e *env.Env, tool catalog.Tool) {
	p.Title(tool.Name)
	p.Line("%s", tool.What)
	if tool.NotFor != "" {
		p.Line("not for: %s", tool.NotFor)
	}
	if len(tool.Examples) > 0 {
		p.Line("examples: %s", strings.Join(tool.Examples, " | "))
	}

	binding, ok := tool.Bind(e)
	if !ok {
		p.Warn("unavailable here: needs %s", strings.Join(tool.Needs, ", "))
		return
	}
	p.Field("parameters", "")
	for _, param := range binding.Params {
		kind := "flag"
		if param.Kind == catalog.ChoiceParam {
			kind = "choice"
		}
		detail := fmt.Sprintf("[%s] %s", kind, param.Desc)
		if param.Default != "" {
			detail += fmt.Sprintf(" (default %s)", param.Default)
		}
		if param.Gated {
			detail += " (asked only if the request mentions it)"
		}
		p.Field(param.Name, detail)
	}
	// The template, not a command line: the placeholders are shown as they are.
	if len(binding.Options) > 0 {
		source := fmt.Sprintf("%d documented by %s", len(binding.Options), binding.Discover)
		if docs, ok := e.Docs[binding.Discover]; ok {
			source += fmt.Sprintf(", read from %s (%d bytes)", docs.Source, docs.Bytes)
		}
		p.Field("options", source)
		for _, option := range binding.Options {
			p.Line("    %-22s %s", strings.Join(option.Flags, ", "), firstSentence(option.Desc))
		}
	}
	p.Field("binds to", strings.Join(binding.Argv, " "))
	for _, program := range catalog.Programs(binding, e) {
		p.Field("manual", fmt.Sprintf("man %s   (or %s --help)", program, program))
	}
}

func paramNames(params []catalog.Param) string {
	names := make([]string, 0, len(params))
	for _, param := range params {
		names = append(names, param.Name)
	}
	return strings.Join(names, ", ")
}

func firstSentence(text string) string {
	if i := strings.Index(text, ". "); i >= 0 {
		return text[:i+1]
	}
	return text
}

func toolNames(tools []catalog.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// ---------------------------------------------------------------------------
// arguments
// ---------------------------------------------------------------------------

type cliOptions struct {
	dryRun  bool
	yolo    bool
	tools   bool
	help    bool
	version bool
}

func parseArgs(args []string) (cliOptions, string, error) {
	opts := cliOptions{}
	var phrase []string
	afterSeparator := false

	for _, arg := range args {
		switch {
		case afterSeparator:
			phrase = append(phrase, arg)
		case arg == "--":
			afterSeparator = true
		case arg == "-n" || arg == "--dry-run":
			opts.dryRun = true
		case arg == "-x" || arg == "--execute":
			// Executing is the default. The flag stays for scripts that want
			// to say so out loud, and it turns an earlier --dry-run back off.
			opts.dryRun = false
		case arg == "-h" || arg == "--help":
			opts.help = true
		case arg == "-V" || arg == "--version":
			opts.version = true
		case arg == "--tools":
			opts.tools = true
		case arg == "--yolo":
			opts.yolo = true
		case strings.HasPrefix(arg, "-") && arg != "-":
			return opts, "", fmt.Errorf("unknown option: %s", arg)
		default:
			// Words without quotes are joined back into one phrase, so
			// `ivo list the files` works as well as `ivo "list the files"`.
			phrase = append(phrase, arg)
		}
	}
	return opts, strings.Join(phrase, " "), nil
}

var helpText = `ivo: turn a phrase in natural language into a shell command.

USAGE
  ivo [options] "phrase in natural language"

The command is never written by a language model. ivo answers typed questions
about the tools this machine has, and this program assembles the argv from the
answers. It asks before running anything, unless --yolo says otherwise, and
--dry-run shows the command without running it.

EXAMPLES
  ivo "list all files in this directory"
  ivo --dry-run "how much space does this directory take"
  ivo "search for TODO in the go files"
  ivo "show the contents of README.md"

OPTIONS
  -n, --dry-run            show the resolved command and stop, running nothing
  -x, --execute            run the resolved command (already the default)
  --yolo                   run without asking for confirmation
  --tools [NAME]           list installed programs, or inspect one program's
                           parameters and documentation
  -h, --help               this help
  -V, --version            version

ENVIRONMENT
  TYPESAFE_API_KEY         TypeSafe API key (required)
  IVO_BASE_URL             API host, default ` + typesafe.DefaultBaseURL + `
  NO_COLOR                 disable colours

EXIT CODES
  1 error, 2 unresolved (ambiguous or unsupported by discovered programs)
  3 blocked by a guardrail, 4 no API key
  the exit code of the executed command is propagated
`
