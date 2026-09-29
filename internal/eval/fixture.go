package eval

import (
	"os"
	"os/exec"
	"path/filepath"
)

// fixture is the directory listing every eval case is evaluated against. A
// fixed fixture is what makes two runs comparable: the model's answer depends
// on the state it is given, and the state includes the listing.
var fixture = []struct {
	name  string
	isDir bool
}{
	{"src", true},
	{"internal", true},
	{"main.go", false},
	{"README.md", false},
	{"go.mod", false},
}

// MakeFixture writes that listing into a fresh temporary directory and returns
// its path. The caller is expected to change into it before probing, so that a
// phrase naming a file has a real file to resolve to.
func MakeFixture() (dir string, cleanup func(), err error) {
	// The directory name is fixed on purpose. It travels in the state the model
	// is asked about, so a random name makes two invocations incomparable in a
	// way that is invisible: the same phrase resolved one way in one run and
	// another way in the next, and the difference was a temporary path.
	dir = filepath.Join(os.TempDir(), "ivo-eval-fixture")
	if err := os.RemoveAll(dir); err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	cleanup = func() { os.RemoveAll(dir) }

	for _, entry := range fixture {
		path := filepath.Join(dir, entry.name)
		if entry.isDir {
			if err := os.Mkdir(path, 0o755); err != nil {
				cleanup()
				return "", nil, err
			}
			continue
		}
		if err := os.WriteFile(path, []byte("eval fixture\n"), 0o600); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	// A repository, because git is part of the vocabulary and "list the last
	// commits" has nothing to list in a bare directory.
	initRepository(dir)

	return dir, cleanup, nil
}

// initRepository makes the fixture a repository with one commit. It is best
// effort: a machine without git still gets a usable fixture.
func initRepository(dir string) {
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=ivo-eval", "GIT_AUTHOR_EMAIL=eval@localhost",
			"GIT_COMMITTER_NAME=ivo-eval", "GIT_COMMITTER_EMAIL=eval@localhost")
		_ = cmd.Run()
	}
	run("init", "-q")
	run("add", "-A")
	run("commit", "-qm", "the fixture commit", "--allow-empty")
}
