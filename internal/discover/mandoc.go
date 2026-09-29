package discover

import (
	"fmt"
	"html"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

// A manual page is written in roff, and mandoc renders it. Which device it
// renders to is the choice this file makes: the text device is a picture of the
// page, and the reader has to guess from the columns which words are flags,
// while the HTML device says so in the markup.
//
// Both styles of page that mandoc writes are read here:
//
//	mdoc:      <dt><code class="Fl">-l</code></dt>
//	           <dd>List files in the long format.</dd>
//	generated: <p class="Pp">-n &lt;number&gt;</p>
//	           <div class="Bd-indent">Limit the number of commits.</div>
//
// The first names the flags, the second does not, and the second is what git,
// rg and gh ship. So a generated page keeps the angle brackets it writes around
// a placeholder and goes through the same reader as the text device, which
// already knows how to read them.
//
// The text device stays as the fallback, for a machine without mandoc.

// manRenderer is the mandoc binary, or "" when this machine has none. The
// lookup is memoized because the answer cannot change while the process runs,
// and it is a variable so the fallback path can be tested.
var manRenderer = sync.OnceValue(func() string {
	path, err := exec.LookPath("mandoc")
	if err != nil {
		return ""
	}
	return path
})

// loadManual reads one manual page, through mandoc when the machine has it and
// through the text device otherwise. The two agree on what an option is.
func loadManual(page string) Docs {
	if docs, ok := readManHTML(page); ok {
		return docs
	}
	manual := Docs{Program: page, Source: "man"}
	if text, err := output("man", "-P", "cat", page); err == nil {
		manual.Options = ParseMan(text)
		manual.Bytes = len(text)
		manual.Summary = summaryOf(text)
		manual.Detail = descriptionOf(text)
	}
	return manual
}

// readManHTML renders the page with mandoc and reads its options out of the
// markup. The boolean is false when this machine cannot do it, which is the
// whole fallback case.
func readManHTML(page string) (Docs, bool) {
	renderer := manRenderer()
	if renderer == "" {
		return Docs{}, false
	}
	path, err := manPagePath(page)
	if err != nil {
		return Docs{}, false
	}
	src, err := output(renderer, "-T", "html", path)
	if err != nil {
		return Docs{}, false
	}
	options := ParseManHTML(src)
	if len(options) == 0 {
		return Docs{}, false
	}
	return Docs{
		Program: page,
		Source:  "man",
		Bytes:   len(src),
		Summary: summaryOf(plainTextOf(src)),
		Detail:  descriptionOf(plainTextOf(src)),
		Options: options,
	}, true
}

// manPagePath asks man where a page lives. man is the program that knows the
// man path, the sections, the suffixes and the compression, and searching for
// the file again here would be a worse copy of that search.
func manPagePath(page string) (string, error) {
	out, err := output("man", "-w", page)
	if err != nil {
		return "", err
	}
	for _, path := range strings.Split(strings.TrimSpace(out), ":") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("man -w %s names no page this machine has", page)
}

// ParseManHTML reads the options out of a manual page rendered to HTML.
//
// The markup states what the text device leaves to a guess: <code class="Fl">
// is a flag and <var class="Ar"> is the placeholder for its value. A generated
// page names neither, so its spec line is read as text, which is what the text
// device does with it too.
func ParseManHTML(src string) []Option {
	var (
		options []Option
		region  string // "spec" or "desc", empty between entries
		open    int    // tags open inside the region
		argOpen int    // open Ar elements, written back as the < and > of a placeholder
		pending string // the spec that is waiting for the description after it
		buf     strings.Builder
	)

	flush := func(kind string) {
		text := flatten(buf.String())
		buf.Reset()
		if text == "" {
			return
		}
		// Only a line that starts with a dash is an option: a tag list in the
		// ENVIRONMENT section is a definition list too.
		if kind == "spec" {
			if strings.HasPrefix(text, "-") {
				pending = text
			}
			return
		}
		if pending == "" {
			return
		}
		flags, arg := parseSpec(pending)
		if len(flags) == 0 {
			return
		}
		options = append(options, Option{Flags: flags, Arg: arg, Desc: text})
		pending = ""
	}

	for _, part := range scanParts(src) {
		if part.Name == "" {
			if region != "" {
				buf.WriteString(part.Text)
			}
			continue
		}

		if region == "" {
			// A description only ever follows a spec, which is what keeps a
			// paragraph that happens to be indented from being read as one.
			// The opening tag counts, so that its own closing tag ends the
			// entry: <dt>...</dt> and <p class="Pp">...</p>.
			switch {
			case !part.End && part.Name == "dt":
				region, open, argOpen = "spec", 1, 0
			case !part.End && part.Name == "dd" && pending != "":
				region, open, argOpen = "desc", 1, 0
			case !part.End && part.Name == "p" && hasClass(part, "Pp"):
				region, open, argOpen = "spec", 1, 0
			case !part.End && part.Name == "div" && hasClass(part, "Bd-indent") && pending != "":
				region, open, argOpen = "desc", 1, 0
			}
			continue
		}

		// Inside an entry, nesting decides where it ends: the tag that opened
		// the region is the one at depth one.
		if part.Void {
			if part.Name == "br" {
				buf.WriteString("\n")
			}
			continue
		}
		if !part.End {
			if part.Name == "var" && hasClass(part, "Ar") && region == "spec" {
				buf.WriteString("<")
				argOpen++
			}
			open++
			continue
		}
		if part.Name == "var" && argOpen > 0 {
			buf.WriteString(">")
			argOpen--
		}
		open--
		if open == 0 {
			flush(region)
			region = ""
		}
	}
	// Some manuals document required arguments only in SYNOPSIS.
	if start := strings.Index(src, `id="SYNOPSIS"`); start >= 0 {
		synopsis := src[start:]
		if end := strings.Index(synopsis, "</section>"); end >= 0 {
			synopsis = synopsis[:end]
		}
		for _, pair := range synopsisArguments.FindAllStringSubmatch(synopsis, -1) {
			flag, arg := html.UnescapeString(pair[1]), cleanArg(html.UnescapeString(pair[2]))
			for i := range options {
				if options[i].Arg != "" {
					continue
				}
				for _, spelling := range options[i].Flags {
					if spelling == flag {
						options[i].Arg = arg
					}
				}
			}
		}
	}
	return options
}

// plainTextOf renders a page back to lines of text, which is the shape the NAME
// section reader and the tool summary already read.
func plainTextOf(src string) string {
	var out strings.Builder
	for _, part := range scanParts(src) {
		if part.Name == "" {
			// Whitespace inside a line is only a separator, in the page and
			// here: a line break is what a block tag makes.
			if fields := strings.Fields(html.UnescapeString(part.Text)); len(fields) > 0 {
				out.WriteString(strings.Join(fields, " "))
				out.WriteString(" ")
			}
			continue
		}
		if blockTags[part.Name] {
			out.WriteString("\n")
		}
	}
	return out.String()
}

// flatten turns the text of one entry into a single line: entities decoded, runs
// of whitespace collapsed.
func flatten(text string) string {
	return strings.Join(strings.Fields(html.UnescapeString(text)), " ")
}

// htmlPart is one tag or one run of text from a rendered page.
type htmlPart struct {
	Text  string // a run of text, when Name is empty
	Name  string // the tag name, in lower case
	Class string // the class attribute, when it has one
	End   bool   // a closing tag
	Void  bool   // a tag with no closing tag, which cannot open a region
}

// voidTags are the tags HTML does not close, so they must not count towards
// nesting: mandoc writes br and col in manual pages.
var voidTags = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"source": true, "track": true, "wbr": true,
}

