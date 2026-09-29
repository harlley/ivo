# ivo

Use the programs installed on your computer through natural language.

```sh
ivo "list the files in this directory"
ivo --dry-run "how much memory is available?"
```

ivo discovers installed programs, reads their local documentation, and uses a
model to select the program and arguments. It shows the command and asks for
confirmation before running it.

## Install

Requires Go 1.26.3 or later and a TypeSafe API key.

```sh
go install github.com/harlley/ivo/cmd/ivo@latest
export PATH="$HOME/go/bin:$PATH"
export TYPESAFE_API_KEY="your-api-key"
```

## Usage

```sh
ivo [options] "request"
```

| Option | Effect |
| --- | --- |
| `-n`, `--dry-run` | Show the command without running it |
| `-x`, `--execute` | Execute after confirmation; cancel an earlier dry-run |
| `--yolo` | Run without confirmation |
| `--tools [NAME]` | List installed programs or inspect one program's documentation |
| `-h`, `--help` | Show help |
| `-V`, `--version` | Show version |

Press Enter to confirm or `n` to cancel. `--tools` works without an API key.
Set `NO_COLOR` to disable colors or `IVO_BASE_URL` to override the API host.

Requests, directory context, command candidates, and selected documentation are
sent to TypeSafe. The executed program's output is printed directly.

## How it works

The model selects from installed executables, documented options, and argument
candidates extracted from the request. Code assembles and validates the command,
then executes it directly without a shell.

The current adapter uses TypeSafe's Jev (`jev-latest`). Other providers can
implement [model.Adapter](internal/model/model.go) with the same typed question
and answer contract.

## Limits

- One program per request; no pipelines or workflows with multiple steps.
- One positional operand; commands requiring more may remain unresolved.
- Missing documentation or uncertain selections can prevent resolution.
- Programs can modify files or data. Review the command before confirming.

## Development

```sh
go test ./...
go vet ./...
./scripts/dev-run.sh --dry-run "list the files"
```

The development runner rebuilds the binary when source files change and runs it
in the caller's directory.

Run evaluations against the real model with an API key. These resolve commands
without executing them; `-n 3` repeats each case three times.

```sh
go run ./cmd/ivo-eval -n 3 -cases evals/cases.json
go run ./cmd/ivo-eval -n 3 -cases evals/discovery.json
go run ./cmd/ivo-eval -n 3 -cases evals/held-out.json
```

## License

[MIT](LICENSE).
