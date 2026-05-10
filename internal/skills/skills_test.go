package skills

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill is a test helper: creates a SKILL.md (and optionally a
// skill.toml) in a fresh subdir of dir.
func writeSkill(t *testing.T, dir, id, body, tomlBody string) {
	t.Helper()
	skillDir := filepath.Join(dir, id)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, SkillMdFilename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if tomlBody != "" {
		if err := os.WriteFile(filepath.Join(skillDir, SkillTomlFilename), []byte(tomlBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ---- frontmatter parsing ----

func TestParseFrontmatter_Minimal(t *testing.T) {
	body := `---
name: foo
description: a test skill
---

# Body`
	name, desc, agents, ok := parseFrontmatter(body)
	if !ok {
		t.Fatal("expected ok")
	}
	if name != "foo" || desc != "a test skill" {
		t.Errorf("got name=%q desc=%q", name, desc)
	}
	if len(agents) != 0 {
		t.Errorf("agents = %+v, want empty (legacy 'all')", agents)
	}
}

func TestParseFrontmatter_WithAgents(t *testing.T) {
	body := `---
name: foo
description: bar
agents: cognitive, researcher
---`
	_, _, agents, ok := parseFrontmatter(body)
	if !ok {
		t.Fatal()
	}
	if len(agents) != 2 || agents[0] != "cognitive" || agents[1] != "researcher" {
		t.Errorf("agents = %+v", agents)
	}
}

func TestParseFrontmatter_RejectsMissingRequired(t *testing.T) {
	cases := []string{
		`---
name: only-name
---`,
		`---
description: only-desc
---`,
		``,
		`# no frontmatter at all`,
	}
	for _, c := range cases {
		if _, _, _, ok := parseFrontmatter(c); ok {
			t.Errorf("unexpected ok for %q", c)
		}
	}
}

func TestParseFrontmatter_HandlesColonInValue(t *testing.T) {
	// Description with a URL-ish colon — split on first ":" only.
	body := `---
name: foo
description: see https://example.com/docs for details
---`
	_, desc, _, ok := parseFrontmatter(body)
	if !ok {
		t.Fatal()
	}
	if !strings.Contains(desc, "https://example.com/docs") {
		t.Errorf("desc = %q", desc)
	}
}

// ---- discovery ----

func TestDiscover_FindsSimpleSkill(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "foo", "---\nname: foo\ndescription: foo skill\n---\n\nbody", "")

	got := Discover([]string{dir})
	if len(got) != 1 || got[0].ID != "foo" {
		t.Fatalf("got %+v", got)
	}
	if got[0].Name != "foo" || got[0].Description != "foo skill" {
		t.Errorf("frontmatter not loaded: %+v", got[0])
	}
	if got[0].Status != StatusActive {
		t.Errorf("status = %q, want active (default)", got[0].Status)
	}
	if got[0].Version != 1 {
		t.Errorf("version = %d, want 1 (default)", got[0].Version)
	}
	if got[0].Author.Kind != AuthorOperator {
		t.Errorf("author = %+v, want operator default", got[0].Author)
	}
	if got[0].TokenCostEstimate <= 0 {
		t.Errorf("token cost should be > 0, got %d", got[0].TokenCostEstimate)
	}
}

func TestDiscover_OrdersStablyByID(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "zeta", "---\nname: z\ndescription: x\n---", "")
	writeSkill(t, dir, "alpha", "---\nname: a\ndescription: x\n---", "")
	writeSkill(t, dir, "mid", "---\nname: m\ndescription: x\n---", "")

	got := Discover([]string{dir})
	if got[0].ID != "alpha" || got[1].ID != "mid" || got[2].ID != "zeta" {
		t.Errorf("not sorted: %+v", []string{got[0].ID, got[1].ID, got[2].ID})
	}
}

func TestDiscover_SkipsMalformedFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "good", "---\nname: g\ndescription: ok\n---", "")
	writeSkill(t, dir, "bad", "---\nthis is not valid yaml\n---", "")

	got := Discover([]string{dir})
	if len(got) != 1 || got[0].ID != "good" {
		t.Fatalf("got %+v", got)
	}
}

func TestDiscover_SkipsNonDirectoryEntries(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "valid", "---\nname: v\ndescription: x\n---", "")
	// Stray file at top level.
	_ = os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("hi"), 0o644)

	got := Discover([]string{dir})
	if len(got) != 1 || got[0].ID != "valid" {
		t.Errorf("got %+v", got)
	}
}

