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

func TestRunQueueLeavesSystemAppsOutOfUpdateAll(t *testing.T) {
	rs := rows(
		installer.AppUpdate{ID: "jellyfin", State: installer.StateAvailable},
		installer.AppUpdate{ID: "maison", State: installer.StateAvailable, Protected: true},
		installer.AppUpdate{ID: "dufs", State: installer.StateCurrent},
		installer.AppUpdate{ID: "legacy", State: installer.StateUntracked},
	)
	q, err := runQueue(rs, nil)
	if err != nil || strings.Join(q, ",") != "jellyfin" {
		t.Errorf("update all = %v, %v; want [jellyfin]", q, err)
	}

	// Named alone, a system app is updated.
	if q, err := runQueue(rs, []string{"maison"}); err != nil || len(q) != 1 {
		t.Errorf("maison alone = %v, %v; want it queued", q, err)
	}
	// Named among others, it is refused rather than silently dropped.
	if _, err := runQueue(rs, []string{"jellyfin", "maison"}); err == nil {
		t.Error("a system app was accepted into a multi-app run")
	}
	// Nothing current or untracked is ever queued.
	if _, err := runQueue(rs, []string{"dufs", "legacy"}); !errors.Is(err, errNothingToRun) {
		t.Errorf("err = %v, want errNothingToRun", err)
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
	if err := s.startUpdateRun(nil); err != nil {
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
	if err := s.startUpdateRun(nil); !errors.Is(err, errRunInProgress) {
		t.Errorf("err = %v, want errRunInProgress", err)
	}
}

func TestUpdatesRoutesAreReachable(t *testing.T) {
	h := New(config.Config{DataRoot: t.TempDir()}, fstest.MapFS{})
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/updates/run"},
		{http.MethodGet, "/api/updates/preflight?ids=nope"},
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
