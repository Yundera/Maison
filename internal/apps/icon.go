package apps

import (
	"context"
	"log"
	"path/filepath"
	"strings"

	"github.com/yundera/maison/internal/appicon"
)

// The dashboard renders an installed app's tile from the icon file in the app's
// own folder, not from the URL in its compose — see internal/appicon for why.
// This file is the read half: List swaps in the local URL when the copy exists,
// the server serves the file behind it, and EnsureIcons fills in the apps that
// have no copy yet (everything installed before Maison started keeping one).

// localIcon returns the dashboard URL for a managed app's copied icon, or "" when
// the app has none — in which case the tile keeps the icon URL from its compose.
func (r *Registry) localIcon(project string) string {
	if appicon.Path(filepath.Join(r.cfg.AppsDir(), project)) == "" {
		return ""
	}
	return appicon.URL(project)
}

// IconPath is the on-disk icon of a managed app, for the HTTP handler that serves
// it. Empty when the app has no copy — or when id is not a plain app name, since
// the answer is handed straight to a file server.
func (r *Registry) IconPath(id string) string {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return ""
	}
	return appicon.Path(filepath.Join(r.cfg.AppsDir(), id))
}

// EnsureIcons takes the local icon copy for every managed app that has none, by
// downloading the icon URL its compose declares.
//
// It exists for the apps already installed on a box when this landed: their
// folders have no icon, and nothing would ever put one there — an app is only
// otherwise copied at install and refreshed at update, and an app can sit
// installed and unchanged for years. Called once at boot, in the background.
//
// Everything here is best-effort and idempotent: an app that already has a copy
// is skipped, and a download that fails (an offline box, a dead CDN link) leaves
// the tile falling back to the URL exactly as before, to be retried next boot.
func (r *Registry) EnsureIcons(ctx context.Context) {
	for _, name := range r.managedDirs() {
		dir := filepath.Join(r.cfg.AppsDir(), name)
		if appicon.Path(dir) != "" {
			continue
		}
		src := r.iconURL(name)
		if src == "" {
			continue
		}
		if err := appicon.Write(ctx, dir, "", src); err != nil {
			log.Printf("apps: %s: icon: %v", name, err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// iconURL is the icon a managed app's compose declares, x-compose-app winning
// over x-casaos — the same precedence buildApp applies to the tile.
func (r *Registry) iconURL(project string) string {
	si, ca := r.metaFor(project, "")
	icon := ""
	if si != nil {
		icon = si.Icon
	}
	if ca != nil && ca.Icon != "" {
		icon = ca.Icon
	}
	return icon
}
