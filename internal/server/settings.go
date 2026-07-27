package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/yundera/maison/internal/appenv"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/domains"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/usersettings"
)

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.settings.Get())
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in usersettings.Settings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := s.settings.Set(in); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.settings.Get())
}

// handlePutDomains replaces the additional domains apps are published on, then
// republishes the running ones onto the new list.
//
// Domains get their own endpoint instead of riding the generic settings PUT
// because that one auto-saves on a keystroke debounce, and this one recreates
// every container on the box. The republish runs in the background: it is a
// compose up per app, the tiles carry their own busy state, and the operator
// shouldn't be staring at a hung request while it happens.
func (s *Server) handlePutDomains(w http.ResponseWriter, r *http.Request) {
	var in []domains.Domain
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	for i := range in {
		in[i].Name = strings.TrimSpace(in[i].Name)
		in[i].Domain = strings.TrimSpace(in[i].Domain)
		if in[i].Name == "" || in[i].Domain == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "each domain needs a name and a domain"})
			return
		}
	}

	cur := s.settings.Get()
	cur.Domains = in
	if err := s.settings.Set(cur); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if s.apps != nil {
		go s.apps.Republish(context.Background())
	}
	writeJSON(w, http.StatusOK, s.settings.Get().Domains)
}

// appEnvBody is the .env.app editor's payload.
//
// The file moves as text, not as a key/value list like an app's own .env
// (handlePutEnv): .env.app's comments are its documentation, and its empty values
// are meaningful, so a round-trip through a map would quietly destroy most of the
// file. See internal/appenv.
type appEnvBody struct {
	Text string `json:"text"`
	// Path is the file's location inside this container, for the editor to show.
	// It is reported rather than described in the UI's translations because it
	// follows STATE_DIR: a deployment that moves the state directory would
	// otherwise be shown a path that does not exist.
	Path string `json:"path"`
	// Ignored are keys the text sets that Maison computes per app anyway
	// (envinject.DerivedKeys) and will overwrite. Reported rather than rejected —
	// the file is the deployment's, and a PCS may well list them for its own
	// reasons; the editor just says they have no effect.
	Ignored []string `json:"ignored"`
}

// newAppEnvBody pairs .env.app's text with its path and the derived keys it sets.
func newAppEnvBody(cfg config.Config, raw []byte) appEnvBody {
	derived := envinject.DerivedKeys()
	var ignored []string
	for _, v := range envinject.ParseEnvFile(raw) {
		if slices.Contains(derived, v.Key) {
			ignored = append(ignored, v.Key)
		}
	}
	return appEnvBody{Text: string(raw), Path: appenv.Path(cfg), Ignored: ignored}
}

// handleGetAppEnv returns the deployment's .env.app as text.
func (s *Server) handleGetAppEnv(w http.ResponseWriter, _ *http.Request) {
	raw, err := appenv.ReadRaw(s.cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, newAppEnvBody(s.cfg, raw))
}

// handlePutAppEnv replaces the deployment's .env.app.
//
// Nothing is restarted: unlike the domains list, which rewrites Caddy labels and so
// has to recreate containers to mean anything, .env.app is read by appenv.Sync on
// each app's next start. Recreating every container on the box because someone
// edited a comment would be a poor trade, and the editor tells the operator the
// change lands on next start instead. Config.AppEnv reads the file live, so the
// next `docker compose up` picks this up with no Maison restart.
func (s *Server) handlePutAppEnv(w http.ResponseWriter, r *http.Request) {
	var in appEnvBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := appenv.WriteRaw(s.cfg, []byte(in.Text)); err != nil {
		// A validation failure is the operator's typo, not a server fault.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, newAppEnvBody(s.cfg, []byte(in.Text)))
}
