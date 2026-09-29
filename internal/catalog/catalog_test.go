package catalog_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/harlley/ivo/internal/catalog"
	"github.com/harlley/ivo/internal/discover"
	"github.com/harlley/ivo/internal/env"
	"github.com/harlley/ivo/internal/model"
)

func testEnv() *env.Env {
	return &env.Env{
		Request: "search for TODO in go files", Commands: []string{"ls", "rg"},
		Candidates: env.Candidates{Paths: []string{"src"}, Terms: []string{"TODO"}, Patterns: []string{"*.go"}},
		Docs: map[string]discover.Docs{
			"ls": {Program: "ls", Options: []discover.Option{{Flags: []string{"-a"}, Desc: "Include hidden files."}, {Flags: []string{"-l"}, Desc: "List in long format."}}},
			"rg": {Program: "rg", Options: []discover.Option{{Flags: []string{"-i"}, Desc: "Ignore case."}, {Flags: []string{"-e"}, Arg: "PATTERN", Desc: "Search for this pattern."}, {Flags: []string{"-g"}, Arg: "GLOB", Desc: "Include matching files."}}},
		},
	}
}

func choice(value string) model.Answer {
	return model.Answer{Type: model.KindChoice, Choice: value, Confidence: 1}
}
func operand(value string) map[string]model.Answer {
	return map[string]model.Answer{"operand": choice(value), "operand?": {Type: model.KindNoul, Noul: 1}}
}
func program(name string) catalog.Tool {
	return catalog.NamedProgram(discover.Program{Name: name, ReadOnly: discover.IsReadOnly(name)})
}

