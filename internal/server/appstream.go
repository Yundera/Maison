package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/coder/websocket"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/go-chi/chi/v5"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/appstats"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/overrideform"
)

type configBody struct {
	Override string `json:"override"`
}

type tipsBody struct {
	Tips string `json:"tips"`
}

type webuiBody struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   string `json:"port"`
	Path   string `json:"path"`
}

type envBody struct {
	Vars []envinject.Var `json:"vars"`
}

// handleGetEnv returns the app's .env as an ordered key/value list.
func (s *Server) handleGetEnv(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	vars, err := s.apps.GetEnv(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, vars)
}

// handlePutEnv rewrites the app's .env to the posted list and recreates the
// stack (compose only reads .env at up time). A malformed entry — bad key,
// duplicate, multi-line value — is rejected before anything is written.
func (s *Server) handlePutEnv(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	var body envBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := envinject.ValidateVars(body.Vars); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.apps.SetEnv(r.Context(), chi.URLParam(r, "id"), body.Vars); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.broadcastApps()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePutWebUI writes the app's opening-URL (webui-*) fields into its override
// and recreates it — a friendly shortcut for editing those keys by hand.
func (s *Server) handlePutWebUI(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	var body webuiBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	err := s.apps.SetWebUI(r.Context(), chi.URLParam(r, "id"), apps.WebUI{
		Scheme: body.Scheme, Host: body.Host, Port: body.Port, Path: body.Path,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.broadcastApps()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleAppServices lists a stack's services with their live state and health —
// so the logs/stats views can reflect a multi-service app.
func (s *Server) handleAppServices(w http.ResponseWriter, r *http.Request) {
	if s.dx == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "docker unavailable"})
		return
	}
	svcs, err := s.dx.ProjectServices(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, svcs)
}

// streamTarget resolves the container a logs/stats stream should attach to: the
// requested service, or the project's first container when none is given.
func (s *Server) streamTarget(ctx context.Context, project, service string) (string, error) {
	if service != "" {
		return s.dx.ContainerIDForService(ctx, project, service)
	}
	return s.dx.FirstContainerID(ctx, project)
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	cfg, err := s.apps.GetConfig(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	var body configBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := s.apps.SetConfig(r.Context(), chi.URLParam(r, "id"), body.Override); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.broadcastApps()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleGetOverrideForm returns the friendly, field-by-field view of the app's
// override — every field carrying both the store's value and the user's, so the
// form can show what is inherited and what was changed.
func (s *Server) handleGetOverrideForm(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	form, err := s.apps.GetOverrideForm(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, form)
}

// handlePutOverrideForm patches the override with the form's values and recreates
// the app.
func (s *Server) handlePutOverrideForm(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	var form overrideform.Form
	if err := json.NewDecoder(r.Body).Decode(&form); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := s.apps.SetOverrideForm(r.Context(), chi.URLParam(r, "id"), &form); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.broadcastApps()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleValidateOverride parses a candidate override against the app's base
// compose without applying it. A rejected override is a 200 with the parse error
// in the body, not an HTTP error: the request itself succeeded, and the answer
// the editor wants is Compose's own message.
func (s *Server) handleValidateOverride(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	var body configBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := s.apps.ValidateOverride(r.Context(), chi.URLParam(r, "id"), body.Override); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleEffectiveConfig returns the project as Compose resolves it — base plus
// override, merged and interpolated.
func (s *Server) handleEffectiveConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	out, err := s.apps.EffectiveConfig(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"config": out})
}

// handlePutTips persists the app's editable tips into its override. It writes
// only the override file — no Docker recreate and no apps broadcast, since tips
// are pure metadata that never affect the tile.
func (s *Server) handlePutTips(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	var body tipsBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := s.apps.SetTips(chi.URLParam(r, "id"), body.Tips); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleRenderTips returns the app's tips with ${VAR} references resolved from
// its base vars and .env — the rendered preview shown from the tile menu.
func (s *Server) handleRenderTips(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	tips, err := s.apps.RenderedTips(chi.URLParam(r, "id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"tips": tips})
}

// handleAppLogs streams a managed/unmanaged app's container logs over a WebSocket.
func (s *Server) handleAppLogs(w http.ResponseWriter, r *http.Request) {
	if s.dx == nil {
		http.Error(w, "docker unavailable", http.StatusServiceUnavailable)
		return
	}
	cid, err := s.streamTarget(r.Context(), chi.URLParam(r, "id"), r.URL.Query().Get("service"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	ctx := conn.CloseRead(r.Context()) // cancels when the client disconnects

	reader, err := s.dx.ContainerLogs(ctx, cid, "300", true)
	if err != nil {
		return
	}
	defer reader.Close()

	// Demux Docker's stdout/stderr multiplexed stream into plain lines.
	pr, pw := io.Pipe()
	go func() {
		_, _ = stdcopy.StdCopy(pw, pw, reader)
		_ = pw.Close()
	}()

	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if err := conn.Write(ctx, websocket.MessageText, sc.Bytes()); err != nil {
			return
		}
	}
}

// handleAppStats streams computed CPU%/memory for an app's main container.
func (s *Server) handleAppStats(w http.ResponseWriter, r *http.Request) {
	if s.dx == nil {
		http.Error(w, "docker unavailable", http.StatusServiceUnavailable)
		return
	}
	cid, err := s.streamTarget(r.Context(), chi.URLParam(r, "id"), r.URL.Query().Get("service"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	ctx := conn.CloseRead(r.Context())

	body, err := s.dx.ContainerStatsStream(ctx, cid)
	if err != nil {
		return
	}
	defer body.Close()

	dec := json.NewDecoder(body)
	for {
		var v container.StatsResponse
		if err := dec.Decode(&v); err != nil {
			return
		}
		out := statsFrame{
			CPUPercent: appstats.CPUPercent(v),
			MemUsage:   appstats.MemUsage(v),
			MemLimit:   v.MemoryStats.Limit,
			Health:     s.dx.ContainerHealth(ctx, cid),
		}
		raw, _ := json.Marshal(out)
		if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
			return
		}
	}
}

type statsFrame struct {
	CPUPercent float64 `json:"cpu_percent"`
	MemUsage   uint64  `json:"mem_usage"`
	MemLimit   uint64  `json:"mem_limit"`
	Health     string  `json:"health"` // "", starting, healthy, unhealthy
}
