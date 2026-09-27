// Command jev turns a phrase in natural language into a shell command.
//
//	jev "list all files in this directory"
//
// The command is never written by a language model. jev asks a System One
// model (TypeSafe's jev) a set of typed questions about a fixed catalog of
// commands and a fixed set of options, and this program assembles the argv
// from the answers.
//
// The command runs once the gates pass: the model was confident, the guardrails
// were satisfied, and the catalog entry is read-only. Pass --dry-run to see the
// resolved command without running it.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/harlleyoliveira/jev-cli/internal/catalog"
	"github.com/harlleyoliveira/jev-cli/internal/env"
	"github.com/harlleyoliveira/jev-cli/internal/resolve"
	"github.com/harlleyoliveira/jev-cli/internal/run"
	"github.com/harlleyoliveira/jev-cli/internal/typesafe"
	"github.com/harlleyoliveira/jev-cli/internal/ui"
)

// version is the released version, also settable at build time with
// -ldflags "-X main.version=...".
var version = "0.1.0"

const (
	// maxEntries caps how many directory entries go into the state.
	maxEntries = 120
	// apiTimeout bounds one call to the API.
	apiTimeout = 30 * time.Second
)

// Exit codes. 1-4 are jev-cli's own; a non-zero exit from an executed command
// is propagated unchanged.
const (
	exitOK         = 0
	exitError      = 1
	exitUnresolved = 2 // nothing was resolved: ambiguous or outside the catalog
	exitBlocked    = 3 // a guardrail stopped it
	exitNoKey      = 4 // no usable API key
)

func main() { os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr)) }

// realMain takes its streams explicitly so the whole pipeline can be tested end
// to end, including argument parsing and output.
func realMain(args []string, stdout, stderr io.Writer) int {
	opts, phrase, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "jev: %v\n", err)
		fmt.Fprintln(stderr, "see `jev --help`")
		return exitUnresolved
	}
	if opts.help {
		fmt.Fprint(stdout, helpText)
		return exitOK
	}
	if opts.version {
		fmt.Fprintf(stdout, "jev %s\n", version)
		return exitOK
	}

	color := os.Getenv("NO_COLOR") == "" && ui.ColorSupported(stdout)
	printer := ui.New(color, stdout, stderr)

	systemEnv, err := env.Probe(env.ProbeOptions{
		Request:          phrase,
		MaxEntries:       maxEntries,
		Binaries:         catalog.Binaries(),
		Document:         catalog.Documented(catalog.All()),
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
		printer.Error("missing the phrase, for example: jev \"list all files in this directory\"")
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
	evaluation, err := plan.Evaluate(ctx, client)
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

	outcome, err := run.Do(ctx, decision.Argv, run.Options{
		Execute: true,
		Allow:   catalog.Allowlist(plan.Bindings, systemEnv),
		Filter:  outputFilter(decision.Output),
		Stdin:   os.Stdin,
		Stdout:  stdout,
		Stderr:  stderr,
	})
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
	if v := strings.TrimSpace(os.Getenv("JEV_BASE_URL")); v != "" {
		return v
	}
	return typesafe.DefaultBaseURL
}

// renderDryRun prints what would run and stops.
func renderDryRun(p *ui.Printer, d *resolve.Decision) {
	p.Command(ui.ShellQuote(d.Argv))
	p.Field("command", fmt.Sprintf("%s, confidence %.2f", d.Tool.Name, d.Intent.Confidence))
	p.Field("severity", fmt.Sprintf("%.2f", d.Severity))
	printNotes(p, d)
	p.Hint("dry-run: nothing ran. Run again without --dry-run to execute.")
}

// renderNoCommand explains why nothing will run.
func renderNoCommand(p *ui.Printer, d *resolve.Decision, plan *resolve.Plan) int {
	switch d.Verdict {
	case resolve.VerdictBlocked:
		p.Error("blocked: %s", d.Reason)
	case resolve.VerdictUnsupported:
		p.Warn("I cannot do that: %s", d.Reason)
		p.Hint("this CLI only runs: %s", strings.Join(toolNames(plan.Tools), ", "))
		p.Hint("see `jev --tools` for the tool list, or `jev --tools <name>` for one tool")
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
		d.Guardrails["guardrail.intent_clear"].Noul,
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
		for _, tool := range catalog.All() {
			if tool.Name == query {
				renderTool(p, e, tool)
				return
			}
		}
		p.Warn("no tool named %q", query)
	}

	p.Title("tools (closed vocabulary)")
	for _, tool := range catalog.All() {
		// Availability here is about the machine, not about the phrase: this
		// listing describes the catalog, so a tool whose candidate values
		// happen to be empty for the current request is still a tool.
		mark := " "
		if !installed(tool, e) {
			mark = "."
		}
		p.Line("%s %-26s %s", mark, tool.Name, firstSentence(tool.What))
		if binding, ok := tool.Bind(e); ok {
			p.Line("    parameters: %s", paramNames(binding.Params))
		} else {
			p.Line("    unavailable here: needs %s", strings.Join(tool.Needs, ", "))
		}
	}
	p.Hint("")
	p.Hint("`jev --tools <name>` for one tool's parameters and its manual page")
	p.Hint(". = unavailable here; everything listed is read-only")
}

// installed reports whether a tool's programs are on this machine. It is a
// weaker question than "usable for this phrase", and it is the right one for
// documentation.
func installed(tool catalog.Tool, e *env.Env) bool {
	for _, need := range tool.Needs {
		if !e.Has(need) {
			return false
		}
	}
	_, ok := tool.Bind(e)
	return ok
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
	dryRun     bool
	allowWrite bool
	tools      bool
	help       bool
	version    bool
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
		case arg == "--allow-write":
			opts.allowWrite = true
		case strings.HasPrefix(arg, "-") && arg != "-":
			return opts, "", fmt.Errorf("unknown option: %s", arg)
		default:
			// Words without quotes are joined back into one phrase, so
			// `jev list the files` works as well as `jev "list the files"`.
			phrase = append(phrase, arg)
		}
	}
	return opts, strings.Join(phrase, " "), nil
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
  jev "search for TODO in the go files"
  jev "show the contents of README.md"

OPTIONS
  -n, --dry-run            show the resolved command and stop, running nothing
  -x, --execute            run the resolved command (already the default)
  --allow-write            permit a tool that is not read-only, and a call the
                           side effect question did not clear
  --tools [NAME]           list the tools, or show one tool's parameters and
                           the manual page to read next
  -h, --help               this help
  -V, --version            version

ENVIRONMENT
  TYPESAFE_API_KEY         TypeSafe API key (required)
  JEV_BASE_URL             API host, default ` + typesafe.DefaultBaseURL + `
  NO_COLOR                 disable colours

EXIT CODES
  1 error, 2 unresolved (ambiguous or outside the catalog)
  3 blocked by a guardrail, 4 no API key
  the exit code of the executed command is propagated
`
