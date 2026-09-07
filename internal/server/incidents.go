package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yundera/maison/internal/incident"
	"github.com/yundera/maison/internal/live"
)

// The incident register's HTTP surface and its live channel.
//
// Like the rest of this API it is unauthenticated, and deliberately so: the gate in
// front of Maison is the security boundary (see onboarding.go), and host dispatch in
// rootHandler already means only the dashboard host reaches /api at all. The inbound
// POST below inherits that — it is reachable from the pcs network, which is where the
// other PCS components that will use it live.

// deliverInterval is the coalescing window: how long a transition waits for company
// before it is mailed.
//
// Two minutes because one root cause takes a moment to fan out — a disk fills, the
// next check finds four apps unhealthy — and an alert that left immediately would beat
// its own consequences into the inbox as separate messages. Nobody acts on an email
// inside two minutes, so the delay costs nothing and the grouping is worth a lot.
const deliverInterval = 2 * time.Minute

// deliverIncidents is the only thing that turns queued transitions into mail. The
// queue is on disk, so a pass missed across a restart is picked up by the next one.
func (s *Server) deliverIncidents(ctx context.Context) {
	t := time.NewTicker(deliverInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.incidents.Deliver()
		}
	}
}

func (s *Server) broadcastIncidents() {
	s.hub.BroadcastLazy(live.ChannelIncidents, s.incidentsSnapshot)
}

func (s *Server) incidentsSnapshot() any {
	if s.incidents == nil {
		return incident.Snapshot{Open: []incident.Incident{}, Recent: []incident.Incident{}}
	}
	return s.incidents.Snapshot()
}

func (s *Server) handleGetIncidents(w http.ResponseWriter, _ *http.Request) {
	if s.incidents == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "incidents unavailable"})
		return
	}
	snap := s.incidents.Snapshot()
	snap.Open, snap.Recent = orEmpty(snap.Open), orEmpty(snap.Recent)
	writeJSON(w, http.StatusOK, snap)
}

// handleReportIncident is the door for everything on the box that is not Maison.
//
// A PCS runs several things that can fail silently and have nowhere to say so — the
// nightly self-check in template-root, certificate renewal in mesh-router-caddy, the
// auth stack. Without this each of them grows its own mailer, its own idea of how
// often to repeat itself, and its own relay configuration to get wrong. With it they
// assert a condition and inherit the dedup, the coalescing and the mute switch that
// are already tested here.
//
// The reporter owns the ID, and that is the contract: the same problem must always
// carry the same ID or it will be announced twice. Re-posting an open incident is the
// expected steady state for a caller on a cron, and costs nothing.
func (s *Server) handleReportIncident(w http.ResponseWriter, r *http.Request) {
	if s.incidents == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "incidents unavailable"})
		return
	}
	var in incident.Report
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if in.ID == "" || in.Kind == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "an incident needs an id and a kind"})
		return
	}
	s.incidents.Report(in)
	writeJSON(w, http.StatusOK, map[string]string{"status": "reported"})
}

// handleResolveIncident clears one. The counterpart to the POST above: a caller that
// reports a condition is the only thing that knows when it has gone.
func (s *Server) handleResolveIncident(w http.ResponseWriter, r *http.Request) {
	if s.incidents == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "incidents unavailable"})
		return
	}
	s.incidents.Resolve(chi.URLParam(r, "id"))
	writeJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
}

func (s *Server) handleAckIncident(w http.ResponseWriter, r *http.Request) {
	if s.incidents == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "incidents unavailable"})
		return
	}
	if err := s.incidents.Ack(chi.URLParam(r, "id")); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.incidents.Snapshot())
}

// handleMuteKind turns a whole class of incident's mail on or off. It still gets
// recorded and still shows on the settings page — see incident.Store.MuteKind.
func (s *Server) handleMuteKind(w http.ResponseWriter, r *http.Request) {
	if s.incidents == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "incidents unavailable"})
		return
	}
	var in struct {
		Kind  string `json:"kind"`
		Muted bool   `json:"muted"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := s.incidents.MuteKind(in.Kind, in.Muted); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.incidents.Snapshot())
}

// handleTestNotification proves the relay end to end.
//
// It sends a real alert through the real composer rather than dialling the server and
// hanging up, and it waits for the result so the SMTP error can be put in front of the
// person who is standing there trying to configure it — which is the whole reason the
// button exists. A test that reported "sent" because it had queued something would be
// worse than no button at all.
func (s *Server) handleTestNotification(w http.ResponseWriter, _ *http.Request) {
	if s.incidents == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "incidents unavailable"})
		return
	}
	if !s.settings.EffectiveSMTP(s.cfg.ProvisionedSMTP()).Configured() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no mail server configured"})
		return
	}
	if err := s.incidents.Test(); err != nil {
		// 502: the request was fine and Maison is fine; the thing on the other end of
		// the relay is not. Same reasoning as handleEmailKey.
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}