// blockTags end a line of text, which is what turns a rendered page back into
// something a reader of the NAME section can read.
var blockTags = map[string]bool{
	"br": true, "dd": true, "div": true, "dl": true, "dt": true, "h1": true,
	"h2": true, "li": true, "p": true, "pre": true, "section": true,
	"table": true, "td": true, "tr": true,
}

// scanParts splits rendered markup into tags and text. It is not an HTML
// parser: the input is generated by one program, and what is read here is one
// class attribute and the nesting, so a general parser would be a dependency
// and a lie about how much of HTML this has to survive.
func scanParts(src string) []htmlPart {
	var parts []htmlPart
	for i := 0; i < len(src); {
		if src[i] != '<' {
			j := strings.IndexByte(src[i:], '<')
			if j < 0 {
				j = len(src) - i
			}
			parts = append(parts, htmlPart{Text: src[i : i+j]})
			i += j
			continue
		}
		switch {
		case strings.HasPrefix(src[i:], "<!--"):
			j := strings.Index(src[i:], "-->")
			if j < 0 {
				return parts
			}
			i += j + 3
			continue
		case strings.HasPrefix(src[i:], "<!"), strings.HasPrefix(src[i:], "<?"):
			j := strings.IndexByte(src[i:], '>')
			if j < 0 {
				return parts
			}
			i += j + 1
			continue
		}
		j := strings.IndexByte(src[i:], '>')
		if j < 0 {
			return parts
		}
		body := src[i+1 : i+j]
		i += j + 1

		part := htmlPart{}
		if strings.HasPrefix(body, "/") {
			part.End = true
			body = body[1:]
		}
		body = strings.TrimSpace(body)
		if strings.HasSuffix(body, "/") {
			part.Void = true
			body = strings.TrimSpace(strings.TrimSuffix(body, "/"))
		}
		part.Name = strings.ToLower(nameOf(body))
		if part.Name == "" {
			continue
		}
		part.Class = classOf(body)
		part.Void = part.Void || voidTags[part.Name]
		parts = append(parts, part)
	}
	return parts
}

// nameOf is the tag name, which is everything before the first attribute.
func nameOf(body string) string {
	if i := strings.IndexAny(body, " \t\r\n"); i >= 0 {
		return body[:i]
	}
	return body
}

// classOf reads the class attribute. It is the only attribute that is read: it
// is how mandoc says what a piece of text is.
func classOf(body string) string {
	lowered := strings.ToLower(body)
	for i := 0; i < len(lowered); i++ {
		if !strings.HasPrefix(lowered[i:], "class") {
			continue
		}
		rest := strings.TrimLeft(body[i+len("class"):], " \t\r\n")
		if !strings.HasPrefix(rest, "=") {
			continue
		}
		rest = strings.TrimLeft(rest[1:], " \t\r\n")
		if rest == "" {
			return ""
		}
		if quote := rest[0]; quote == '"' || quote == '\'' {
			rest = rest[1:]
			if end := strings.IndexByte(rest, quote); end >= 0 {
				return rest[:end]
			}
			return ""
		}
		if end := strings.IndexAny(rest, " \t\r\n>"); end >= 0 {
			return rest[:end]
		}
		return rest
	}
	return ""
}

// hasClass reports whether one of an element's classes is the given one.
func hasClass(part htmlPart, class string) bool {
	for _, have := range strings.Fields(part.Class) {
		if have == class {
			return true
		}
	}
	return false
}

var synopsisArguments = regexp.MustCompile(`<code class="Fl">([^<]+)</code>\s*<var class="Ar">([^<]+)</var>`)