func TestDiscover_MissingDirectoryNoError(t *testing.T) {
	// Common case: ~/.config/retainer/skills doesn't exist.
	got := Discover([]string{"/this/path/does/not/exist"})
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestDiscover_MultipleDirsConcatenate(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	writeSkill(t, dir1, "alpha", "---\nname: a\ndescription: x\n---", "")
	writeSkill(t, dir2, "beta", "---\nname: b\ndescription: y\n---", "")

	got := Discover([]string{dir1, dir2})
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

// ---- skill.toml sidecar ----

func TestDiscover_SidecarOverridesFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "foo",
		"---\nname: foo\ndescription: from-frontmatter\n---\n\nbody",
		`id = "foo-overridden"
name = "Foo (Sidecar)"
description = "from-sidecar"
version = 3
status = "archived"

[scoping]
agents = ["researcher"]
contexts = ["web", "research"]

[provenance]
author = "system"
created_at = "2026-04-01T00:00:00Z"
updated_at = "2026-04-30T00:00:00Z"
`)

	got := Discover([]string{dir})
	if len(got) != 1 {
		t.Fatalf("got %d", len(got))
	}
	s := got[0]
	if s.ID != "foo-overridden" {
		t.Errorf("id = %q", s.ID)
	}
	if s.Name != "Foo (Sidecar)" {
		t.Errorf("name = %q", s.Name)
	}
	if s.Description != "from-sidecar" {
		t.Errorf("description = %q (sidecar must win)", s.Description)
	}
	if s.Version != 3 {
		t.Errorf("version = %d", s.Version)
	}
	if s.Status != StatusArchived {
		t.Errorf("status = %q", s.Status)
	}
	if len(s.Agents) != 1 || s.Agents[0] != "researcher" {
		t.Errorf("agents = %+v", s.Agents)
	}
	if len(s.Contexts) != 2 {
		t.Errorf("contexts = %+v", s.Contexts)
	}
	if s.Author.Kind != AuthorSystem {
		t.Errorf("author = %+v", s.Author)
	}
	if s.CreatedAt != "2026-04-01T00:00:00Z" {
		t.Errorf("created_at = %q", s.CreatedAt)
	}
}

func TestDiscover_SidecarAgentDefaultIsConservativeCognitive(t *testing.T) {
	// When skill.toml is present (any field set) but doesn't specify
	// agents, the conservative default is ["cognitive"] — opposite of
	// frontmatter's "all" default.
	dir := t.TempDir()
	writeSkill(t, dir, "foo",
		"---\nname: foo\ndescription: from-fm\n---",
		`version = 2
`)
	got := Discover([]string{dir})
	if len(got) != 1 {
		t.Fatalf("got %d", len(got))
	}
	if len(got[0].Agents) != 1 || got[0].Agents[0] != "cognitive" {
		t.Errorf("agents = %+v, want [cognitive] default when sidecar present", got[0].Agents)
	}
}

func TestDiscover_SidecarUnknownStatusFallsBackToActive(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "foo",
		"---\nname: foo\ndescription: x\n---",
		`status = "weird"`)
	got := Discover([]string{dir})
	if got[0].Status != StatusActive {
		t.Errorf("status = %q, want active (defensive)", got[0].Status)
	}
}

func TestDiscover_AgentAuthorRecord(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "foo",
		"---\nname: foo\ndescription: x\n---",
		`[provenance]
author = "agent"
agent_name = "remembrancer"
cycle_id = "abc-123"
`)
	got := Discover([]string{dir})
	a := got[0].Author
	if a.Kind != AuthorAgent {
		t.Errorf("kind = %q", a.Kind)
	}
	if a.AgentName != "remembrancer" || a.CycleID != "abc-123" {
		t.Errorf("agent author = %+v", a)
	}
}

func TestDiscover_MalformedTomlIgnored(t *testing.T) {
	// Sidecar parse error → silently fall back to frontmatter
	// defaults. The skill still loads.
	dir := t.TempDir()
	writeSkill(t, dir, "foo",
		"---\nname: foo\ndescription: ok\n---",
		`this is not toml = =`)
	got := Discover([]string{dir})
	if len(got) != 1 {
		t.Fatalf("got %d", len(got))
	}
	if got[0].Description != "ok" {
		t.Errorf("frontmatter should win when toml malformed: %+v", got[0])
	}
}

