// Package agentid persists the workspace's stable agent identity —
// a UUID minted on first boot and reused for the workspace's
// lifetime. The UUID lets external auditors correlate cycle-log
// JSONL entries back to a specific Retainer instance, even across
// restarts and version upgrades.
//
// SD parity: ports `_impl_docs/ref/springdrift/src/agent_identity.gleam`.
// Storage shape diverges slightly — Springdrift writes
// `.springdrift/identity.json` (the project dir); we write
// `<workspace>/data/identity.json` so the file lives in the
// gitignored data dir alongside other runtime state.
//
// The UUID is **telemetry**, not prose. It does NOT appear in the
// persona; the agent doesn't introspect or recite it. It's stamped
// onto every cycle-log event as `instance_id` (the first 8 chars
// of the UUID) so a reader walking the JSONL can group events by
// instance.
package agentid

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/seamus-brady/retainer/internal/actorstate"
)

// Filename is the on-disk name under <workspace>/data/. Exported so
// tests + the operator-facing TUI can reference it directly.
const Filename = "identity.json"

// SchemaVersion pins the JSON shape so a future migration can
// detect old files. Bump only on a breaking schema change; field
// additions stay version 1.
const SchemaVersion = 1

// instanceIDLength is the prefix length used for the compact
// instance_id telemetry tag. Eight chars matches SD
// (`string.slice(agent_uuid, 0, 8)`); collisions across two
// workspaces are statistically tolerable for human-readable logs.
const instanceIDLength = 8

// Identity is the persisted record. JSON tags pin the wire shape —
// changing them means bumping SchemaVersion + writing a migration.
type Identity struct {
	SchemaVersion int       `json:"schema_version"`
	UUID          string    `json:"uuid"`
	CreatedAt     time.Time `json:"created_at"`
	LastSeenAt    time.Time `json:"last_seen_at"`
}

// InstanceID returns the eight-char prefix of the UUID — the
// short tag used in cycle-log emissions. Returns "" when the
// identity is zero-valued (fresh struct, never loaded).
func (i Identity) InstanceID() string {
	if len(i.UUID) < instanceIDLength {
		return i.UUID
	}
	return i.UUID[:instanceIDLength]
}

// IsZero reports whether the identity is the zero value (no UUID
// loaded). Used by callers that want to gracefully degrade
// telemetry when load failed.
func (i Identity) IsZero() bool {
	return i.UUID == ""
}

// LoadOrCreate reads <dataDir>/identity.json. When the file is
// missing OR the on-disk record is malformed, generates a fresh
// UUID + writes the new record + returns it.
//
// Always returns a usable Identity unless the filesystem itself is
// unwritable (which the caller should treat as a fatal bootstrap
// error). Updates LastSeenAt on every load — that timestamp is
// "last process start", not "last activity".
//
// Atomic write via internal/actorstate (temp + rename), so a crash
// mid-write leaves the prior identity.json intact. Schema-version
// mismatches are treated as malformed → regenerate (SD-equivalent
// behaviour: we're V1, no migrations needed yet).
func LoadOrCreate(dataDir string, logger *slog.Logger) (Identity, error) {
	if logger == nil {
		logger = slog.Default()
	}
	path := filepath.Join(dataDir, Filename)

	var existing Identity
	if err := actorstate.Read(path, &existing); err != nil {
		// Read failure means decode failure (Read returns nil for
		// missing-file). Log + regenerate — the prior file was
		// corrupt, not a hard error. SD does the same.
		logger.Warn("agentid: existing identity unreadable; regenerating", "path", path, "err", err)
		return create(path, logger)
	}

	// SchemaVersion zero means either fresh-start (Read returned
	// nil for missing file) OR a record without the version field
	// (pre-V1). Either way, regenerate.
	if existing.UUID == "" || existing.SchemaVersion != SchemaVersion {
		return create(path, logger)
	}

	// Update last-seen and write back. Best-effort: a write
	// failure here logs but doesn't fail the load — operator
	// would rather lose a timestamp update than the whole boot.
	existing.LastSeenAt = time.Now().UTC()
	if err := actorstate.Write(path, existing); err != nil {
		logger.Warn("agentid: failed to update last_seen_at; using stale timestamp", "err", err)
	}
	logger.Info("agentid: loaded existing identity",
		"uuid", existing.UUID,
		"instance_id", existing.InstanceID(),
		"created_at", existing.CreatedAt,
	)
	return existing, nil
}

// create mints a fresh identity, writes it, returns it. Only the
// "first ever boot" + "regenerate after corrupt file" paths use
// this; LoadOrCreate is the public entry point.
func create(path string, logger *slog.Logger) (Identity, error) {
	now := time.Now().UTC()
	id := Identity{
		SchemaVersion: SchemaVersion,
		UUID:          uuid.NewString(),
		CreatedAt:     now,
		LastSeenAt:    now,
	}
	if err := actorstate.Write(path, id); err != nil {
		return Identity{}, fmt.Errorf("agentid: write fresh identity: %w", err)
	}
	logger.Info("agentid: minted new identity",
		"uuid", id.UUID,
		"instance_id", id.InstanceID(),
		"path", path,
	)
	return id, nil
}

// errLogger is the sentinel value for tests that want LoadOrCreate
// to fail at write time. Not used in production.
var errLogger = errors.New("agentid: forced write error (test sentinel)")

// Suppress unused-var lint for the sentinel.
var _ = errLogger
