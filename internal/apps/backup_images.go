package apps

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/yundera/maison/internal/composecmd"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/incident"
)

// ensureImages makes sure every image the app's `compose up` will ask for is already
// in the local image store, pulling what is missing — while the app is still serving.
//
// Why this exists: the restart after a backup is a full `compose up`, and compose
// removes the old containers before it discovers it has to pull. If the compose was
// rewritten since the containers were created (a registry move, a pin edited by hand)
// the images it names may never have been pulled, and a registry that is down or
// does not have the tag at that moment leaves the app stopped with nothing but a log
// line. That is how watch.nsl.sh lost fifteen apps for eleven hours on 2026-11-12:
// the compose files had moved to a new registry the evening before, the 04:00 backup
// stopped each app, and every restart failed its pull.
//
// So the pull is moved in front of the stop, the same order an update already uses
// (installer.ApplyUpdate pulls while the old version serves). A failure here refuses
// the backup *before* anything is stopped: one night without a backup of this app,
// instead of an outage. The error names the images, and it lands in the backup run's
// incident like any other failed target.
//
// Only managed apps are checked: an unmanaged stack is restarted with StartProject,
// which reuses its existing containers and never pulls.
func (r *Registry) ensureImages(ctx context.Context, id string, emit func(BackupEvent)) error {
	if r.dx == nil || !r.isManaged(id) {
		return nil
	}
	dir := filepath.Join(r.cfg.AppsDir(), id)
	images, err := composecmd.Images(ctx, dir, id, r.composeFiles(dir), envinject.Env(r.cfg, id))
	if err != nil {
		return fmt.Errorf("could not resolve the images it restarts from: %w", err)
	}
	var missing []string
	for _, img := range images {
		ok, err := r.dx.HasImage(ctx, img)
		if err != nil {
			return fmt.Errorf("could not check image %s: %w", img, err)
		}
		if !ok {
			missing = append(missing, img)
		}
	}
	var failed []string
	for _, img := range missing {
		emit(BackupEvent{Phase: PhaseCopy, Message: "Pulling " + img})
		if err := r.dx.PullImage(ctx, img, func(float64, string) {}); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", img, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("left running and not backed up: it could not be restarted afterwards, "+
			"because these images are not on the box and cannot be pulled: %s", strings.Join(failed, "; "))
	}
	return nil
}

// restartIncidentID is the incident a backup raises when it could not bring the app
// back up. KindAppStackup, so the detectors already clear it if the app is uninstalled.
func restartIncidentID(id string) string { return "app.restart:" + id }

func restartIncident(id string, err error) incident.Report {
	return incident.Report{
		ID:       restartIncidentID(id),
		Kind:     incident.KindAppStackup,
		Severity: incident.Critical,
		Title:    id + " did not come back up after its backup",
		Detail: "The backup stopped " + id + " and could not start it again:\n" + err.Error() +
			"\n\nThe app stays down until it is started. Fix the cause above (often an image " +
			"the registry could not serve), then start the app from the dashboard.",
		Args: map[string]string{"app": id},
	}
}
