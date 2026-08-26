package installer

import (
	"context"
	"log"

	"github.com/yundera/maison/internal/appicon"
	"github.com/yundera/maison/internal/appstore"
)

// writeIcon copies the store app's icon into the app folder, beside the compose
// file, so the dashboard renders its tile from disk (see internal/appicon).
//
// Failing to get an icon is logged, never returned: an app whose icon is missing
// from its store — or whose CDN is unreachable from this box — installs and
// updates exactly as before, with the tile falling back to the URL in its compose.
func writeIcon(ctx context.Context, app *appstore.CatalogApp, appDir, project string) {
	if app == nil {
		return
	}
	if err := appicon.Write(ctx, appDir, app.Dir(), app.Icon); err != nil {
		log.Printf("%s: icon: %v", project, err)
	}
}
