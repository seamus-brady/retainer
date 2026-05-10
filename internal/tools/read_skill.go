package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/seamus-brady/retainer/internal/llm"
	"github.com/seamus-brady/retainer/internal/skills"
)

// ReadSkill loads the body of a SKILL.md file by path. Used by the
// agent (and by the cog) to consult a procedure before acting in its
// class — see `delegation-strategy`, `memory-management`,
// `web-research`, etc.
//
// Path safety is non-negotiable. The handler routes through
// `skills.ReadBody`, which:
//
//  1. Requires the path ends with `/SKILL.md`.
//  2. Requires the file exists and is a regular file (not a
//     directory or device).
//  3. Resolves symlinks and confirms the canonical path is under
//     one of the configured skills directories.
//
// A `read_skill` call with `/etc/passwd` or a SKILL.md symlinked to
// somewhere outside the skills tree returns an error rather than
// reading the contents.
type ReadSkill struct {
	// SkillsDirs is the allow-list of directories that contain
	// skill bodies. Typically `<workspace>/config/skills` plus an
	// optional `~/.config/retainer/skills`. Empty list = no
	// reads possible (all calls fail safely).
	SkillsDirs []string
}

func (ReadSkill) Tool() llm.Tool {
	return llm.Tool{
		Name: "read_skill",
		Description: "Read the body of a skill (a procedure file at SKILL.md). Pass the `location` " +
			"value from the matching <skill> entry in <available_skills>. The skill body is markdown " +
			"that walks you through the decision procedure for a class of action — read it before " +
			"acting in that class. Returns the markdown verbatim.",
		InputSchema: llm.Schema{
			Name: "read_skill",
			Properties: map[string]llm.Property{
				"path": {
					Type: "string",
					Description: "Absolute path to the SKILL.md file. Must come from the <location> tag " +
						"of an entry in <available_skills>.",
				},
			},
			Required: []string{"path"},
		},
	}
}

func (h ReadSkill) Execute(_ context.Context, input []byte) (string, error) {
	if len(input) == 0 {
		return "", fmt.Errorf("read_skill: empty input")
	}
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("read_skill: decode input: %w", err)
	}
	path := strings.TrimSpace(in.Path)
	if path == "" {
		return "", fmt.Errorf("read_skill: path must not be empty")
	}
	body, err := skills.ReadBody(path, h.SkillsDirs)
	if err != nil {
		// ErrPathInvalid is the safety check; surface as a tool
		// error so the LLM can recover (e.g. by re-reading the
		// available_skills block to find a valid path).
		if errors.Is(err, skills.ErrPathInvalid) {
			return "", fmt.Errorf("read_skill: %w", err)
		}
		return "", fmt.Errorf("read_skill: %w", err)
	}
	return body, nil
}
