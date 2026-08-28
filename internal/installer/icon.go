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
// IconRel rather than Icon: by the time a CatalogApp leaves the store package,
// an icon named beside the compose has been rewritten into the URL that serves it
// out of the store tree, and what has to be copied is the file behind it. Icon
// still rides along for the app whose compose names an outside server, which is
// the only case left that needs a download.
//
// Failing to get an icon is logged, never returned: an app whose icon is missing
// from its store — or whose host is unreachable from this box — installs and
// updates exactly as before, with the tile falling back to the URL in its compose.
func writeIcon(ctx context.Context, app *appstore.CatalogApp, appDir, project string) {
	if app == nil {
		return
	}
	if err := appicon.Write(ctx, appDir, app.Dir(), app.IconRel(), app.Icon); err != nil {
		log.Printf("%s: icon: %v", project, err)
	}
}
