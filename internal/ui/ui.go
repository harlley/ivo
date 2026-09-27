// Package ui renders jev-cli's own output. The executed command's output is
// never touched: it goes straight to the terminal, so pipes and pagers work.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// ANSI codes, used only when colour is on.
const (
	reset  = "\033[0m"
	bold   = "\033[1m"
	dim    = "\033[2m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
)

// Printer writes jev-cli's own messages. Diagnostics go to Err so that stdout
// stays clean for --json and for the executed command.
type Printer struct {
	Out   io.Writer
	Err   io.Writer
	Color bool
}

// New returns a Printer over the given streams.
func New(color bool, out, err io.Writer) *Printer {
	return &Printer{Out: out, Err: err, Color: color}
}

func (p *Printer) paint(code, s string) string {
	if !p.Color {
		return s
	}
	return code + s + reset
}

// Command prints the resolved command line, ready to copy into a shell.
func (p *Printer) Command(text string) {
	fmt.Fprintf(p.Out, "%s %s\n", p.paint(dim, "$"), p.paint(bold+green, text))
}

// Title prints a small heading.
func (p *Printer) Title(text string) {
	fmt.Fprintln(p.Out, p.paint(bold, text))
}

// Line prints an indented line of detail.
func (p *Printer) Line(format string, args ...any) {
	fmt.Fprintf(p.Out, "  %s\n", fmt.Sprintf(format, args...))
}

// Field prints an aligned "label  value" pair.
func (p *Printer) Field(label, value string) {
	fmt.Fprintf(p.Out, "  %s  %s\n", p.paint(dim, fmt.Sprintf("%-20s", label)), value)
}

// Success prints a positive verdict.
func (p *Printer) Success(format string, args ...any) {
	fmt.Fprintln(p.Err, p.paint(green, fmt.Sprintf(format, args...)))
}

// Warn prints something the user should notice.
func (p *Printer) Warn(format string, args ...any) {
	fmt.Fprintln(p.Err, p.paint(yellow, fmt.Sprintf(format, args...)))
}

// Error prints a failure.
func (p *Printer) Error(format string, args ...any) {
	fmt.Fprintln(p.Err, p.paint(red, fmt.Sprintf(format, args...)))
}

// Hint prints a dim suggestion.
func (p *Printer) Hint(format string, args ...any) {
	fmt.Fprintln(p.Err, p.paint(dim, fmt.Sprintf(format, args...)))
}

// Say prints a plain message to stdout.
func (p *Printer) Say(format string, args ...any) {
	fmt.Fprintf(p.Out, "%s\n", fmt.Sprintf(format, args...))
}

// IsTerminal reports whether f is attached to a terminal, so colour is only
// used where it belongs.
func IsTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// ColorSupported reports whether colour should be used for this stream. A
// buffer, a test, a pipe, a file, never gets ANSI codes.
func ColorSupported(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && IsTerminal(f)
}

// ShellQuote renders an argv slice the way a shell would need to see it, for
// display only. It is never used to execute anything.
func ShellQuote(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, tok := range argv {
		if tok == "" {
			parts = append(parts, "''")
			continue
		}
		if !strings.ContainsAny(tok, " \t\n\"'\\$`&|;<>()*?[]{}!~#") {
			parts = append(parts, tok)
			continue
		}
		parts = append(parts, "'"+strings.ReplaceAll(tok, "'", `'\''`)+"'")
	}
	return strings.Join(parts, " ")
}
