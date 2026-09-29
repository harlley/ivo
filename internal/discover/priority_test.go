package discover

import (
	"testing"
)

// TestPriorityPutsTheSeedFirstAndTheUsedAfter covers what the list is for: the
// commands a person reaches for come first, and the commands they actually ran
// come next, without anyone writing them down.
func TestPriorityPutsTheSeedFirstAndTheUsedAfter(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("IVO_LEARNED", "")

	Priority() // reads the seed, which is what a first run has
	Promote("frobnicate")
	Promote("frobnicate")
	Promote("once-only")

	list := Priority()
	if len(list) == 0 || list[0] != seedCommands[0] {
		t.Fatalf("the seed is not first: %v", list[:min(3, len(list))])
	}
	position := map[string]int{}
	for i, name := range list {
		position[name] = i
	}
	if _, ok := position["frobnicate"]; !ok {
		t.Fatal("a command that was used twice is not on the list")
	}
	if _, ok := position["once-only"]; !ok {
		t.Fatal("a command that was used once is not on the list")
	}
	if position["frobnicate"] > position["once-only"] {
		t.Error("the more used command has to come first")
	}
	if len(list) > maxPriority {
		t.Errorf("the priority list holds %d names, want at most %d", len(list), maxPriority)
	}
	if position[seedCommands[0]] > position["frobnicate"] {
		t.Error("the seed has to come first")
	}
}

// TestPromoteIsOffForAReproducibleRun is the eval's switch: a list that grows
// with use would make two runs incomparable and hide tuning behind state.
func TestPromoteIsOffForAReproducibleRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("IVO_LEARNED", "0")

	if Learned() {
		t.Fatal("learned = true, want the seed only")
	}
	Promote("frobnicate")
	for _, name := range Priority() {
		if name == "frobnicate" {
			t.Fatal("a promotion landed while learning was off")
		}
	}
}

// TestPromoteKeepsTheListFromBecomingTheMachine: the learned half is a list of
// what a person uses, and it stops being one if it is allowed to grow into a
// copy of the command list.
func TestPromoteKeepsTheListFromBecomingTheMachine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("IVO_LEARNED", "")

	for i := 0; i < maxLearned+25; i++ {
		Promote(string(rune('a'+i%26)) + string(rune('a'+i/26)) + "cmd")
	}
	state, err := readPriority()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Used) > maxLearned {
		t.Errorf("the learned list holds %d commands, want at most %d", len(state.Used), maxLearned)
	}
}
