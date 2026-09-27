# jev-cli

Turn a phrase in natural language into a shell command, without letting a
language model write the command.

```console
$ jev --dry-run "list all files in this directory"
$ ls .
  command               list_directory, confidence 1.00
  severity              0.08
  target_path           . (confidence 1.00)
  ls_hidden             omitted: not mentioned (p=0.17)
  ls_long               omitted: not mentioned (p=0.05)
  ls_sort_time          omitted: not mentioned (p=0.04)
  ls_sort_size          omitted: not mentioned (p=0.03)
  ls_recursive          omitted: not mentioned (p=0.15)
dry-run: nothing ran. Run again without --dry-run to execute.

$ jev "list all files in this directory"
cmd
go.mod
internal
```

jev is a System One model from [TypeSafe AI](https://docs.typesafe.ai). It does
not generate text. It answers typed questions (Choice, Score, Noul) about a
closed catalog of commands, and this program assembles the argv from the
answers.

## Why it works this way

A CLI that asks an LLM to "return the shell command" has two problems: the
command can be anything, and the generated text has to be parsed back into
something runnable. Both disappear here by construction.

- **Nothing is generated.** Every string that reaches the command line was
  either authored in this repository (the programs, the flags) or confirmed to
  exist by code (paths, patterns, search terms). There is no path from the
  model's answer to an arbitrary command: an answer is always a key from a set
  we defined.
- **Nothing goes through a shell.** The command is a `[]string` run with
  `exec.CommandContext`. There is no `sh -c`, so quotes, `;`, `|`, `$()` and
  backticks cannot become a second command.
- **Code decides whether to run.** Calibrated confidence and probabilities
  choose between running, asking and refusing. A command runs only if the
  choice was confident, the guardrails were satisfied, and the catalog entry is
  read-only. Anything else stops and explains itself, and `--dry-run` shows the
  command without running it.
- **The vocabulary is closed.** A command that is not in the catalog is never
  invented. The honest answer is "I cannot do that", with the closest
  candidates and their probabilities.

## Install

```console
$ go install github.com/harlleyoliveira/jev-cli/cmd/jev@latest
```

No external dependencies, standard library only. Make sure `$HOME/go/bin` is on
your PATH.

The API key comes from `TYPESAFE_API_KEY`, from the config file, or from
`--api-key-file`. It is never a normal flag, because arguments are visible in
the process list.

```console
$ export TYPESAFE_API_KEY=...
$ jev "where am I"
```

Reading it from the macOS Keychain keeps it out of your dotfiles and your shell
history:

```sh
security add-generic-password -a "$USER" -s TYPESAFE_API_KEY -w   # prompts for the secret
export TYPESAFE_API_KEY="$(security find-generic-password -a "$USER" -s TYPESAFE_API_KEY -w)"
```

## Usage

```
jev [options] "phrase in natural language"
```

| Option | Effect |
| --- | --- |
| (default) | run the resolved command |
| `-n`, `--dry-run` | show the resolved command and the decisions, then stop |
| `-x`, `--execute` | run the resolved command (already the default) |
| `--allow-write` | allow commands that are not read-only |
| `-j`, `--json` | JSON result; the executed command's output is captured |
| `--path VALUE` | use this path, without asking the model |
| `--pattern VALUE` | use this file name pattern |
| `--term VALUE` | use this search text |
| `-v`, `--explain` | show the request, the raw response, notes and token usage |
| `--commands` | list the closed catalog and what is available here |
| `-m`, `--model` | model (default `jev-latest`) |
| `--base-url` | API host |
| `--max-entries` | maximum directory entries in the state |
| `--min-confidence` | minimum confidence to act (default `0.5`) |
| `--timeout` | API call timeout, in seconds |
| `--no-color` | no colours |

The exit code of the executed command is propagated. jev-cli's own codes are
`1` error, `2` unresolved (ambiguous or outside the catalog), `3` blocked by a
guardrail, and `4` no API key.

## How it works

```
phrase
  |
  +- CODE   probe the environment (env.Probe)
  |           cwd, directory listing (counted and filtered here),
  |           git repository, binaries on PATH, and the candidates
  |           lifted out of the phrase: paths that exist, globs, terms
  |
  +- ONE REQUEST   POST /v1/systemone
  |           state     = phrase + filtered environment
  |           questions =
  |              intent                        Choice over the catalog
  |              target_path                   Choice over real paths
  |              name_pattern / search_terms / flags   (speculative)
  |              <slot>?                       Noul "did the user mention this?"
  |              guardrail.injection           Noul
  |              guardrail.destructive_request Noul
  |              guardrail.intent_clear        Noul
  |              guardrail.severity            Score
  |
  +- CODE   read only the winning command's answers (catalog.Assemble)
  |           apply the gates, build the argv
  |
  +- CODE   run it, or stop and show it with --dry-run
```

Every question goes in a single request. The model evaluates them in parallel,
so a speculative question about a command that lost costs tokens, not latency.
This is the speculative fan-out pattern from the TypeSafe documentation.

### Five patterns from the docs

1. **Function calling.** Each closed-set argument becomes a Choice whose keys
   are exactly the accepted values. Nothing has to map a label back to an
   argument: the answer is the token.
2. **`stated` (`<slot>?`).** Before using an optional argument, flag or value, a
   Noul asks whether the user *said anything* about it. If not, the declared
   default stands. This is what keeps a question answered in silence from
   becoming a decision: against the real model, "list all files in this
   directory" answers the hidden-files flag at **p=0.52**, just over the 0.5
   line, and the command used to come out as `ls -a .`. The same phrase puts the
   gate at p=0.18, so the flag stays off. When the request does mention hidden
   files, the gate rises to 0.93 and `-a` appears.
3. **Pre-parsed value extraction.** Paths, patterns and search terms are open
   strings, and the model cannot produce them. Code over-finds with regex and
   `stat`, and the model only *selects* among the candidates. Everything comes
   back verbatim.
4. **Confidence-gated routing and guardrails.** The gates use the numbers from
   the documentation (0.5 for an ambiguous intent, 0.35 to review, 0.70 to act
   on a hazard, 2.0 for severity) and are configurable. The guardrail questions
   run in the same request, so they cost no extra latency.
5. **Speculative fan-out.** All slot questions for every available command are
   asked up front, and code reads only the ones belonging to the winner.

### Counting and arithmetic stay in code

The model does not count reliably and Score levels calibrate poorly to
magnitudes, so `entry_count`, search depth and ordering are computed here and
never asked of the model.

## The catalog

Everything this CLI can run, and nothing else:

| Command | What it does |
| --- | --- |
| `list_directory` | list what is inside a directory |
| `find_files` | find files by name, with a bounded depth |
| `search_text` | search inside file contents (`rg`, or `grep`), with an optional file name filter |
| `show_file` | print a file, all of it or one end |
| `count_lines` | count lines |
| `disk_usage` | size of a path |
| `file_info` | what kind of file something is |
| `report_working_directory` | print the current directory |
| `git_status`, `git_log`, `git_diff` | repository state, history and diff |

All of them are **read-only**. `--commands` lists the catalog and marks what is
unavailable in the current environment (for example the `git_` commands outside
a repository).

Adding a command means adding an entry in
[`internal/catalog/entries.go`](internal/catalog/entries.go): an argv template
with placeholders, the slots that decide each placeholder, and the contrastive
descriptions (what it is, what it is not for) that keep neighbouring commands
apart.

## Calibration against the real model

The thresholds above are starting points. These are the numbers the real jev
returned for this project, and what they changed:

| Phrase | Before | After |
| --- | --- | --- |
| `list all files in this directory` | `ls -a .` (flag at 0.52, a false positive) | `ls .` (gate at 0.18) |
| `list everything including hidden files` | `ls -a .` | `ls -a .` (p=0.98, correct) |
| `search for TODO in the go files` | `rg -e TODO .` (the file filter was ignored) | `rg -g '*.go' -e TODO .` |
| `what changed` | `ask` with nothing useful to show | `ask` plus a `git status` suggestion plus candidates |

The confidence gates remain the most likely place to need tuning. `what changed`
stops at `ask` because the model splits between `git_status` and `git_diff`
(0.40), which is real ambiguity rather than an error. If you would rather it
acted there, lower `min_confidence` in the config; the command is still
read-only.

## Known limits

- **No command chaining.** No pipes: one argv, one program. A phrase that asks
  for two things lands on `ask` or `unsupported`. An internal pipeline is the
  natural next step.
- **Nothing is written.** Requests to change something are refused with that
  explanation, rather than answered with something adjacent. Writing would need
  a real risk taxonomy; the flags exist (`ReadOnly`, `--allow-write`), but no
  entry uses them yet.
- **A closed vocabulary is closed.** What is not in the catalog does not happen.
  The honest output is `unsupported`, with the most likely candidates and their
  probabilities.
- **255 options per Choice.** The directory listing is bounded by
  `max_entries`. A command whose candidates do not exist, or would not fit,
  simply is not offered in that invocation, instead of becoming a 422.
- **The state is not hostile to the model.** Free text, including file names,
  can influence answers. Hence the filtered and bounded listing, the injection
  guardrail, and a program allowlist as the last line of defence.

## Configuration

`~/.config/jev/config.json` (or `$XDG_CONFIG_HOME/jev/config.json`, or
`$JEV_CONFIG`). All keys are optional:

```json
{
  "api_key": "",
  "model": "jev-latest",
  "base_url": "https://api.typesafe.ai",
  "max_entries": 120,
  "min_confidence": 0.5,
  "clarity_threshold": 0.35,
  "destructive_threshold": 0.35,
  "injection_threshold": 0.7,
  "severity_threshold": 2.0,
  "timeout_seconds": 30,
  "no_color": false,
  "allow_write": false
}
```

Environment variables: `TYPESAFE_API_KEY`, `JEV_MODEL`, `JEV_BASE_URL`,
`JEV_CONFIG`, `NO_COLOR`. If the file holds an API key and other users can read
it, the CLI says so.

## Development

```console
$ go test ./...
$ go vet ./...
$ ./scripts/test-sheet.sh          # a human-readable mapping check, dry run only
```

The suite covers the HTTP client (including retries on 429 and 529 and the
refusal to retry 401 and 422), argv assembly (exclusive flag groups, flag
dependencies, defaults, forced literals), candidate extraction, the guardrail
verdicts, and an end-to-end test that runs the binary against a fake TypeSafe
server and actually executes the resolved `ls`.

It also asserts the invariants the design rests on: the request body carries
exactly `state`, `model` and `questions` with only noul, choice and score
questions; every token in the argv was authored in the catalog, checked across
more than a thousand answer combinations; and no command can reach a shell.

## License

MIT.