func TestEveryInstalledProgramUsesTheSameBinding(t *testing.T) {
	e := testEnv()
	for _, name := range []string{"ls", "rg", "vm_stat", "custom-unfamiliar-program"} {
		tool := program(name)
		binding, ok := tool.Bind(e)
		if !ok {
			t.Fatal(name)
		}
		if got := strings.Join(binding.Argv, " "); got != name+" {flags} {target}" {
			t.Fatal(got)
		}
		if binding.Discover != name {
			t.Fatalf("wrong documentation: %+v", binding)
		}
		if err := catalog.ToolQuestion([]catalog.Tool{tool}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFlagsComeFromTheChosenProgramsDocumentation(t *testing.T) {
	e := testEnv()
	tool := program("ls")
	binding, _ := tool.Bind(e)
	result, err := catalog.Fill(tool, binding, operand("."), []string{"-a", "-l"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(result.Argv, " "); got != "ls -a -l ." {
		t.Fatal(got)
	}
	if _, err := catalog.Fill(tool, binding, operand("."), []string{"-i"}, e); err == nil {
		t.Fatal("accepted a flag belonging to a different program")
	}
	if _, err := catalog.Fill(tool, binding, operand("."), []string{"--invented"}, e); err == nil {
		t.Fatal("accepted an invented flag")
	}
}

func TestGenericTextFlagsAndOperandPreserveSearchArguments(t *testing.T) {
	e := testEnv()
	tool := program("rg")
	binding, _ := tool.Bind(e)
	result, err := catalog.Fill(tool, binding, operand("src"), []string{"-g *.go", "-e TODO"}, e)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"rg", "-g", "*.go", "-e", "TODO", "src"}
	if strings.Join(result.Argv, "|") != strings.Join(want, "|") {
		t.Fatalf("argv=%q", result.Argv)
	}
}

func TestOperandsAreLiteralCandidatesAndMayBeOmitted(t *testing.T) {
	e := testEnv()
	tool := program("rg")
	binding, _ := tool.Bind(e)
	for _, value := range []string{"src", "TODO"} {
		result, err := catalog.Fill(tool, binding, operand(value), nil, e)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Argv) != 2 || result.Argv[1] != value {
			t.Fatal(result.Argv)
		}
	}
	result, err := catalog.Fill(tool, binding, map[string]model.Answer{"operand?": {Type: model.KindNoul, Noul: 0}}, nil, e)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Argv) != 1 {
		t.Fatal(result.Argv)
	}
	if _, err := catalog.Fill(tool, binding, operand("invented text"), nil, e); err == nil {
		t.Fatal("invented operand was accepted")
	}
}

func TestMetacharactersStayInsideOneArgument(t *testing.T) {
	e := testEnv()
	literal := "hello; $(touch /tmp/never)"
	e.Candidates.Terms = []string{literal}
	tool := program("rg")
	binding, _ := tool.Bind(e)
	result, err := catalog.Fill(tool, binding, operand(literal), nil, e)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Argv) != 2 || result.Argv[1] != literal {
		t.Fatalf("literal was split: %q", result.Argv)
	}
	if got := catalog.Allowlist(map[string]catalog.Binding{"rg": binding}, e); len(got) != 1 || got[0] != "rg" {
		t.Fatal(got)
	}
}

func TestSubcommandsBringTheirOwnOptions(t *testing.T) {
	e := testEnv()
	e.Request = "last 3 commits"
	e.Docs["git"] = discover.Docs{Program: "git", Subcommands: []discover.Subcommand{{Name: "log", Desc: "Show commit history."}}}
	e.Docs["git log"] = discover.Docs{Program: "git log", Options: []discover.Option{{Flags: []string{"-n"}, Arg: "number", Desc: "Limit the number of commits."}, {Flags: []string{"--oneline"}, Desc: "One line per commit."}}}
	tool := program("git")
	binding, _ := tool.Bind(e)
	result, err := catalog.Fill(tool, binding, map[string]model.Answer{"operand?": {Type: model.KindNoul}}, []string{"log", "-n 3", "--oneline"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(result.Argv, " "); got != "git log -n 3 --oneline" {
		t.Fatal(got)
	}
}

func TestOptionQuestionsStayWithinChoiceLimit(t *testing.T) {
	e := testEnv()
	for _, name := range e.Commands {
		options := catalog.AllDocumentedOptions(e, name)
		q := catalog.OptionQuestion(options, nil, 0)
		if err := q.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestVerificationQuestionsCarryTheActualCandidate(t *testing.T) {
	for _, q := range []model.Question{catalog.SatisfiedQuestion("ls -a ."), catalog.VerifyQuestion("ls -a .")} {
		raw, err := json.Marshal(q)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "ls -a .") {
			t.Fatal(string(raw))
		}
	}
}

func TestNumericFlagsCannotReceiveTextCandidates(t *testing.T) {
	e := testEnv()
	e.Request = "show 3 records"
	e.Docs["records"] = discover.Docs{Program: "records", Options: []discover.Option{{Flags: []string{"-n"}, Arg: "count", Desc: "Number of records."}}}
	options := catalog.AllDocumentedOptions(e, "records")
	if len(options) != 1 || options[0].Key() != "-n 3" {
		t.Fatalf("numeric candidates = %+v", options)
	}
}

func TestArchiveValuesArePathsRatherThanActionWords(t *testing.T) {
	e := testEnv()
	e.Request = "use tar to pack main.go into out.tar"
	e.Candidates = env.Candidates{Paths: []string{"main.go"}, Terms: []string{"tar", "pack", "main.go", "into", "out.tar"}}
	e.Docs["tar"] = discover.Docs{Options: []discover.Option{{Flags: []string{"-c"}}, {Flags: []string{"-f"}, Arg: "file", Desc: "Write the archive to file."}}}
	options := catalog.AllDocumentedOptions(e, "tar")
	found := false
	for _, option := range options {
		if option.Key() == "-f out.tar" {
			found = true
		}
		if option.Key() == "-f tar" || option.Key() == "-f pack" || option.Key() == "-f into" {
			t.Fatalf("action word bound as archive: %+v", option)
		}
	}
	if !found {
		t.Fatal("missing output archive candidate")
	}
}

func TestNonRepeatableOptionsRejectConflictingValuesAndAliases(t *testing.T) {
	e := testEnv()
	e.Request = "show 2 or 3 records"
	e.Docs["records"] = discover.Docs{Options: []discover.Option{{Flags: []string{"-n", "--count"}, Arg: "count", Desc: "Limit the number of records."}}}
	tool := program("records")
	binding, _ := tool.Bind(e)
	if _, err := catalog.Fill(tool, binding, nil, []string{"-n 2", "-n 3"}, e); err == nil {
		t.Fatal("accepted two competing counts")
	}
	option := discover.Option{Flags: []string{"-n", "--count"}, Argv: []string{"-n", "3"}}
	if !option.ConflictsWith([]string{"--count 2"}) {
		t.Fatal("alternate spelling escaped duplicate check")
	}
	option = discover.Option{Flags: []string{"-w"}, Argv: []string{"-w"}, Desc: "May be specified more than once."}
	if option.ConflictsWith([]string{"-w"}) {
		t.Fatal("documented repetition was blocked")
	}
}

func TestOptionDescriptionsKeepOutputDependencies(t *testing.T) {
	option := discover.Option{Flags: []string{"--whole"}, Argv: []string{"--whole"}, Desc: "Include the whole diff. This modifies patch output only; it does not enable patches."}
	question := catalog.OptionQuestion([]discover.Option{option}, nil, 0).(model.ChoiceQuestion)
	description := question.Criteria["--whole"].(string)
	if !strings.Contains(description, "does not enable patches") {
		t.Fatalf("lost output dependency: %q", description)
	}
}

func TestDocumentedPredicatesCanFollowThePositionalOperand(t *testing.T) {
	e := testEnv()
	e.Request = "find files with extension .md"
	e.Candidates = env.Candidates{Patterns: []string{"*.md"}, Terms: []string{"extension"}}
	e.Docs["find"] = discover.Docs{Options: []discover.Option{{Flags: []string{"-name"}, Arg: "pattern", AfterOperand: true, Desc: "Match the file name against a pattern."}}}
	tool := program("find")
	binding, _ := tool.Bind(e)
	answers := operand(".")
	result, err := catalog.Fill(tool, binding, answers, []string{"-name *.md"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(result.Argv, "|"); got != "find|.|-name|*.md" {
		t.Fatalf("invalid predicate order: %q", result.Argv)
	}
}