// ---- ForAgent ----

func sampleSkills() []SkillMeta {
	return []SkillMeta{
		{ID: "all", Agents: []string{"all"}},
		{ID: "all-spec", Agents: []string{"all_specialists"}},
		{ID: "cog", Agents: []string{"cognitive"}},
		{ID: "researcher", Agents: []string{"researcher"}},
		{ID: "multi", Agents: []string{"researcher", "observer"}},
		{ID: "legacy", Agents: nil},
	}
}

func TestForAgent_Cognitive(t *testing.T) {
	got := ForAgent(sampleSkills(), "cognitive")
	want := map[string]bool{"all": true, "cog": true, "legacy": true}
	for _, s := range got {
		if !want[s.ID] {
			t.Errorf("unexpected match for cognitive: %s", s.ID)
		}
		delete(want, s.ID)
	}
	if len(want) != 0 {
		t.Errorf("missing matches: %+v", want)
	}
}

func TestForAgent_Researcher(t *testing.T) {
	got := ForAgent(sampleSkills(), "researcher")
	want := map[string]bool{
		"all":        true,
		"all-spec":   true, // researcher is a specialist
		"researcher": true,
		"multi":      true,
		"legacy":     true,
	}
	for _, s := range got {
		if !want[s.ID] {
			t.Errorf("unexpected: %s", s.ID)
		}
		delete(want, s.ID)
	}
	if len(want) != 0 {
		t.Errorf("missing: %+v", want)
	}
}

func TestForAgent_AllSpecialistsExcludesCognitive(t *testing.T) {
	skills := []SkillMeta{
		{ID: "spec-only", Agents: []string{"all_specialists"}},
	}
	if len(ForAgent(skills, "cognitive")) != 0 {
		t.Error("all_specialists must NOT match cognitive")
	}
	if len(ForAgent(skills, "researcher")) != 1 {
		t.Error("all_specialists must match researcher")
	}
}

// ---- ForContext ----

func TestForContext_EmptyContextsAlwaysInjects(t *testing.T) {
	skills := []SkillMeta{{ID: "x", Contexts: nil}}
	got := ForContext(skills, []string{"web"})
	if len(got) != 1 {
		t.Errorf("empty contexts should always inject; got %+v", got)
	}
}

func TestForContext_AllAlwaysInjects(t *testing.T) {
	skills := []SkillMeta{{ID: "x", Contexts: []string{"all"}}}
	if len(ForContext(skills, nil)) != 1 {
		t.Error(`"all" context should inject regardless of query domains`)
	}
}

func TestForContext_DomainOverlap(t *testing.T) {
	skills := []SkillMeta{
		{ID: "web-only", Contexts: []string{"web"}},
		{ID: "code-only", Contexts: []string{"code"}},
	}
	got := ForContext(skills, []string{"web", "research"})
	if len(got) != 1 || got[0].ID != "web-only" {
		t.Errorf("got %+v", got)
	}
}

func TestForContext_NoOverlapDrops(t *testing.T) {
	skills := []SkillMeta{{ID: "x", Contexts: []string{"web"}}}
	if len(ForContext(skills, []string{"code"})) != 0 {
		t.Error("no overlap should drop")
	}
}

// ---- ToSystemPromptXML ----

func TestToSystemPromptXML_Empty(t *testing.T) {
	if got := ToSystemPromptXML(nil); got != "" {
		t.Errorf("empty input should return empty: %q", got)
	}
}

func TestToSystemPromptXML_Shape(t *testing.T) {
	got := ToSystemPromptXML([]SkillMeta{
		{ID: "a", Name: "Skill A", Description: "Description A", Path: "/path/to/A/SKILL.md"},
		{ID: "b", Name: "Skill B", Description: "Description B", Path: "/path/to/B/SKILL.md"},
	})
	for _, want := range []string{
		"<available_skills>",
		"  <skill>",
		"    <name>Skill A</name>",
		"    <description>Description A</description>",
		"    <location>/path/to/A/SKILL.md</location>",
		"    <name>Skill B</name>",
		"</available_skills>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestToSystemPromptXML_EscapesSpecialChars(t *testing.T) {
	got := ToSystemPromptXML([]SkillMeta{
		{Name: `A & B "test"`, Description: `<dangerous>`, Path: "/x"},
	})
	for _, want := range []string{"&amp;", "&quot;", "&lt;dangerous&gt;"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing escape %q in: %s", want, got)
		}
	}
	for _, naughty := range []string{`A & B`, `<dangerous>`} {
		if strings.Contains(got, naughty) {
			t.Errorf("raw %q leaked into XML output: %s", naughty, got)
		}
	}
}

