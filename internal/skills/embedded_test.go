package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedSkills_AllStartersPresent(t *testing.T) {
	embedded := EmbeddedSkills()
	for _, want := range []string{"delegation-strategy", "memory-management", "web-research"} {
		if _, ok := embedded[want]; !ok {
			t.Errorf("missing embedded skill %q", want)
		}
		if body, ok := embedded[want]["SKILL.md"]; !ok || len(body) == 0 {
			t.Errorf("embedded skill %q has no SKILL.md", want)
		}
	}
}

func TestEmbeddedSkills_FrontmatterParseable(t *testing.T) {
	// Each embedded skill must round-trip through parseFrontmatter
	// so it loads cleanly via Discover when seeded into a workspace.
	for id, files := range EmbeddedSkills() {
		body := string(files["SKILL.md"])
		name, desc, _, ok := parseFrontmatter(body)
		if !ok {
			t.Errorf("%s: frontmatter parse failed", id)
			continue
		}
		if name == "" || desc == "" {
			t.Errorf("%s: name=%q desc=%q", id, name, desc)
		}
	}
}

func TestEmbeddedSkills_DiscoverableAfterSeeding(t *testing.T) {
	// Seed the embedded skills into a temp dir and run Discover —
	// proves the seed/load round-trip.
	dir := t.TempDir()
	for id, files := range EmbeddedSkills() {
		for filename, body := range files {
			path := filepath.Join(dir, id, filename)
			if err := writeFile(path, body); err != nil {
				t.Fatal(err)
			}
		}
	}
	got := Discover([]string{dir})
	// Pin the canonical embedded set. Each new skill added to
	// internal/skills/defaults/ should be added here so additions
	// are deliberate, not silent.
	want := map[string]bool{
		"delegation-strategy":    true,
		"memory-management":      true,
		"web-research":           true,
		"agents-observer":        true,
		"agents-scheduler":       true,
		"agents-using-observer":  true,
		"agents-using-scheduler": true,
		"self-diagnostic":        true,
		"harness-turns":          true,
		"harness-tools":          true,
		"harness-memory":         true,
		"harness-policy":         true,
		"harness-dispatch":       true,
		"harness-errors":         true,
	}
	for _, s := range got {
		if !want[s.ID] {
			t.Errorf("unexpected discovered skill %q", s.ID)
		}
		delete(want, s.ID)
	}
	if len(want) != 0 {
		t.Errorf("missing discovered skills: %+v", want)
	}
}

func TestEmbeddedSkills_DescriptionsMentionRetainer(t *testing.T) {
	// Belt-and-braces — the user explicitly wanted "appropriate not
	// straight" ports. Each skill's description should be tailored
	// to Retainer's current shape, not Springdrift's full surface.
	embedded := EmbeddedSkills()
	for id, files := range embedded {
		body := string(files["SKILL.md"])
		_, desc, _, _ := parseFrontmatter(body)
		// Either the description directly references Retainer, or
		// the body mentions a deferred / current-shape qualifier so
		// the agent doesn't think it's the full Springdrift surface.
		mentions := []string{"Retainer", "current shape", "deferred"}
		anyHit := false
		for _, m := range mentions {
			if strings.Contains(desc, m) || strings.Contains(body, m) {
				anyHit = true
				break
			}
		}
		if !anyHit {
			t.Errorf("%s: description/body should signal current-shape adaptation; got desc=%q", id, desc)
		}
	}
}

func TestDefaultBootstrapSkillIDs(t *testing.T) {
	// Pin the canonical bootstrap set. Changing this is a deliberate
	// behavioural change, not a refactor.
	want := []string{"delegation-strategy", "memory-management"}
	if len(DefaultBootstrapSkillIDs) != len(want) {
		t.Fatalf("got %+v, want %+v", DefaultBootstrapSkillIDs, want)
	}
	for i, w := range want {
		if DefaultBootstrapSkillIDs[i] != w {
			t.Errorf("[%d] = %q, want %q", i, DefaultBootstrapSkillIDs[i], w)
		}
	}
}

func TestSeedDir(t *testing.T) {
	if SeedDir != "skills" {
		t.Errorf("SeedDir = %q, want 'skills'", SeedDir)
	}
}

func TestEmbeddedSkills_SelfDiagnosticPinsCriticalSurfaces(t *testing.T) {
	// The self-diagnostic skill is the operator's lever for "is
	// everything working?". A regression in its body that drops a
	// step would silently shrink coverage. Pin every surface the
	// procedure must touch — if a future edit removes one, the
	// fix is to either add it back or update this test deliberately.
	body, ok := EmbeddedSkills()["self-diagnostic"]
	if !ok {
		t.Fatal("self-diagnostic skill not embedded")
	}
	skillMD := string(body["SKILL.md"])

	// Each surface the diagnostic must exercise. Names match the
	// tool / agent registrations in cmd/retainer/main.go +
	// bootstrap.go — drift between this list and those is the
	// signal worth catching.
	surfaces := []string{
		"memory_write",
		"memory_read",
		"memory_clear_key",
		"agent_observer",
		"agent_researcher",
		"agent_taskmaster",
		"agent_scheduler",
		"read_skill",
	}
	for _, s := range surfaces {
		if !strings.Contains(skillMD, s) {
			t.Errorf("self-diagnostic skill missing %q surface", s)
		}
	}
}

// writeFile is a small test helper that creates parent dirs as
// needed and writes the body.
func writeFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}
