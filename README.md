# jev-cli

A tool-calling layer for the shell. Give it a phrase in natural language, get a
tool call with typed arguments, bound to a command line.

```console
$ jev --dry-run "list all files in this directory"
$ ls .
  command               list_directory, confidence 1.00
  severity              0.08
  target_path           . (confidence 1.00)
  hidden                omitted: not mentioned (p=0.17)
  details               omitted: not mentioned (p=0.05)
dry-run: nothing ran. Run again without --dry-run to execute.

$ jev "list all files in this directory"
cmd
go.mod
internal
README.md
scripts
```

The call is never written by a language model. It answers typed questions about
a fixed set of tools and their parameters, and code binds the filled call to an
argv. [TypeSafe's jev](https://docs.typesafe.ai) supplies the answers.

## Why it works this way

Asking an LLM to "return the shell command" has two problems: the command can be
anything, and the generated text has to be parsed back into something runnable.
Both disappear by construction.

- **The model fills parameters, it does not write commands.** Every token on the
  command line comes from a parameter value this repository authored, or from a
  path code confirmed exists. There is no path from an answer to an arbitrary
  command: an answer is always a key from a set we defined.
- **Nothing goes through a shell.** The command is a `[]string` run with
  `exec.CommandContext`. There is no `sh -c`, so quotes, `;`, `|`, `$()` and
  backticks cannot become a second command.
- **Code decides whether to call.** Calibrated confidence and probabilities
  choose between calling, asking and refusing. A call runs only if the tool
  choice was confident and the guardrails passed; `--dry-run` shows it without
  running anything.
- **The vocabulary is closed.** A tool that is not in the catalog is never
  invented. The honest answer is "I cannot do that", with the closest candidates
  and their probabilities.

## Install

```console
$ go install github.com/harlleyoliveira/jev-cli/cmd/jev@latest
```

No external dependencies, standard library only. Make sure `$HOME/go/bin` is on
your PATH.

The API key comes from `TYPESAFE_API_KEY`. Reading it from the macOS Keychain
keeps it out of your dotfiles and your shell history:

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
| (default) | run the resolved call |
| `-n`, `--dry-run` | show the resolved call and the parameter decisions, then stop |
| `-x`, `--execute` | run it (already the default; cancels an earlier `--dry-run`) |
| `--allow-write` | permit a tool that is not read-only, and a call the side-effect question did not clear |
| `--tools [NAME]` | list the tools, or show one tool's parameters and its manual page |
| `-h`, `--help` | usage |
| `-V`, `--version` | version |

Environment: `TYPESAFE_API_KEY` (required), `JEV_BASE_URL` (API host, useful for
pointing the CLI at a fake server), `NO_COLOR`.

The exit code of the run command is propagated. jev-cli's own codes are `1`
error, `2` unresolved (ambiguous or outside the catalog), `3` blocked by a
guardrail, `4` no API key.

## How it works

```
phrase
  |
  +- CODE   probe the environment
  |           cwd, directory listing (counted and filtered here), binaries on
  |           PATH, what each program documents about itself, and the
  |           candidates lifted out of the phrase: paths that exist, globs,
  |           words worth searching for
  |
  +- REQUEST 1   which tool, its operand values, and the guardrails
  |
  +- REQUEST N   the walk, repeated until the call is enough:
  |                does the call built so far satisfy the request?
  |                if not, which option should be added?
  |
  +- CODE   bind the filled call to argv, apply the gates
  |
  +- CODE   run it, or stop and show it with --dry-run
```

The walk is the shape of the navigation. A tool is chosen first, and only then
are *its* options listed, so the option question never competes with the flags
of tools that lost:

```
list all files, including hidden ones
  ls                        does ls satisfy it?      no
  ls -a                     does ls -a satisfy it?   yes
  -> ls -a .

list everything in detail, including hidden files
  ls                        does ls satisfy it?      no
  ls -a                     does ls -a satisfy it?   no
  ls -a -l                  does ls -a -l satisfy it? yes
  -> ls -a -l .
```

Both questions of a round ride in the same request, so a round costs one round
trip whatever the answer turns out to be, and the walk is bounded at four
rounds. The last round verifies rather than adds: without it a call that needs
two options ends on an option nobody checked, which is how `ls -a -A -l` came
back for a request that wanted two of the three.

### The layer

A tool is a name, a description and a list of typed parameters:

```go
Tool{
    Name: "list_directory",
    What: "List what is inside a directory ...",
    Bind: func(e *env.Env) (Binding, bool) {
        return Binding{
            Argv: []string{"ls", "{hidden}", "{details}", "{target}"},
            Params: []Param{
                targetParam(),                                  // closed set, required
                {Kind: FlagParam, Name: "hidden",  Argv: []string{"-a"}, Gated: true, ...},
                {Kind: FlagParam, Name: "details", Argv: []string{"-l"}, Gated: true, ...},
            },
        }, true
    },
}
```

The model fills the parameters and code binds them, so the question set is
derived from the parameters rather than hand written per command. The shell is
an implementation detail of `Argv`: nothing the model sees is phrased in terms
of flags. `catalog.Result` carries both the typed call (`Call{Name, Args}`) and
its binding (`Argv`).

### Four patterns from the TypeSafe documentation

1. **Function calling.** A closed parameter becomes a Choice whose keys are
   exactly the accepted values, so nothing has to map a label back to an
   argument: the answer is the value.
2. **The `stated` gate.** Before a gated parameter is used, a Noul asks whether
   the request *said anything* about it. If not, the declared default stands.
   This is what keeps a question answered in silence from becoming a decision:
   against the real model, "list all files in this directory" answers the
   hidden-files flag at **p=0.52**, just over the 0.5 line, and the call used to
   come out as `ls -a .`. The same phrase puts the gate at p=0.17.
3. **Pre-parsed value extraction.** Paths, globs and search terms are open
   strings, and the model cannot produce them. Code over-finds with regex and
   `stat`, and the model only picks among the candidates. Everything comes back
   verbatim.
4. **Confidence-gated routing and guardrails.** The gates use the numbers from
   the documentation (0.5 for an unclear tool choice, 0.35 to review, 0.70 to
   act on a hazard, 2.0 for severity). The guardrail questions ride along in the
   same request, so they cost no extra latency.

Counting and arithmetic stay in code: the model does not count reliably, so
`entry_count` and the listing caps are computed here.

## Where the tools come from

Two places, and only the first of them is written down:

- **Shaped tools.** Five, written here, because their operands have a meaning
  this layer has to know: `list_directory` takes a path, `search_text` takes a
  term and a path, and so on. Their flags are still not written down: those come
  from the programs they bind to.
- **The commands this machine can run.** The shell is asked for its command list
  (2093 of them here, in 0.1 seconds), and the request is read word by word to
  find which words name a program:

```
open the current project in zed
  open     is this a program?  no
  current  is this a program?  no
  project  is this a program?  no
  zed      is this a program?  yes   -> zed, which this machine has
```

One question per word, all in one request, so the filter costs one round trip.
A word the model calls a program still has to *exist* here before it becomes a
tool, so the model can find a command but never invent one. Its description and
its options then come from its own `--help` or manual, read once and cached.

Reading the list natively is what makes this affordable: listing 2093 names
takes a tenth of a second, while asking the system to describe just eight of
them took seven.

### What is allowed to run

`ls`, `rg`, `cat` and a few dozen more are classified read-only in code, and
they run. Everything else, `zed` and `rm` alike, has to pass one more question
asked about the *call that was built*, not about the request:

```
"open the current project in zed"   ->  zed .       changes nothing  ->  runs
"remove the build directory with rm" ->  rm -rf build   changes data  ->  refused, use --allow-write
```

Judging the call rather than the sentence is what makes a tool space that is not
written down usable: opening an editor in a project *reads* like a change, while
`zed .` does not. `--allow-write` overrides the judgment when you mean it.

The shaped tools, in detail:

| Tool | What it does |
| --- | --- |
| `list_directory` | list a directory, with optional hidden entries and per-entry detail |
| `search_text` | search inside file contents with `rg` or `grep`, with an optional file name filter |
| `show_file` | print a file, all of it or one end of it |
| `report_working_directory` | print the current directory |
| `read_manual` | print the manual page of one of the programs above |

Every other command on the machine is reachable by naming it.

A shaped tool means adding one function in
[`internal/catalog/entries.go`](internal/catalog/entries.go): a name, the
description the model chooses by, and the operands with their binding. Nothing
else needs writing down: a command nobody shaped is still reachable, with its
description and its flags read from the program itself.

| Tool | Options come from | Read from | Cost |
| --- | --- | --- | --- |
| `list_directory` | `ls` | `man ls` (BSD `ls` has no `--help`) | 46 options, 26 kB |
| `search_text` | `rg` | `rg --help` | 104 options, 74 kB |
| `search_text` (fallback) | `grep` | `man grep` | 64 options, 15 kB |

`--help` is consulted first, and it is not only cheaper: `man rg` is 108 kB and
parses to *zero* options here, so for ripgrep the help output is the only source
that works at all. The manual is the fallback for programs that only have one,
which on macOS includes every BSD tool. The choice, the source and its size are
cached under `~/Library/Caches/jev/docs`, and `jev --tools <name>` shows them.

### Navigating the catalog

The closed vocabulary is documented, so you do not have to guess at it:

```console
$ jev --tools
tools (closed vocabulary)
    list_directory             List what is inside a directory: ...
      parameters: target, hidden, details
    search_text                Search for a piece of text inside the contents of files.
      parameters: terms, target, ignore_case, files
    show_file                  Print the contents of a file, all of it or just one end of it.
      parameters: how_much, target
    report_working_directory   Print the absolute path of the directory the command is running in.
      parameters:
    read_manual                Print the manual page of one of the programs this catalog uses.
      parameters: program

$ jev --tools search_text
search_text
  Search for a piece of text inside the contents of files. ...
  parameters
  terms                 [choice] the literal text to look for
  target                [choice] the path the call acts on
  ignore_case           [flag] matching that ignores letter case (asked only if the request mentions it)
  files                 [choice] a file name filter for the search (default any) (asked only if the request mentions it)
  binds to              rg {ignore_case} {files} -e {terms} {target}
  manual                man rg   (or rg --help)
```

`--tools` needs no key and no network: it is the layer describing itself, and it
ends with the `man` or `--help` command to read the underlying program. When a
phrase needs the documentation itself, `read_manual` turns that into a call, for
example `man -P cat rg`.

Two details there are the binding's job rather than the model's. The pager is
overridden, because `man` on a terminal would open one and wait for input, which
is exactly the interactive trap a tool call must not fall into. And the output
is adapted, because `man` formats for a terminal: it writes bold as "X backspace
X", so the binding declares `OutputStripOverstrike` and the page arrives as
plain text. The catalog says what the output needs, `run` knows how to do it.

What this deliberately does *not* do is derive the parameters from `man`
automatically. `ls` alone has around fifty flags, and enumerating them would
blow up the question battery, which is already about 3,000 tokens for five
tools, and would cost accuracy for options nobody asked about. The useful half
of that idea is to pull the *descriptions* for the curated parameter list from
`--help`, with a checked-in cache, so the wording stops being hand written.

## Evals

The deterministic tests cannot tell you whether the model answers well, only
what happens once it has answered. The eval is the other half: it runs the real
model over a fixed set of phrases and compares each decision against what a
correct call looks like.

```console
$ go run ./cmd/jev-eval            # one run over evals/cases.json
$ go run ./cmd/jev-eval -n 3       # three runs, to see agreement
```

Cases live in [`evals/cases.json`](evals/cases.json) and assert a verdict, a
tool, an exact argv or required/forbidden tokens. Each case records *why* it
exists. An eval never executes a command: it judges the decision, never the
effect.

The current run, against `jev-1.13.0`:

```
16/16 runs passed, agreement 16/16 cases, 11.3s, 75741 tokens, jev-1.13.0
```

The tokens buy up to six requests per phrase: one for the word filter, one to
choose the tool, up to four rounds of the walk, and one more when the tool is
not read-only.

The eval earned its place immediately. Its first run failed two cases, and both
were real:

| Case | Was | Why it failed | Now |
| --- | --- | --- | --- |
| `list the files with details` | `ls .` | the gate asked about "permissions, size, owner or date", so "with details" did not match it | `ls -l .` |
| `grep for the word timeout` | `ask` | the clarity guardrail demanded a named target, but an unstated parameter takes its default | `rg -e timeout .` |

Neither was a threshold problem. Both questions were phrased in terms of shell
flags rather than in terms of a tool's parameters, which is the whole point of
having the layer. A second round of failures, after the options started coming
from the manual, is what forced the staged walk: asking about every option of
every tool in one request put eighty questions in flight and the needed flags
came back below the line.

## Tests

```console
$ go test ./...
$ go vet ./...
$ ./scripts/test-sheet.sh          # mapping check by eye, dry run only
```

What the deterministic suite covers, and what it cannot:

- the HTTP client against a real server: retries on 429 and 529, refusals on 401
  and 422, and a loud failure when an answer is missing;
- the environment probe against a real filesystem and real processes;
- the filler against synthetic answers: gated flags, defaults, the escape hatch,
  and an answer outside the closed set failing loudly;
- the verdicts: act, ask, unsupported, blocked, and that nothing but `act` ever
  carries a runnable command;
- an end-to-end run of the binary against a fake TypeSafe server that really
  executes the resolved `ls`;
- the invariants: the request body carries exactly `state`, `model` and
  `questions` with only noul, choice and score questions; every token in the
  argv was authored in the catalog, checked across thousands of answer
  combinations; and no tool can reach a shell.

What none of that covers is whether the real model picks the right tool and the
right parameter values. That is what the eval is for.

## Known limits

- **No command chaining.** No pipes: one call, one program. A phrase that asks
  for two things lands on `ask` or `unsupported`.
- **Nothing is written without being asked twice.** A program that is not in the
  read-only table runs only when the call-level judgment clears it, or when
  `--allow-write` says so. The judgment is a model answer with a threshold, so it
  is a gate and not a proof.
- **A closed vocabulary is closed.** What is not in the catalog does not happen,
  and the honest output is `unsupported`.
- **255 values per Choice.** The directory listing is capped at 120 entries. A
  tool whose candidates do not exist, or would not fit, is not offered in that
  invocation instead of becoming a 422.
- **The state is not hostile to the model.** Free text, including file names,
  can influence answers. Hence the filtered and bounded listing, the injection
  guardrail, and a program allowlist as the last line of defence.

## License

MIT.
