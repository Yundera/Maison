package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backup"
	"github.com/yundera/maison/internal/backup/adapter"
	"github.com/yundera/maison/internal/backupconfig"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/engine"
	"github.com/yundera/maison/internal/incident"
)

// Engine approval and discovery while running.
//
// An engine arrives as an adapter.json in an app's folder (adapter.Discover). That used
// to be the deployment's file alone, written by the host side; it is now also how a
// store app — Kopia on a stock box — offers itself as a backup engine. What Maison runs
// from it is an image, as root, with every app's data in scope, so a descriptor is not
// enough on its own: the owner approves the image first, and until then the engine is
// listed, raises backup.approval, and runs nothing. See docs/backup.md § Store-installed
// engines.

// gatedEngine is an engine built from a descriptor, which is every engine but local.
type gatedEngine interface {
	Approved() bool
	Descriptor() adapter.Descriptor
}

// engineApproved reports whether an engine may be run: local always, an adapter once
// the owner has approved its image.
func engineApproved(p apps.Provider) bool {
	g, ok := p.(gatedEngine)
	return !ok || g.Approved()
}

// newGatedEngine builds the provider for one descriptor. Approval is read from the store
// on every call rather than captured, so approving or revoking takes effect on the next
// verb without rebuilding anything; the image it is checked against is this descriptor's,
// and a descriptor that changes is a new provider (rescanEngines).
func newGatedEngine(cfg config.Config, store *backupconfig.Store, d adapter.Descriptor) *adapter.Provider {
	repo := adapter.ImageRepo(d.Image)
	return adapter.NewGated(cfg, d, func() bool {
		return store != nil && store.Get().Approved(d.EngineID) == repo
	})
}

// seedApprovals approves, once, every engine the box already has.
//
// Approval arrived after engines did, and a box that was backing up the night before
// must go on backing up the night after: every descriptor present on the first start of
// a Maison that knows about approval was put there by the deployment, because no store
// app could have offered one before. After that one pass, ApprovalsSeeded is set and
// every engine that appears waits for the owner. A deployment that rebuilds a box later
// re-approves its own engine through the admin socket (`maison backup approve`).
func seedApprovals(store *backupconfig.Store, found []adapter.Descriptor) {
	if store == nil || store.Get().ApprovalsSeeded {
		return
	}
	err := store.Update(func(c *backupconfig.Config) {
		for _, d := range found {
			es := c.Engines[d.EngineID]
			es.Approved = adapter.ImageRepo(d.Image)
			c.Engines[d.EngineID] = es
			log.Printf("backup: %s: approved %s (present before engine approval existed)", d.EngineID, es.Approved)
		}
		c.ApprovalsSeeded = true
	})
	if err != nil {
		log.Printf("backup: could not record the initial engine approvals: %v", err)
	}
}

// rescanEngines registers every descriptor that is new or has changed since it was last
// read, without a restart.
//
// Engines are only ever added or replaced, never removed (backup.Set): an engine app
// that was uninstalled keeps its history in the list, and its reads fail with a reason
// rather than its backups vanishing from the page. A changed descriptor is a new
// provider, which drops the capabilities and status the old one cached.
func (s *Server) rescanEngines() {
	if s.engines == nil {
		return
	}
	s.engineMu.Lock()
	defer s.engineMu.Unlock()

	changed := false
	for _, d := range adapter.Discover(s.cfg) {
		if p, ok := s.engines.Get(d.EngineID); ok {
			if g, ok := p.(gatedEngine); ok && g.Descriptor().Equal(d) {
				continue
			}
		}
		log.Printf("backup: %s: engine descriptor found or changed (image %s)", d.EngineID, d.Image)
		s.engines.Register(newGatedEngine(s.cfg, s.backupConf, d))
		changed = true
	}
	if !changed {
		return
	}
	if s.backupConf != nil {
		applyEngineSettings(s.engines, s.backupConf)
	}
	if s.backupSched != nil {
		s.backupSched.Reload()
	}
}

// errEngineNotFound is an approval for an engine the box does not have.
var errEngineNotFound = errors.New("not found")

// errBinding is an approval refused because the descriptor is not vouched for by the
// app it sits in.
var errBinding = errors.New("binding")

