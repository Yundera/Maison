package apps

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/yundera/maison/internal/appicon"
	"github.com/yundera/maison/internal/asset"
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

// EnsureIcons takes the local icon copy for every managed app that has none, from
// whatever its compose declares — a file in the app's own folder, or a URL to
// download.
//
// It exists for two kinds of app. The ones already installed on a box when the
// copy landed: their folders have no icon, and nothing would ever put one there,
// since an app is only otherwise copied at install and refreshed at update, and an
// app can sit installed and unchanged for years. And the ones nobody installed at
// all — a compose dropped into an app folder by hand, beside an icon.png, which is
// picked up here and rendered like any other tile. Called once at boot, in the
// background.
//
// The app's own folder is the source directory for a relative icon, because for an
// installed app that folder *is* where its compose lives: the rule that resolves a
// store app's icon against the extracted store resolves this one against the app.
//
// Everything here is best-effort and idempotent: an app that already has a copy
// is skipped, and a source that fails (an offline box, a dead link, a name that
// points at nothing) leaves the tile as it was, to be retried next boot.
func (r *Registry) EnsureIcons(ctx context.Context) {
	for _, name := range r.managedDirs() {
		dir := filepath.Join(r.cfg.AppsDir(), name)
		if appicon.Path(dir) != "" {
			continue
		}
		declared := r.iconURL(name)
		rel, _ := asset.Rel(declared)
		src := ""
		if asset.IsURL(declared) {
			src = declared
		}
		if rel == "" && src == "" {
			// Nothing declared, or something that is neither. The convention lookup in
			// appicon still applies: an app folder holding a plain icon.png gets a tile
			// without its compose having to say so.
			if !hasConventionIcon(dir) {
				continue
			}
		}
		if err := appicon.Write(ctx, dir, dir, rel, src); err != nil {
			log.Printf("apps: %s: icon: %v", name, err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// hasConventionIcon reports whether the app folder holds a plain icon.<ext> for
// appicon's by-convention lookup to find. Checked before calling Write only to
// keep the common case — an app that declares nothing and ships nothing — from
// doing any work at all.
func hasConventionIcon(dir string) bool {
	for _, ext := range asset.Exts {
		if st, err := os.Stat(filepath.Join(dir, "icon"+ext)); err == nil && st.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// iconURL is the icon a managed app's compose declares — a URL or a path beside
// the compose, as written — with x-compose-app winning over x-casaos, the same
// precedence buildApp applies to the tile.
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
