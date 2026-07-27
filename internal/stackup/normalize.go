package stackup

import (
	"bytes"
	"log"
	"os"
	"path/filepath"

	"github.com/yundera/maison/internal/appenv"
	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
)

// Normalize reconciles an app's base compose and .env with the deployment as it is
// right now, and runs before every up.
//
// An app is installed against one deployment and started against whatever the
// deployment has since become: a new app network, a new data root, a new domain or
// public IP. Nothing about that invalidates the app's own config, so nothing about
// it should stop the app from starting. Normalize is what makes that true:
//
//   - the base compose is put back through envinject.Transform, so its data-root
//     binds and its external network are ${DATA_ROOT} / ${APP_NET} references
//     rather than whatever those happened to resolve to at install time;
//   - the deployment's .env.app is ensured into the app's own .env, so the same up
//     run by hand from the app's folder resolves those references identically.
//
// Both are idempotent, and both are best-effort: a compose we cannot parse is the
// operator's to fix, and failing the up over it would strand the app with no way
// back — so we carry on with what is on disk and let `docker compose` report.
func Normalize(cfg config.Config, project, dir string) {
	basePath := filepath.Join(dir, "docker-compose.yml")

	if raw, err := os.ReadFile(basePath); err == nil {
		f, err := composefile.Parse(raw)
		if err != nil {
			log.Printf("%s: normalize compose: %v", project, err)
		} else {
			main := ""
			if si, _ := f.StoreInfo(); si != nil {
				main = si.Main
			}
			out, err := envinject.Transform(raw, cfg, main)
			switch {
			case err != nil:
				log.Printf("%s: normalize compose: %v", project, err)
			case !bytes.Equal(out, raw):
				if err := os.WriteFile(basePath, out, 0o644); err != nil {
					log.Printf("%s: normalize compose: %v", project, err)
				}
			}
		}
	}

	if err := appenv.Sync(cfg, project, dir); err != nil {
		log.Printf("%s: sync .env: %v", project, err)
	}
}
