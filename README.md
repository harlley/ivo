# ivo

Intention into commands.

A natural-language interface to the programs installed on your computer.
There are no predefined tools: listing files, reading text, inspecting memory
and using an unfamiliar CLI all go through the same discovery and binding code.

```console
$ ivo --dry-run "how much memory this computer has available?"
$ ivo "list the files in this directory"
$ ivo --tools
$ ivo --tools vm_stat
```

The model chooses among real executable names, documented options, and literal
arguments extracted from the request. It never generates a shell command.

## Install

```console
$ go install github.com/harlley/ivo/cmd/ivo@latest
```

Standard library only. Make sure `$HOME/go/bin` is on PATH and set
`TYPESAFE_API_KEY`. For example, using an existing macOS Keychain entry:

```sh
export TYPESAFE_API_KEY="$(security find-generic-password -a "$USER" -s TYPESAFE_API_KEY -w)"
```

## Usage

```
ivo [options] "phrase in natural language"
```

| Option | Effect |
| --- | --- |
| default | show the resolved command and ask before running |
| `-n`, `--dry-run` | resolve and display without executing the command |
| `-x`, `--execute` | execute after confirmation; cancels an earlier dry-run |
| `--yolo` | execute without confirmation |
| `--tools [NAME]` | list installed executables, or inspect one program's documentation |
| `-h`, `--help` | usage |
| `-V`, `--version` | version and build identity, when available |

Enter confirms; `n`, an unrecognized answer, or closed input declines. A script
must explicitly use `--yolo` to execute without a person answering.

Environment: `TYPESAFE_API_KEY`, `IVO_BASE_URL` (for a compatible or test API),
and `NO_COLOR`. Resolution sends the request, bounded directory context,
installed command candidates and selected documentation to TypeSafe. `--tools`
needs no API key or network.

The child's exit code is propagated. The CLI's own exit codes are `1` for an
error, `2` for an unresolved request, `3` for a blocked request and `4` for a
missing or rejected API key.

## How discovery works

1. Code inventories executable programs available on this machine. Cached names
   are checked against the current PATH so removed commands are not offered.
2. The installed names are split into batches of 128. A typed choice for each
   batch asks which program best fits the purpose of the request. The batch
   questions and initial guardrails share one API request. A program need not
   appear anywhere in the user's wording.
3. Up to twelve nominated programs have their cached documentation or local
   manuals inspected. Candidate inspection does not launch a program with
   `--help`. The program's description helps distinguish similar commands.
4. A second choice selects the program. Operands are then resolved with that
   program and its documentation in context. The same generic binding is used for
   every executable: program, documented options, optional operand.
5. A bounded walk chooses subcommands and options. Numeric values, text values,
   patterns and operands come from request candidates. Each selection is checked
   against the offered set before becoming an argv token.
6. The assembled command is verified against the request and selected program's
   documentation. Missing documentation never counts as successful verification.
7. The command is shown for confirmation, then executed with `exec.CommandContext`.

The option walk can widen its offered list once when a narrow list misses a
needed option. Values remain bounded by the API's 255 choices per question.
Every choice has a `none_of_these` option; unsupported requests stay unresolved.

Descriptions and flags come from local documentation, not a handwritten catalog.
Documentation readers support `--help`, text manuals, and `mandoc` HTML, with a
cache under the operating system's cache directory. Automatic candidate
inspection uses cached documentation or manuals so nominating an executable
does not run it. Documentation can be incomplete, particularly for programs
without a manual or a previously cached help page.

## Model adapters

The resolution pipeline depends on `internal/model.Adapter`, whose `Ask` method
accepts typed questions and returns normalized answers, usage, and latency.
The shared contract lives in `internal/model`; discovery, argument binding,
verification, and execution do not depend on TypeSafe.

`internal/typesafe` is the first adapter, using Jev (`jev-latest`). It owns the
API endpoint, authentication, default model, HTTP transport, and retries.
The CLI and evaluation runner construct this adapter and pass it to the resolver.
To add another provider, implement `model.Adapter`, translate the shared question
and answer types into that provider's protocol, and wire it in at those entry
points. A replacement must support choices, probabilities, and rubric scores;
free-text command generation is not the adapter contract.

## Execution and limits

- The CLI runs one program per request. Pipelines, multi-step workflows and
  interpreting command output into a natural-language answer are not implemented.
- The generic binding currently has one positional operand. Commands needing
  multiple positional operands or values absent from the request can remain
  unresolved. Removing predefined tools does not guarantee every CLI grammar
  can already be represented.
- The first discovery pass uses command names and any descriptions already in
  the environment. Unknown names, missing documentation and model uncertainty
  can still prevent resolution.
- A bounded option walk and bounded candidate sets limit latency and token use.
  Explicit arguments can be missed; an unverified command is not executed.
- Execution uses an argv, without `sh -c`. Shell metacharacters in a literal
  argument do not create another command. This alone does not make an invocation
  safe: programs can have side effects, and confirmation remains the default.
- The existing read-only classification is used for caution labels and
  mismatches, not for deciding which programs can be discovered. Other programs
  receive a judgment about the assembled call's side effects.

## Development

```console
$ go test ./...
$ go vet ./...
$ ./scripts/dev-run.sh -V
$ ./scripts/dev-run.sh --dry-run "list the files"
```

The development runner rebuilds the installed binary when the source changes,
embeds the commit and build time, and preserves the caller's working directory.
To have `ivo` always use this checkout:

```sh
ivo() { "$HOME/Projects/ivo/scripts/dev-run.sh" "$@"; }
```

`./scripts/test-sheet.sh` checks the working copy with dry runs. The deterministic
suite covers discovery batches, invalid nominations, documented arguments,
verification, guardrails, confirmation and execution against a local fake API.

Real-model evaluations use the same discovery pipeline and never execute the
resolved command:

```console
$ go run ./cmd/ivo-eval -cases evals/cases.json
$ go run ./cmd/ivo-eval -cases evals/discovery.json
```

The discovery cases include system information outside the former file-oriented
catalog. `evals/held-out.json` records additional grammar and option-selection
cases. Case files use `argv_any` to accept complete equivalent invocations and reject
extra operands, missing option values, and wrong value bindings. Run with `-n 3`
to measure repeatability. The held-out patch case requires `-p`; `--full-diff`
alone does not show changes.

Results depend on the installed programs and the current model; tests
with synthetic answers are not evidence of real-model accuracy.

## License

MIT.