// approveEngine records the owner's approval of an engine's current image.
//
// bind is the check that makes approving from the HTTP API safe even though that API is
// reachable from the shared app network: the descriptor's container must belong to the
// compose project named after the engine — the engine's own app — and must run the image
// the descriptor names. A descriptor any other app wrote into that folder can then only
// name what that app already runs, so approving it trusts nothing beyond having
// installed the app. The admin socket skips it: its caller is the deployment, and the
// deployment may approve an engine whose container it has not started yet.
func (s *Server) approveEngine(ctx context.Context, id string, bind bool) error {
	s.rescanEngines()
	if s.engines == nil || s.backupConf == nil {
		return errors.New("backups unavailable")
	}
	p, ok := s.engines.Get(id)
	if !ok {
		return fmt.Errorf("%w: unknown backup engine: %s", errEngineNotFound, id)
	}
	g, ok := p.(gatedEngine)
	if !ok {
		return nil // local: nothing to approve
	}
	d := g.Descriptor()
	if bind {
		if d.Container == "" {
			return fmt.Errorf("%w: this engine names no container to check, so it can only be enabled by the deployment", errBinding)
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		c, err := engine.New(s.cfg).InspectContainer(cctx, d.Container)
		switch {
		case err != nil:
			return fmt.Errorf("%w: the engine container %s is not running — start the %s app and try again", errBinding, d.Container, id)
		case c.Project != id:
			return fmt.Errorf("%w: the engine container %s belongs to %q, not to the %s app", errBinding, d.Container, c.Project, id)
		case c.Image != d.Image:
			return fmt.Errorf("%w: the engine container %s runs %s, but the engine declares %s", errBinding, d.Container, c.Image, d.Image)
		}
	}
	repo := adapter.ImageRepo(d.Image)
	if s.backupConf.Get().Approved(id) == repo {
		return nil
	}
	if err := s.backupConf.Update(func(c *backupconfig.Config) {
		es := c.Engines[id]
		es.Approved = repo
		c.Engines[id] = es
	}); err != nil {
		return err
	}
	log.Printf("backup: %s: approved %s", id, repo)
	s.afterApprovalChange(p, id)
	return nil
}

// revokeEngine withdraws an approval. The engine stays listed and stops running.
func (s *Server) revokeEngine(id string) error {
	if s.engines == nil || s.backupConf == nil {
		return errors.New("backups unavailable")
	}
	p, ok := s.engines.Get(id)
	if !ok {
		return fmt.Errorf("%w: unknown backup engine: %s", errEngineNotFound, id)
	}
	if _, ok := p.(gatedEngine); !ok {
		return fmt.Errorf("%w: the local engine cannot be disabled", errBinding)
	}
	if err := s.backupConf.Update(func(c *backupconfig.Config) {
		es := c.Engines[id]
		es.Approved = ""
		c.Engines[id] = es
	}); err != nil {
		return err
	}
	log.Printf("backup: %s: approval withdrawn", id)
	s.afterApprovalChange(p, id)
	return nil
}

// afterApprovalChange recomputes where backups go and closes or reopens the incident now
// rather than at the next detector pass.
func (s *Server) afterApprovalChange(p apps.Provider, id string) {
	if inv, ok := p.(statusInvalidator); ok {
		inv.Invalidate()
	}
	applyEngineSettings(s.engines, s.backupConf)
	if s.backupSched != nil {
		s.backupSched.Reload()
	}
	if s.incidents != nil && engineApproved(p) {
		s.incidents.Resolve(incident.KindBackupApproval + ":" + id)
	}
}

// writeApprovalError maps an approval failure to its status.
func writeApprovalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errEngineNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, errBinding):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}
}

// handleApproveEngine is the owner's Enable button.
//
//	200 {status}   approved (or already was)
//	404            no such engine
//	409 {error}    the descriptor is not vouched for by its own app's container
//	503            backups unavailable
func (s *Server) handleApproveEngine(w http.ResponseWriter, r *http.Request) {
	if err := s.approveEngine(r.Context(), chi.URLParam(r, "id"), true); err != nil {
		writeApprovalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "approved"})
}

func (s *Server) handleRevokeEngine(w http.ResponseWriter, r *http.Request) {
	if err := s.revokeEngine(chi.URLParam(r, "id")); err != nil {
		writeApprovalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// handleRescanEngines reads the engine descriptors again. The container watcher already
// does this on every container event; this is the button for when it did not.
func (s *Server) handleRescanEngines(w http.ResponseWriter, r *http.Request) {
	s.rescanEngines()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleAdminApproveEngine is `maison backup approve <engine>`: the deployment vouching
// for an engine it provisioned, with no container check.
func (s *Server) handleAdminApproveEngine(w http.ResponseWriter, r *http.Request) {
	if err := s.approveEngine(r.Context(), chi.URLParam(r, "id"), false); err != nil {
		writeApprovalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "approved"})
}

// usableWriters drops engines that are not approved from a write set. An engine the
// owner ticked and has not enabled is reported as missing instead: a destination the
// user asked for that cannot be written, which is what makes an uninstall refuse
// rather than proceed without the backup they expected.
func usableWriters(set *backup.Set, ids []string) (usable, unusable []string) {
	for _, id := range ids {
		if p, ok := set.Get(id); ok && !engineApproved(p) {
			unusable = append(unusable, id)
			continue
		}
		usable = append(usable, id)
	}
	return usable, unusable
}
