// Package example exposes the embedded workspace template (config.example.toml).
// The binary uses this to seed new workspaces via `retainer init`, so
// `go install`-distributed binaries don't need the source tree at runtime.
package example

import _ "embed"

//go:embed config.example.toml
var ConfigTOML []byte
