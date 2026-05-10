package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seamus-brady/retainer/internal/skills"
)

func writeTestSkill(t *testing.T, dir, id, body string) string {
	t.Helper()
	skillDir := filepath.Join(dir, id)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(skillDir, skills.SkillMdFilename)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadSkill_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := writeTestSkill(t, dir, "foo",
		"---\nname: foo\ndescription: x\n---\n\n# Procedure\n\nDo this then that.")

	h := ReadSkill{SkillsDirs: []string{dir}}
	body, err := h.Execute(context.Background(),
		[]byte(`{"path":"`+path+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "Do this then that") {
		t.Errorf("body = %q", body)
	}
}

func TestReadSkill_RejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	h := ReadSkill{SkillsDirs: []string{dir}}
	_, err := h.Execute(context.Background(),
		[]byte(`{"path":"/etc/passwd"}`))
	if err == nil {
		t.Fatal("expected error for /etc/passwd")
	}
	if !errors.Is(err, skills.ErrPathInvalid) {
		t.Errorf("err = %v, want ErrPathInvalid wrapped", err)
	}
}

func TestReadSkill_RejectsPathOutsideSkillsDirs(t *testing.T) {
	configured := t.TempDir()
	other := t.TempDir()
	intruder := writeTestSkill(t, other, "foo", "---\nname:foo\ndescription:x\n---")

	h := ReadSkill{SkillsDirs: []string{configured}}
	_, err := h.Execute(context.Background(),
		[]byte(`{"path":"`+intruder+`"}`))
	if err == nil {
		t.Fatal("expected error for path outside configured skills_dirs")
	}
}

func TestReadSkill_RejectsNonSkillMdSuffix(t *testing.T) {
	dir := t.TempDir()
	otherFile := filepath.Join(dir, "foo", "OTHER.md")
	if err := os.MkdirAll(filepath.Dir(otherFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherFile, []byte("not a skill"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := ReadSkill{SkillsDirs: []string{dir}}
	_, err := h.Execute(context.Background(),
		[]byte(`{"path":"`+otherFile+`"}`))
	if err == nil || !errors.Is(err, skills.ErrPathInvalid) {
		t.Errorf("err = %v, want ErrPathInvalid", err)
	}
}

func TestReadSkill_RejectsMissingFile(t *testing.T) {
	dir := t.TempDir()
	h := ReadSkill{SkillsDirs: []string{dir}}
	_, err := h.Execute(context.Background(),
		[]byte(`{"path":"`+filepath.Join(dir, "ghost", skills.SkillMdFilename)+`"}`))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestReadSkill_RejectsEmptyInput(t *testing.T) {
	h := ReadSkill{SkillsDirs: []string{"/tmp"}}
	_, err := h.Execute(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "empty input") {
		t.Errorf("err = %v", err)
	}
}

func TestReadSkill_RejectsMalformedInput(t *testing.T) {
	h := ReadSkill{SkillsDirs: []string{"/tmp"}}
	_, err := h.Execute(context.Background(), []byte("{not json"))
	if err == nil || !strings.Contains(err.Error(), "decode input") {
		t.Errorf("err = %v", err)
	}
}

func TestReadSkill_RejectsEmptyPath(t *testing.T) {
	h := ReadSkill{SkillsDirs: []string{"/tmp"}}
	_, err := h.Execute(context.Background(), []byte(`{"path":"   "}`))
	if err == nil || !strings.Contains(err.Error(), "path must not be empty") {
		t.Errorf("err = %v", err)
	}
}

func TestReadSkill_NoSkillsDirsAllConfiguredCallsFail(t *testing.T) {
	// Defensive: empty SkillsDirs means no path can ever be safe;
	// every call must error rather than treating "no allow-list"
	// as "allow everything".
	dir := t.TempDir()
	path := writeTestSkill(t, dir, "foo", "---\nname:foo\ndescription:x\n---")

	h := ReadSkill{SkillsDirs: nil}
	_, err := h.Execute(context.Background(),
		[]byte(`{"path":"`+path+`"}`))
	if err == nil {
		t.Fatal("empty SkillsDirs should reject all calls")
	}
}

func TestReadSkill_ToolMetadata(t *testing.T) {
	tool := ReadSkill{}.Tool()
	if tool.Name != "read_skill" {
		t.Errorf("name = %q", tool.Name)
	}
	if !strings.Contains(tool.Description, "read") {
		t.Errorf("description should mention reading: %q", tool.Description)
	}
	if _, ok := tool.InputSchema.Properties["path"]; !ok {
		t.Error("missing path property")
	}
	if len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != "path" {
		t.Errorf("required = %+v", tool.InputSchema.Required)
	}
}
