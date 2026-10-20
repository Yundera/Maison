package server

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yundera/maison/internal/diskuse"
	"github.com/yundera/maison/internal/metrics"
)

// defaultHistoryPoints is how many buckets a history request is reduced to when
// the caller does not say. It is a screen's worth: sending the raw 43,200
// one-minute records for a thirty-day range would be over a megabyte of JSON to
// draw a line a thousand pixels wide.
const defaultHistoryPoints = 500

// maxHistoryPoints caps what a caller can ask for.
const maxHistoryPoints = 2000

// historyResponse is what the graphs read.
type historyResponse struct {
	// From and To are the window actually served, in unix milliseconds.
	From int64 `json:"from"`
	To   int64 `json:"to"`

	// StepMs is the width of one bucket. The client needs it to tell a gap in the
	// recording from two adjacent points: empty buckets are omitted rather than
	// sent as zeroes, so the line has to be broken wherever two points are further
	// apart than this.
	StepMs int64 `json:"step_ms"`

	Spans []metrics.Span `json:"spans"`

	// Enabled, RetentionMs and Bytes describe the recorder itself, which the
	// settings card beside the graphs shows: whether it is running, how far back it
	// can reach, and what it currently costs on disk.
	Enabled     bool  `json:"enabled"`
	RetentionMs int64 `json:"retention_ms"`
	Bytes       int64 `json:"bytes"`
}

// handleHistory serves the recorded history for a window, downsampled server-side.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "history unavailable"})
		return
	}
	q := r.URL.Query()
	to := parseMillis(q.Get("to"), time.Now())
	from := parseMillis(q.Get("from"), to.Add(-time.Hour))
	if !from.Before(to) {
		from = to.Add(-time.Hour)
	}
	points := defaultHistoryPoints
	if n, err := strconv.Atoi(q.Get("points")); err == nil && n > 0 {
		points = min(n, maxHistoryPoints)
	}

	resp := historyResponse{
		From:        from.UnixMilli(),
		To:          to.UnixMilli(),
		StepMs:      metrics.Step(from, to, points).Milliseconds(),
		Spans:       []metrics.Span{},
		Enabled:     s.settings.HistoryEnabled(),
		RetentionMs: s.history.Retention().Milliseconds(),
		Bytes:       s.history.Bytes() + s.appHistoryBytes(),
	}

	recs, err := s.history.Read(from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	resp.Spans = metrics.Downsample(recs, from, to, points)

	// A bucket narrower than the recording resolution would render as a comb of
	// one-point spans separated by holes. Report the resolution instead, so the
	// client's gap detection uses the real spacing.
	if step := s.history.Step().Milliseconds(); resp.StepMs < step {
		resp.StepMs = step
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDeleteHistory discards the recording. The sampler recreates the file on
// its next tick if history is still switched on.
func (s *Server) handleDeleteHistory(w http.ResponseWriter, _ *http.Request) {
	if s.history == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "history unavailable"})
		return
	}
	if err := s.history.Delete(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if s.appHistory != nil {
		if err := s.appHistory.DeleteAll(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) appHistoryBytes() int64 {
	if s.appHistory == nil {
		return 0
	}
	return s.appHistory.Bytes()
}

// appHistoryResponse is what an app's Stats tab graphs read: the same window and
// gap contract as historyResponse, plus the app's daily folder sizes.
type appHistoryResponse struct {
	From   int64             `json:"from"`
	To     int64             `json:"to"`
	StepMs int64             `json:"step_ms"`
	Spans  []metrics.AppSpan `json:"spans"`
	Disk   []diskPoint       `json:"disk"`

	// Enabled is the history switch; Recording is whether usage (not just folder
	// size) can be recorded on this box at all. Reason says why not, in words the
	// tab can show as they are.
	Enabled   bool   `json:"enabled"`
	Recording bool   `json:"recording"`
	Reason    string `json:"reason,omitempty"`
}

type diskPoint struct {
	At    time.Time `json:"at"`
	Bytes int64     `json:"bytes"`
}

// handleAppHistory serves one app's recorded usage for a window, downsampled
// server-side like the host history.
func (s *Server) handleAppHistory(w http.ResponseWriter, r *http.Request) {
	if s.appHistory == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "history unavailable"})
		return
	}
	q := r.URL.Query()
	to := parseMillis(q.Get("to"), time.Now())
	from := parseMillis(q.Get("from"), to.Add(-time.Hour))
	if !from.Before(to) {
		from = to.Add(-time.Hour)
	}
	points := defaultHistoryPoints
	if n, err := strconv.Atoi(q.Get("points")); err == nil && n > 0 {
		points = min(n, maxHistoryPoints)
	}

	resp := appHistoryResponse{
		From:      from.UnixMilli(),
		To:        to.UnixMilli(),
		StepMs:    max(metrics.Step(from, to, points), s.appHistory.Step()).Milliseconds(),
		Spans:     []metrics.AppSpan{},
		Disk:      []diskPoint{},
		Enabled:   s.settings.HistoryEnabled(),
		Recording: s.appHistory.Recording(),
	}
	switch {
	case !resp.Enabled:
		resp.Reason = "History recording is switched off (Settings › Resources)."
	case !resp.Recording:
		resp.Reason = "This box does not expose container cgroups to Maison, so only the daily folder size is recorded."
	}

	usage, disk, err := s.appHistory.Read(chi.URLParam(r, "id"), from, to)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	resp.Spans = metrics.DownsampleApp(usage, from, to, points)
	for _, d := range disk {
		resp.Disk = append(resp.Disk, diskPoint{At: d.At, Bytes: int64(d.Bytes)})
	}
	writeJSON(w, http.StatusOK, resp)
}

