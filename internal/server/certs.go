package server

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/yundera/maison/internal/composecmd"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/stackup"
)

// certRefreshInterval is how often installed apps' certificates are re-picked:
// often enough that an app installed before the proxy had its certificate moves
// off the self-signed one within minutes, cheap enough (a directory listing and
// a file compare per app that declares one) to run all the time. A renewal is
// done by the proxy weeks before expiry, so it is not what sets the pace.
const certRefreshInterval = 10 * time.Minute

// refreshCertificates re-picks every installed app's `certificates` on a timer
// and restarts the services whose files changed — the half of the feature that
// a renewal, or a better certificate appearing, needs (see internal/certs).
func (s *Server) refreshCertificates(ctx context.Context) {
	t := time.NewTicker(certRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.refreshCertificatesOnce(ctx)
	}
}

func (s *Server) refreshCertificatesOnce(ctx context.Context) {
	running := map[string]map[string]bool{} // project → service → running
	if conts, err := s.dx.ListAllContainers(ctx); err == nil {
		for _, c := range conts {
			if c.State != "running" {
				continue
			}
			if running[c.Project] == nil {
				running[c.Project] = map[string]bool{}
			}
			running[c.Project][c.Service] = true
		}
	}
	for _, id := range s.apps.ManagedIDs() {
		dir := filepath.Join(s.cfg.AppsDir(), id)
		if !stackup.DeclaresCertificates(dir) {
			continue
		}
		restart, err := stackup.RefreshCertificates(s.cfg, id, dir)
		if err != nil {
			log.Printf("%s: certificates: %v", id, err)
			continue
		}
		// Only what runs: restarting a stopped container would start it.
		var svcs []string
		for _, svc := range restart {
			if running[id][svc] {
				svcs = append(svcs, svc)
			}
		}
		if len(svcs) == 0 {
			continue
		}
		files := []string{filepath.Join(dir, "docker-compose.yml")}
		if _, err := os.Stat(filepath.Join(dir, "docker-compose.override.yml")); err == nil {
			files = append(files, filepath.Join(dir, "docker-compose.override.yml"))
		}
		if err := composecmd.Restart(ctx, dir, id, files, envinject.Env(s.cfg, id), svcs); err != nil {
			log.Printf("%s: restart after certificate change: %v", id, err)
		} else {
			log.Printf("%s: certificate changed, restarted %v", id, svcs)
		}
	}
}
