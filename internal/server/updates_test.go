package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/appstore"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/installer"
	"github.com/yundera/maison/internal/live"
)

func rows(rs ...installer.AppUpdate) []installer.AppUpdate { return rs }

func TestRunQueueLeavesStaysUpAppsOutOfUpdateAll(t *testing.T) {
	rs := rows(
		installer.AppUpdate{ID: "jellyfin", State: installer.StateAvailable},
		installer.AppUpdate{ID: "maison", State: installer.StateAvailable, StaysUp: true},
		installer.AppUpdate{ID: "dufs", State: installer.StateCurrent},
		installer.AppUpdate{ID: "legacy", State: installer.StateUntracked},
	)
	q, err := runQueue(rs, nil)
	if err != nil || strings.Join(q, ",") != "jellyfin" {
		t.Errorf("update all = %v, %v; want [jellyfin]", q, err)
	}

	// Named alone, a stays-up app is updated.
	if q, err := runQueue(rs, []string{"maison"}); err != nil || len(q) != 1 {
		t.Errorf("maison alone = %v, %v; want it queued", q, err)
	}
	// Named among others, it is refused rather than silently dropped.
	if _, err := runQueue(rs, []string{"jellyfin", "maison"}); err == nil {
		t.Error("a stays-up app was accepted into a multi-app run")
	}
	// Nothing current or untracked is ever queued.
	if _, err := runQueue(rs, []string{"dufs", "legacy"}); !errors.Is(err, errNothingToRun) {
		t.Errorf("err = %v, want errNothingToRun", err)
	}
}

// A blocked app is never part of "Update all"; named alone it is queued, so the owner
// gets the maintainer's message from the refusal instead of "nothing to update".
func TestRunQueueSkipsBlockedAppsUnlessNamedAlone(t *testing.T) {
	rs := rows(
		installer.AppUpdate{ID: "jellyfin", State: installer.StateAvailable},
		installer.AppUpdate{ID: "odoo", State: installer.StateBlocked},
	)
	if q, err := runQueue(rs, nil); err != nil || strings.Join(q, ",") != "jellyfin" {
		t.Errorf("update all = %v, %v; want [jellyfin]", q, err)
	}
	if q, err := runQueue(rs, []string{"odoo"}); err != nil || strings.Join(q, ",") != "odoo" {
		t.Errorf("odoo alone = %v, %v; want it queued", q, err)
	}
	if q, _ := runQueue(rs, []string{"jellyfin", "odoo"}); strings.Join(q, ",") != "jellyfin" {
		t.Errorf("named among others = %v; want odoo dropped", q)
	}
}

// An update's time inside a run grows by its migrations' own timeouts.
func TestUpdateBudgetGrowsWithMigrations(t *testing.T) {
	s := &Server{}
	s.updates.rows = rows(installer.AppUpdate{ID: "odoo", State: installer.StateAvailable,
		DataPlan: installer.DataPlan{Migrations: []installer.MigrationInfo{{ID: 2, Timeout: time.Hour}, {ID: 3, Timeout: 30 * time.Minute}}}})
	if got, want := s.updateBudget("odoo"), updateTimeout+90*time.Minute; got != want {
		t.Errorf("budget = %s, want %s", got, want)
	}
	if got := s.updateBudget("other"); got != updateTimeout {
		t.Errorf("unknown app budget = %s", got)
	}
}

func testUpdatesServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Config{DataRoot: t.TempDir()}
	return &Server{
		cfg:       cfg,
		hub:       live.NewHub(nil),
		apps:      apps.New(cfg, nil),
		installer: installer.New(cfg, appstore.New(nil, filepath.Join(cfg.DataRoot, "cache")), nil),
	}
}

