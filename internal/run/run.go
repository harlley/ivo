// Package run executes a resolved command.
//
// The command is an argv slice, never a string handed to a shell, so quoting
// quirks and metacharacters cannot become a second command. On top of that,
// the program name must appear in an allowlist derived from the catalog.
package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// DefaultMaxOutputBytes caps captured output in --json mode.
const DefaultMaxOutputBytes = 256 << 10

// Options configures a single execution.
type Options struct {
	// Execute is false for a dry run: nothing is started.
	Execute bool
	// Allow is the list of program names that may appear in argv[0].
	Allow []string
	// Capture buffers stdout and stderr instead of inheriting them. Required
	// in --json mode, where the output has to travel inside the JSON document.
	Capture bool
	// MaxOutputBytes bounds the captured output.
	MaxOutputBytes int
	// Filter transforms the program's output before it is written. When it is
	// set the output is captured rather than streamed, because a filter cannot
	// see what it has not read.
	Filter func([]byte) []byte

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Outcome is the result of an execution attempt.
type Outcome struct {
	Ran       bool
	ExitCode  int
	Stdout    string
	Stderr    string
	Truncated bool
}

// Validate refuses anything that should never have been assembled.
func Validate(argv []string, allow []string) error {
	if len(argv) == 0 {
		return errors.New("run: empty command")
	}
	prog := argv[0]
	allowed := false
	for _, a := range allow {
		if a == prog {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("run: %q is not in the allowed binary list (%s)", prog, strings.Join(allow, ", "))
	}
	for _, tok := range argv {
		if strings.ContainsRune(tok, 0) {
			return errors.New("run: token contains a NUL byte")
		}
	}
	return nil
}

// Do runs the command when asked. A non-zero exit from the child is reported
// as an outcome, not as an error: ivo is a wrapper, and the child's status
// is its own.
func Do(ctx context.Context, argv []string, opts Options) (Outcome, error) {
	if err := Validate(argv, opts.Allow); err != nil {
		return Outcome{}, err
	}
	if !opts.Execute {
		return Outcome{Ran: false}, nil
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = opts.Stdin

	capture := opts.Capture || opts.Filter != nil

	var stdout, stderr *limitedBuffer
	if capture {
		limit := opts.MaxOutputBytes
		if limit <= 0 {
			limit = DefaultMaxOutputBytes
		}
		stdout, stderr = newLimitedBuffer(limit), newLimitedBuffer(limit)
		cmd.Stdout, cmd.Stderr = stdout, stderr
	} else {
		cmd.Stdout, cmd.Stderr = opts.Stdout, opts.Stderr
	}

	err := cmd.Run()

	out := Outcome{Ran: true}
	if opts.Filter != nil && stdout != nil {
		fmt.Fprint(opts.Stdout, string(opts.Filter([]byte(stdout.String()))))
		stdout.buf.Reset()
	}
	if stdout != nil {
		out.Stdout, out.Truncated = stdout.String(), stdout.truncated
	}
	if stderr != nil {
		out.Stderr = stderr.String()
		out.Truncated = out.Truncated || stderr.truncated
	}

	switch {
	case err == nil:
		return out, nil
	case ctx.Err() != nil:
		out.ExitCode = 130
		return out, nil
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			out.ExitCode = exitErr.ExitCode()
			if out.ExitCode < 0 {
				out.ExitCode = 128
			}
			return out, nil
		}
		return out, fmt.Errorf("run: %w", err)
	}
}

// StripOverstrike turns the "X backspace X" sequences a terminal formatter
// writes for bold and underline into plain text, and collapses the doubled
// characters of the simpler "XX" form. It is what makes a manual page readable
// when the reader is not a terminal.
func StripOverstrike(in []byte) []byte {
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		if in[i] != 0x08 {
			out = append(out, in[i])
			continue
		}
		// A backspace erases the character before it. Overstrike is the two
		// character form, "X backspace X", where the second character replaces
		// the first; a backspace at the end is a plain erase.
		if len(out) == 0 {
			continue
		}
		if i+1 < len(in) {
			out[len(out)-1] = in[i+1]
			i++
			continue
		}
		out = out[:len(out)-1]
	}
	return out
}

// limitedBuffer keeps the first N bytes and remembers that it dropped the rest.
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func newLimitedBuffer(limit int) *limitedBuffer {
	return &limitedBuffer{limit: limit}
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	remaining := l.limit - l.buf.Len()
	if remaining <= 0 {
		l.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		l.buf.Write(p[:remaining])
		l.truncated = true
		return len(p), nil
	}
	l.buf.Write(p)
	return len(p), nil
}

func (l *limitedBuffer) String() string { return l.buf.String() }
