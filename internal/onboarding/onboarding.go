// Package onboarding gates the dashboard behind the deployment's first-run
// setup — on a PCS, claiming the box's own local account.
//
// The entire feature is one file on disk: ${StateDir}/onboarding.json. Its
// PRESENCE is the state. While it is there, the dashboard is replaced by an
// interstitial pointing at the URL it names; when the deployment's setup is
// done, the deployment deletes the file and the dashboard returns. Maison never
// writes it and never decides for itself when setup is finished. That is the
// same division of labour .env.app already has — the deployment's file, read
// live — and the same one that makes template-root's onboarding.sh replaceable:
// what "onboarded" means is the deployment's business, not the dashboard's.
//
// There is deliberately no status endpoint to poll and no completion callback to
// trust. A callback would be a client-controlled state transition: anything that
// can reach Maison — every container on a flat app network, or a curious owner
// with the address bar — could mark the box set up. And a boolean cached here
// would go stale exactly where it hurts, because "setup is done" is not a fact
// about Maison: a restored backup or a migrated box can carry the marker without
// the credential it is supposed to stand for, and the owner would face a
// dashboard that never asks and a login that never works. Reading the file each
// time keeps Maison out of the business of remembering; the deployment's
// self-check reconciles the file against the real answer on every tick.
//
// No file = feature off, which is what a standalone Maison always sees.
package onboarding

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/yundera/maison/internal/config"
)

// Config is the deployment's onboarding file.
type Config struct {
	// URL is where the browser goes to complete setup. Absolute, because it is
	// normally a different host from the dashboard's.
	URL string `json:"url"`
}

// Path is where that file lives: beside Maison's settings and .env.app, in the
// state directory. A deployment writes it there; Maison only ever reads it.
func Path(cfg config.Config) string {
	return filepath.Join(cfg.StateDir(), "onboarding.json")
}

// Pending reports whether setup is still owed, and where to send the browser
// for it.
//
// EVERY failure answers "not pending". A missing file is the normal off state.
// A malformed, unreadable or nonsensical one is an operator mistake, and the two
// ways of being wrong about it are not symmetrical: guessing "not pending" costs
// a redundant trip through a wizard the owner can still reach by its own URL,
// while guessing "pending" locks them out of their own dashboard behind a page
// whose only button goes nowhere. Fail open.
func Pending(cfg config.Config) (string, bool) {
	b, err := os.ReadFile(Path(cfg))
	if err != nil {
		return "", false
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return "", false
	}
	raw := strings.TrimSpace(c.URL)
	if raw == "" {
		return "", false
	}
	// The file is written by root on the host, so this is a footgun guard rather
	// than a defence: the URL is rendered into an anchor, and a scheme like
	// `javascript:` there would turn a typo in a config file into script on the
	// dashboard's own origin.
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return raw, true
}