// ---- ExpandTilde ----

func TestExpandTilde_NoTilde(t *testing.T) {
	if got := ExpandTilde("/abs/path"); got != "/abs/path" {
		t.Errorf("got %q", got)
	}
}

func TestExpandTilde_TildeSlash(t *testing.T) {
	got := ExpandTilde("~/skills")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("UserHomeDir failed, skipping")
	}
	want := filepath.Join(home, "skills")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ---- safe path / ReadBody ----

func TestIsSafePath_HappyPath(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "foo", "---\nname: foo\ndescription: x\n---\n\nbody", "")
	skillPath := filepath.Join(dir, "foo", SkillMdFilename)

	if err := IsSafePath(skillPath, []string{dir}); err != nil {
		t.Errorf("expected safe, got %v", err)
	}
}

func TestIsSafePath_RejectsPathNotEndingInSkillMd(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "foo", "OTHER.md")
	if err := IsSafePath(other, []string{dir}); !errors.Is(err, ErrPathInvalid) {
		t.Errorf("err = %v, want ErrPathInvalid", err)
	}
}

func TestIsSafePath_RejectsPathOutsideSkillsDirs(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	writeSkill(t, other, "foo", "---\nname: foo\ndescription: x\n---", "")
	skillPath := filepath.Join(other, "foo", SkillMdFilename)

	if err := IsSafePath(skillPath, []string{dir}); !errors.Is(err, ErrPathInvalid) {
		t.Errorf("err = %v, want ErrPathInvalid (path outside configured dir)", err)
	}
}

func TestIsSafePath_RejectsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, "skills")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(outside, SkillMdFilename)
	if err := os.WriteFile(target, []byte("---\nname: x\ndescription: y\n---"), 0o644); err != nil {
		t.Fatal(err)
	}

	// symlink: skillsDir/sneaky/SKILL.md -> outside/SKILL.md
	if err := os.MkdirAll(filepath.Join(skillsDir, "sneaky"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(skillsDir, "sneaky", SkillMdFilename)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not supported on this platform: %v", err)
	}

	err := IsSafePath(link, []string{skillsDir})
	if !errors.Is(err, ErrPathInvalid) {
		t.Errorf("symlink-escape should reject; got %v", err)
	}
}

func TestIsSafePath_RejectsMissingFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope", SkillMdFilename)
	if err := IsSafePath(missing, []string{dir}); !errors.Is(err, ErrPathInvalid) {
		t.Errorf("err = %v", err)
	}
}

func TestReadBody_HappyPath(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "foo", "---\nname: foo\ndescription: x\n---\n\nthis is the body", "")
	skillPath := filepath.Join(dir, "foo", SkillMdFilename)

	body, err := ReadBody(skillPath, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "this is the body") {
		t.Errorf("body = %q", body)
	}
}

func TestReadBody_RejectsUnsafePath(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadBody("/etc/passwd", []string{dir}); !errors.Is(err, ErrPathInvalid) {
		t.Errorf("err = %v", err)
	}
}

// ---- Token cost ----

func TestEstimateTokenCost(t *testing.T) {
	if got := EstimateTokenCost(""); got != 0 {
		t.Errorf("empty = %d", got)
	}
	if got := EstimateTokenCost("0123"); got != 1 {
		t.Errorf("4 chars = %d, want 1", got)
	}
	if got := EstimateTokenCost("01234567"); got != 2 {
		t.Errorf("8 chars = %d, want 2", got)
	}
}

// ---- statusFromString / authorKindFromString ----

func TestStatusFromString(t *testing.T) {
	if statusFromString("active") != StatusActive {
		t.Error()
	}
	if statusFromString("ARCHIVED") != StatusArchived {
		t.Error()
	}
	if statusFromString("nonsense") != StatusActive {
		t.Error()
	}
}

func TestAuthorKindFromString(t *testing.T) {
	if authorKindFromString("system") != AuthorSystem {
		t.Error()
	}
	if authorKindFromString("AGENT") != AuthorAgent {
		t.Error()
	}
	if authorKindFromString("nonsense") != AuthorOperator {
		t.Error()
	}
}