// appSizesEvery is how often each app's data folder is measured for its history.
const appSizesEvery = 24 * time.Hour

// recordAppSizes measures every app's data folder once a day and records it into
// the app's history, then drops the history of apps that are no longer installed.
//
// Measuring a folder is a walk — the only way to know what it holds — so it runs
// once a day over the apps' folder only, not over the whole data root, and never on
// the minute tick. When it last ran is kept on disk, so a restart does not re-walk;
// the first walk after boot waits ten minutes so it does not compete with the stack
// coming up.
func (s *Server) recordAppSizes(ctx context.Context) {
	scanner := diskuse.New(s.cfg.AppsDir(), "", "")
	marker := filepath.Join(s.cfg.StateDir(), "metrics", "app-sizes.at")

	run := func() {
		if !s.settings.HistoryEnabled() {
			return
		}
		if fi, err := os.Stat(marker); err == nil && time.Since(fi.ModTime()) < appSizesEvery {
			return
		}
		res := scanner.Scan()
		if res.Status != diskuse.StatusDone {
			log.Printf("metrics: app sizes: %s %s", res.Status, res.Error)
			return
		}
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		list, err := s.apps.List(lctx)
		cancel()
		if err != nil {
			log.Printf("metrics: app sizes: %v", err)
			return
		}
		installed := make(map[string]bool, len(list))
		for _, a := range list {
			installed[a.ID] = true
		}
		sizes := map[string]int64{}
		for _, e := range res.Top {
			if installed[e.Name] {
				sizes[e.Name] = e.Bytes
			}
		}
		s.appHistory.RecordDisk(sizes, time.Now())
		s.appHistory.Prune(installed)
		if err := os.MkdirAll(filepath.Dir(marker), 0o755); err == nil {
			_ = os.WriteFile(marker, nil, 0o644) // its mtime is the record
		}
	}

	select {
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Minute):
	}
	run()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// handleResources answers the same payload the "resources" live channel pushes, as
// a one-shot for a caller that does not hold a subscription open. The page itself
// subscribes rather than calling this.
//
// It shares the Detailer's rate baseline with the live channel, so calling it while
// the page is open makes that one sample cover a shorter window than the 2s cadence
// — the rates stay correct for the window they describe, they just jitter. Worth
// knowing before wiring this into anything that polls.
func (s *Server) handleResources(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.detailer.Sample())
}

// resourcesSnapshot produces the host breakdown for the live channel.
func (s *Server) resourcesSnapshot() any { return s.detailer.Sample() }

// handleBenchState reports both benchmarks without starting either.
func (s *Server) handleBenchState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.bench.State())
}

// handleDiskBench starts a disk benchmark. It answers immediately with the state
// as of that moment — the run itself takes tens of seconds, and holding the
// request open for it would be at the mercy of every proxy in front of Maison.
// The client polls GET /api/system/bench.
func (s *Server) handleDiskBench(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusAccepted, s.bench.StartDisk())
}

// handleNetworkBench starts a link benchmark. Same asynchronous contract as the
// disk one.
func (s *Server) handleNetworkBench(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusAccepted, s.bench.StartNetwork())
}

func parseMillis(s string, fallback time.Time) time.Time {
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil || ms <= 0 {
		return fallback
	}
	return time.UnixMilli(ms)
}
