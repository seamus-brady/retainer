// Package paths resolves the runtime workspace directory.
//
// A workspace is a single contained directory holding one agent's state:
//
//   $RETAINER_WORKSPACE/
//     config/   ← config.toml, persona.md, skills/, character spec
//     data/     ← logs/, cycle-log/, JSONL stores
//
// The binary itself lives elsewhere (typically $GOBIN / ~/go/bin) and
// operates on the workspace it is pointed at. Multiple workspaces are
// supported — pass a different --workspace flag per invocation.
//
// Resolution priority, highest first:
//   1. explicit string passed to Resolve (from --workspace flag)
//   2. $RETAINER_WORKSPACE env var
//   3. $HOME/retainer (default)
//
// Linux and macOS only; Windows returns an error.
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const appName = "retainer"

type Paths struct {
	Workspace string
	Config    string
	Data      string
}

// Resolve returns the workspace and its config / data subdirs. Pass an empty
// string to use the env var or default; pass a non-empty path to override.
// Does no I/O — directories are not created here.
func Resolve(explicit string) (Paths, error) {
	if runtime.GOOS == "windows" {
		return Paths{}, errors.New("paths: Windows is not supported")
	}

	ws := explicit
	if ws == "" {
		ws = os.Getenv("RETAINER_WORKSPACE")
	}
	if ws == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("paths: resolve $HOME: %w", err)
		}
		ws = filepath.Join(userHome, appName)
	}

	return Paths{
		Workspace: ws,
		Config:    filepath.Join(ws, "config"),
		Data:      filepath.Join(ws, "data"),
	}, nil
}
