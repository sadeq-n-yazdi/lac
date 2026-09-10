package skill_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"code.sadeq.uk/lac/internal/skill"
)

// The skill exists twice: the copy embedded in the binary, and the one at skills/lac/SKILL.md that
// people read on the repository page. Two copies drift, so this fails the moment they do — the fix
// is `make sync-skill`.
func TestTheEmbeddedSkillMatchesTheRepositoryCopy(t *testing.T) {
	published, err := os.ReadFile(filepath.Join("..", "..", "skills", "lac", "SKILL.md"))
	if err != nil {
		t.Fatalf("reading the published skill: %v", err)
	}

	if string(published) != skill.Content() {
		t.Error("skills/lac/SKILL.md and the embedded copy have drifted; run: make sync-skill")
	}
}

// A skill is only useful if the tool that reads it can tell when to trigger it, so the frontmatter
// has to be there and has to describe the situation rather than the software.
func TestTheSkillHasUsableFrontmatter(t *testing.T) {
	content := skill.Content()

	if !strings.HasPrefix(content, "---\n") {
		t.Fatal("the skill does not start with frontmatter")
	}

	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		t.Fatal("the frontmatter is not closed")
	}
	frontmatter := content[4 : end+4]

	if !strings.Contains(frontmatter, "name: lac") {
		t.Error("the frontmatter does not name the skill")
	}
	if !strings.Contains(frontmatter, "description:") {
		t.Error("the frontmatter has no description, so nothing will ever trigger it")
	}

	// The triggers that matter: the moments an agent is about to do the wrong thing.
	for _, trigger := range []string{"run the tests", "who else is working", "heavy"} {
		if !strings.Contains(frontmatter, trigger) {
			t.Errorf("the description does not cover %q, which is when this skill is needed", trigger)
		}
	}
}

// The skill has to teach the three things LAC does, or an agent reading it still will not know
// what to do.
func TestTheSkillCoversWhatMatters(t *testing.T) {
	content := skill.Content()

	for _, subject := range []string{
		"lac_acquire_slot",
		"lac_release_slot",
		"lac_agents",
		"lac_inbox",
		"lac run --resource",
		"even if it failed",
	} {
		if !strings.Contains(content, subject) {
			t.Errorf("the skill never mentions %q", subject)
		}
	}
}

func TestInstallWritesTheSkill(t *testing.T) {
	directory := t.TempDir()

	result, err := skill.Install(directory)
	if err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}

	want := filepath.Join(directory, "lac", "SKILL.md")
	if result.Path != want {
		t.Errorf("Path = %q, want %q", result.Path, want)
	}

	written, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatalf("reading what was installed: %v", err)
	}
	if string(written) != skill.Content() {
		t.Error("the installed skill does not match the embedded one")
	}
}

// Installing twice is what an operator does after upgrading, so it must be quiet and safe.
func TestInstallingTwiceIsANoOp(t *testing.T) {
	directory := t.TempDir()

	if _, err := skill.Install(directory); err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}

	result, err := skill.Install(directory)
	if err != nil {
		t.Fatalf("a second Install() = %v, want nil", err)
	}
	if !result.Unchanged {
		t.Error("Install() did not report that nothing changed")
	}
}

// An older copy must be replaced on upgrade, or the agents keep reading last month's advice.
func TestInstallUpdatesAnOlderCopy(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "lac", "SKILL.md")

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("creating the directory: %v", err)
	}

	older := "---\nname: lac\ndescription: an older version\n---\n\nThe local agents coordinator, once.\n"
	if err := os.WriteFile(target, []byte(older), 0o644); err != nil {
		t.Fatalf("writing the older copy: %v", err)
	}

	result, err := skill.Install(directory)
	if err != nil {
		t.Fatalf("Install() = %v, want nil", err)
	}
	if !result.Updated {
		t.Error("Install() did not report that it replaced the older copy")
	}

	written, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading the result: %v", err)
	}
	if string(written) != skill.Content() {
		t.Error("the older copy was not replaced")
	}
}

// Somebody else's skill that happens to share the name is their work, not ours to overwrite.
func TestInstallRefusesToOverwriteSomebodyElsesSkill(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "lac", "SKILL.md")

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("creating the directory: %v", err)
	}

	theirs := "---\nname: lac\ndescription: my own notes about lacquer\n---\n\nNothing to do with agents.\n"
	if err := os.WriteFile(target, []byte(theirs), 0o644); err != nil {
		t.Fatalf("writing their skill: %v", err)
	}

	if _, err := skill.Install(directory); err == nil {
		t.Fatal("Install() overwrote a skill it did not write")
	}

	survived, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading their skill: %v", err)
	}
	if string(survived) != theirs {
		t.Error("their skill was destroyed")
	}
}
