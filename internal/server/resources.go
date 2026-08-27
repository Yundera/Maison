package server

import (
	"net/http"
	"strconv"
	"time"

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
		Bytes:       s.history.Bytes(),
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
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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
