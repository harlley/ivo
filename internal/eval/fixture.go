package eval

import (
	"os"
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
	dir, err = os.MkdirTemp("", "jev-eval-*")
	if err != nil {
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
	return dir, cleanup, nil
}
