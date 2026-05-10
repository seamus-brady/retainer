package skills

import (
	"embed"
	"io/fs"
	"strings"
)

//go:embed defaults/*/SKILL.md
var embeddedDefaults embed.FS

// EmbeddedSkills returns the starter skills shipped with the binary,
// keyed by skill ID with relative-path → bytes inside each skill.
// Used by `retainer init` to seed `<workspace>/config/skills/<id>/`
// so a fresh workspace has the load-bearing decision procedures
// (delegation-strategy, memory-management, web-research) ready to
// read.
//
// Returns an empty map when nothing is embedded — defensive against
// build configurations that strip the embed.
func EmbeddedSkills() map[string]map[string][]byte {
	out := make(map[string]map[string][]byte)
	_ = fs.WalkDir(embeddedDefaults, "defaults", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		// path is e.g. "defaults/delegation-strategy/SKILL.md"
		rel := strings.TrimPrefix(path, "defaults/")
		parts := strings.SplitN(rel, "/", 2)
		if len(parts) != 2 {
			return nil
		}
		id, file := parts[0], parts[1]
		body, err := embeddedDefaults.ReadFile(path)
		if err != nil {
			return nil
		}
		if out[id] == nil {
			out[id] = make(map[string][]byte)
		}
		out[id][file] = body
		return nil
	})
	return out
}

// DefaultBootstrapSkillIDs is the canonical set of skills inlined on a
// fresh-session first cycle. Mirrors Springdrift's default
// (`delegation-strategy` + `memory-management` per the porting doc;
// `system-map` is in Springdrift but deferred here because it's
// heavily cross-system and would need a full Retainer rewrite).
var DefaultBootstrapSkillIDs = []string{
	"delegation-strategy",
	"memory-management",
}

// SeedDir is the relative directory name workspace skills live under.
// `retainer init` writes into <workspace>/config/<SeedDir>/<id>/SKILL.md.
const SeedDir = "skills"
