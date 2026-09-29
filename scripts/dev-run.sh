#!/bin/sh
# Run the working copy of ivo, rebuilding it when the source changed.
#
# A binary on PATH is a snapshot. Every fix made after the last install keeps
# not happening, and a stale binary cannot be told apart from a fresh one: it
# answers with the same version string, so a bug that is already fixed looks
# unfixed and you go looking in the wrong place. It happened here, which is why
# this script exists.
#
# It rebuilds only when something changed. A no-op go build costs about half a
# second, which is too much to pay on every invocation of a command meant to be
# typed, but a compare of modification times costs nothing.
#
# The caller's directory is never changed: ivo reads the directory it runs in,
# so a runner that cd'd into the repository would answer about the repository.
#
#	./scripts/dev-run.sh "list all files in this directory"
#	./scripts/dev-run.sh -V
#
# Point a shell function at it to make `ivo` itself always run the tree:
#
#	ivo() { "$HOME/Projects/ivo/scripts/dev-run.sh" "$@"; }
set -eu

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

command -v go >/dev/null 2>&1 || {
	echo "ivo: go is not on PATH" >&2
	exit 1
}

# Where go installs. This is asked rather than assumed, because go env is the
# only thing that knows about a GOPATH set with go env -w, which is not
# necessarily in the environment, and a wrong guess here would run the wrong
# binary instead of failing.
gobin=$(go env GOBIN)
[ -n "$gobin" ] || gobin="$(go env GOPATH)/bin"
bin="$gobin/ivo"

# Anything newer than the binary means the binary is not this source. The
# search covers go.mod too: a dependency or a language bump changes the build.
stale=""
if [ ! -x "$bin" ]; then
	stale="missing"
elif [ -n "$(find "$repo" -name '*.go' -newer "$bin" -print -quit 2>/dev/null)" ]; then
	stale="source is newer"
elif [ "$repo/go.mod" -nt "$bin" ]; then
	stale="go.mod is newer"
fi

if [ -n "$stale" ]; then
	identity=$(git -C "$repo" rev-parse --short HEAD 2>/dev/null || echo unknown)
	# Untracked work counts as edits: a new file is part of what runs here.
	if [ -n "$(git -C "$repo" status --porcelain 2>/dev/null | head -1)" ]; then
		identity="$identity+edits"
	fi
	built=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	echo "ivo: rebuilding from $identity ($stale)" >&2
	(cd "$repo" && go install \
		-ldflags "-X main.commit=$identity -X main.builtAt=$built" \
		./cmd/ivo) || exit $?
fi

exec "$bin" "$@"