func waitRunDone(t *testing.T, s *Server) UpdateRun {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.updates.mu.Lock()
		run := s.updates.run
		run.Items = append([]UpdateRunItem{}, run.Items...)
		checking := s.updates.checking != nil
		s.updates.mu.Unlock()
		if !run.Running && !checking {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("run did not finish")
	return UpdateRun{}
}

// One app's failure must not strand the rest of the queue. Neither app has a folder
// here, so both updates fail at their first step — which is enough to prove the run
// reaches the second one and records both.
func TestRunContinuesPastAFailure(t *testing.T) {
	s := testUpdatesServer(t)
	s.updates.rows = rows(
		installer.AppUpdate{ID: "a", State: installer.StateAvailable},
		installer.AppUpdate{ID: "b", State: installer.StateAvailable},
	)
	if err := s.startUpdateRun(nil, installer.UpdateOptions{}); err != nil {
		t.Fatalf("startUpdateRun: %v", err)
	}
	run := waitRunDone(t, s)
	if len(run.Items) != 2 {
		t.Fatalf("items = %+v, want 2", run.Items)
	}
	for _, it := range run.Items {
		if it.Status != runFailed || it.Error == "" {
			t.Errorf("%s: %+v, want failed with its error", it.ID, it)
		}
	}
	if run.Finished.IsZero() {
		t.Error("finished run carries no finish time")
	}
}

func TestSecondRunIsRefusedWhileOneIsGoing(t *testing.T) {
	s := testUpdatesServer(t)
	s.updates.rows = rows(installer.AppUpdate{ID: "a", State: installer.StateAvailable})
	s.updates.run.Running = true
	if err := s.startUpdateRun(nil, installer.UpdateOptions{}); !errors.Is(err, errRunInProgress) {
		t.Errorf("err = %v, want errRunInProgress", err)
	}
}

// Cancelling drops the apps still waiting and leaves the one being updated alone: the
// loop must skip a cancelled item rather than start it.
func TestCancelDropsTheQueueButNotTheCurrentApp(t *testing.T) {
	s := testUpdatesServer(t)
	s.updates.run = UpdateRun{Running: true, Items: []UpdateRunItem{
		{ID: "a", Status: runUpdating},
		{ID: "b", Status: runQueued},
		{ID: "c", Status: runQueued},
	}}
	if err := s.cancelUpdateRun(); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	run := s.updates.run
	if !run.Cancelled || !run.Running {
		t.Errorf("run = %+v, want cancelled and still running until a finishes", run)
	}
	want := []string{runUpdating, runCancelled, runCancelled}
	for i, it := range run.Items {
		if it.Status != want[i] {
			t.Errorf("%s: %s, want %s", it.ID, it.Status, want[i])
		}
	}

	s.updates.run = UpdateRun{}
	if err := s.cancelUpdateRun(); !errors.Is(err, errNoRun) {
		t.Errorf("cancel with nothing running = %v, want errNoRun", err)
	}
}

// The loop itself: an item cancelled before its turn is never updated.
func TestRunSkipsCancelledItems(t *testing.T) {
	s := testUpdatesServer(t)
	s.updates.run = UpdateRun{Running: true, Cancelled: true, Items: []UpdateRunItem{
		{ID: "a", Status: runCancelled},
		{ID: "b", Status: runCancelled},
	}}
	s.runUpdates([]string{"a", "b"}, installer.UpdateOptions{})
	run := waitRunDone(t, s)
	for _, it := range run.Items {
		if it.Status != runCancelled || it.Error != "" {
			t.Errorf("%s: %+v, want left cancelled and untouched", it.ID, it)
		}
	}
}

func TestUpdatesRoutesAreReachable(t *testing.T) {
	h := New(config.Config{DataRoot: t.TempDir()}, fstest.MapFS{})
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/updates/run"},
		{http.MethodPost, "/api/updates/cancel"},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		// Whatever this box can do, the answer is the API's JSON, not the SPA's HTML.
		if !strings.Contains(rec.Header().Get("Content-Type"), "json") || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("%s %s -> %d %q; want a JSON error from the updates handler",
				c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

// Skipping the backup is chosen per app, after that app was refused. A run of
// several — or "update all" — cannot ask for it.
func TestNoBackupRunNeedsExactlyOneApp(t *testing.T) {
	s := &Server{}
	for _, ids := range [][]string{nil, {"a", "b"}} {
		if err := s.startUpdateRun(ids, installer.UpdateOptions{NoBackup: true}); !errors.Is(err, errNoBackupNeedsOneApp) {
			t.Errorf("ids %v: err = %v, want errNoBackupNeedsOneApp", ids, err)
		}
	}
}
