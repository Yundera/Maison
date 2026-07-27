package apps

import (
	"os"
	"path/filepath"
	"strings"
)

// The "reached" markers record which apps have been observed responding at least
// once. Their only consumer is the launch page's first-boot patience: an app
// with no marker is treated as booting for the first time (a legitimately slow
// event for apps that seed a database or run migrations on first run), so the
// page reassures rather than warns and waits far longer before hinting at
// trouble. See internal/server/launch.go.
//
// The signal is advisory — it never gates anything — so it is stored as a plain
// empty file per app id under Maison's own state dir, and every failure here
// is swallowed: a missing or unreadable marker just means "treat as first boot",
// which is the safe direction.

func (r *Registry) reachedDir() string {
	return filepath.Join(r.cfg.StateDir(), "reached")
}

// markerName maps an app id (a compose project name) to a safe file name. Project
// names are already restricted to a conservative alphabet, but guard against path
// separators regardless so a marker can never escape reachedDir.
func markerName(id string) string {
	repl := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}
	return strings.Map(repl, id)
}

// HasReached reports whether app id has ever been observed reachable. False on
// any error (including an absent marker), so the caller falls back to first-boot
// patience.
func (r *Registry) HasReached(id string) bool {
	if id == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(r.reachedDir(), markerName(id)))
	return err == nil
}

// MarkReached records that app id has been observed reachable. Idempotent and
// best-effort — a write failure only costs a little extra first-boot patience
// next time, never correctness.
func (r *Registry) MarkReached(id string) {
	if id == "" || r.HasReached(id) {
		return
	}
	dir := r.reachedDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, markerName(id)), nil, 0o644)
}
