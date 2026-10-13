package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backup"
	"github.com/yundera/maison/internal/backup/adapter"
	"github.com/yundera/maison/internal/backupconfig"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/incident"
)

func declareEngine(t *testing.T, cfg config.Config, id, image string) {
	t.Helper()
	dir := cfg.EngineDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"engineId": id, "image": image, "container": id + "-engine", "hostname": "pcs"})
	if err := os.WriteFile(filepath.Join(dir, adapter.DescriptorFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func approvalServer(t *testing.T, cfg config.Config) *Server {
	t.Helper()
	store := backupconfig.New(filepath.Join(cfg.StateDir(), "backup.json"))
	set := buildEngines(cfg, store)
	return &Server{
		cfg:         cfg,
		engines:     set,
		backupConf:  store,
		backupSched: backup.NewScheduler(cfg, nil, set, store),
		incidents:   incident.New(filepath.Join(cfg.StateDir(), "incidents.json")),
	}
}

func needsApproval(t *testing.T, s *Server, id string) bool {
	t.Helper()
	p, ok := s.engines.Get(id)
	if !ok {
		t.Fatalf("engine %s not registered", id)
	}
	return !engineApproved(p)
}

// The engine a box already had when approval arrived keeps working; one that appears
// afterwards — a store app's — waits for the owner.
func TestEnginesPresentAtFirstBootAreApprovedOnceAndNewOnesWait(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	declareEngine(t, cfg, "kopia", "ghcr.io/yundera/maison-kopia-engine:1.2.0")
	s := approvalServer(t, cfg)

	if needsApproval(t, s, "kopia") {
		t.Fatal("the deployment's engine was not approved on first boot")
	}
	conf := s.backupConf.Get()
	if !conf.ApprovalsSeeded || conf.Approved("kopia") != "ghcr.io/yundera/maison-kopia-engine" {
		t.Fatalf("seed not recorded: %+v", conf)
	}

	// Appears while running: registered by the rescan, gated.
	declareEngine(t, cfg, "restic", "example/restic-engine:1")
	s.rescanEngines()
	if !needsApproval(t, s, "restic") {
		t.Fatal("an engine that appeared after the seed was approved without anyone saying so")
	}

	// A second boot does not seed again.
	s2 := approvalServer(t, cfg)
	if !needsApproval(t, s2, "restic") {
		t.Error("a restart approved the waiting engine")
	}
	if needsApproval(t, s2, "kopia") {
		t.Error("a restart lost the seeded approval")
	}
}

// An update of the engine within its own image keeps the approval; a descriptor that
// starts naming another image does not.
func TestApprovalFollowsTheRepositoryNotTheTag(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	declareEngine(t, cfg, "kopia", "ghcr.io/yundera/maison-kopia-engine:1.2.0")
	s := approvalServer(t, cfg)

	declareEngine(t, cfg, "kopia", "ghcr.io/yundera/maison-kopia-engine:1.3.0")
	s.rescanEngines()
	if needsApproval(t, s, "kopia") {
		t.Error("a new tag of the approved image lost the approval")
	}

	declareEngine(t, cfg, "kopia", "evil.example/not-kopia:1")
	s.rescanEngines()
	if !needsApproval(t, s, "kopia") {
		t.Error("a different image ran under the old approval")
	}
}

// A ticked engine that is not enabled is not written to, and is missing — so an
// uninstall that should land there refuses rather than silently skipping the backup.
func TestATickedUnapprovedEngineIsMissingNotAWriter(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	s := approvalServer(t, cfg) // seeds with nothing
	declareEngine(t, cfg, "kopia", "ghcr.io/yundera/maison-kopia-engine:1.3.0")
	s.rescanEngines()

	yes := true
	c := s.backupConf.Get()
	c.Engines = map[string]backupconfig.EngineSettings{"kopia": {Schedule: &yes, Uninstall: &yes}}
	if err := s.backupConf.Set(c); err != nil {
		t.Fatal(err)
	}
	applyEngineSettings(s.engines, s.backupConf)

	if writesTo(s.engines, apps.TriggerSchedule, "kopia") {
		t.Error("the schedule writes to an engine nobody enabled")
	}
	if m := s.engines.Missing(apps.TriggerUninstall); len(m) != 1 || m[0] != "kopia" {
		t.Errorf("Missing(uninstall) = %v, want [kopia]", m)
	}

	if err := s.approveEngine(t.Context(), "kopia", false); err != nil {
		t.Fatal(err)
	}
	if !writesTo(s.engines, apps.TriggerSchedule, "kopia") {
		t.Error("enabling the engine did not make it a writer")
	}
}

// The HTTP route vouches for the descriptor through its container; with no container to
// inspect it refuses, and an unknown engine is a 404. The admin socket is the deployment
// and skips the check.
func TestApproveRouteStatuses(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	s := approvalServer(t, cfg)
	declareEngine(t, cfg, "kopia", "ghcr.io/yundera/maison-kopia-engine:1.3.0")

	post := func(h http.HandlerFunc, id string) int {
		r := chi.NewRouter()
		r.Post("/e/{id}", h)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("POST", "/e/"+id, nil))
		return rec.Code
	}
	if code := post(s.handleApproveEngine, "nope"); code != http.StatusNotFound {
		t.Errorf("unknown engine: %d, want 404", code)
	}
	// No docker in the test: the container cannot be inspected, so nothing vouches.
	if code := post(s.handleApproveEngine, "kopia"); code != http.StatusConflict {
		t.Errorf("unverifiable container: %d, want 409", code)
	}
	if !needsApproval(t, s, "kopia") {
		t.Fatal("a refused approval was recorded")
	}
	if code := post(s.handleAdminApproveEngine, "kopia"); code != http.StatusOK {
		t.Errorf("admin approve: %d, want 200", code)
	}
	if needsApproval(t, s, "kopia") {
		t.Error("admin approval did not take")
	}
	if code := post(s.handleRevokeEngine, "kopia"); code != http.StatusOK || !needsApproval(t, s, "kopia") {
		t.Errorf("revoke: %d, approved still", code)
	}
	if code := post(s.handleRevokeEngine, apps.EngineLocal); code != http.StatusConflict {
		t.Errorf("revoking local: %d, want 409", code)
	}
}

// A PUT from a page loaded before the approval does not withdraw it.
func TestPutBackupConfigKeepsApprovals(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir()}
	declareEngine(t, cfg, "kopia", "ghcr.io/yundera/maison-kopia-engine:1.2.0")
	s := approvalServer(t, cfg)

	rec := httptest.NewRecorder()
	s.handlePutBackupConfig(rec, httptest.NewRequest("PUT", "/api/backup/config",
		jsonBody(t, map[string]any{"enabled": true, "hour": 3, "engines": map[string]any{"kopia": map[string]any{}}})))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	if needsApproval(t, s, "kopia") {
		t.Error("saving the settings page revoked the approval")
	}
}

func jsonBody(t *testing.T, v any) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}
