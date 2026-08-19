package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/yundera/maison/internal/appenv"
	"github.com/yundera/maison/internal/caddyroutes"
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

// domainsView is the domains editor's payload.
//
// It carries more than the list it edits, because the list on its own does not say
// what it is a list *of*: an operator looking at "sslip → ${APP_PUBLIC_IP_DASH}.sslip.io"
// cannot tell whether it is an addition to the deployment's domain or a replacement
// of it. So the primary domain travels with it — as the token a compose label is
// actually written with, and as the host that token currently resolves to.
type domainsView struct {
	// PrimaryToken is the placeholder a store app's own route is templated with
	// (`${APP_DOMAIN}`). Shown read-only: it comes from the app's compose, which
	// Maison never edits.
	PrimaryToken string `json:"primaryToken"`

	// PrimaryHost is what that token resolves to on this box (.env.app's APP_DOMAIN),
	// or "" on a deployment with no domain — in which case apps have no reachable
	// web address at all and the editor says so.
	PrimaryHost string `json:"primaryHost"`

	// Domains is the operator's list of additional domains.
	Domains []domains.Domain `json:"domains"`

	// Vars are the .env.app variables a domain template may interpolate, so the
	// editor can preview the host a template resolves to before it is saved onto
	// every app.
	//
	// Deliberately not all of .env.app: that file also carries the seeded admin
	// password, which has no business in this endpoint. Only the routing family
	// travels.
	Vars map[string]string `json:"vars"`
}

// routingVarPrefixes selects the .env.app keys that are about *where an app is
// reached*, which are the only ones a domain template has any use for. See
// internal/appenv's default file: they are one commented section of it.
var routingVarPrefixes = []string{"APP_DOMAIN", "APP_PUBLIC_IP", "domain"}

// domainsView builds the editor's payload from the live settings and .env.app.
//
// Both halves come from one read of .env.app — the same source Config.AppDomain
// reads, but taken here directly, like the .env.app editor beside it does. That
// keeps the shown primary host and the shown variables from ever disagreeing.
func (s *Server) domainsView() domainsView {
	env := appenv.Load(s.cfg)
	vars := map[string]string{}
	for k, v := range env {
		for _, p := range routingVarPrefixes {
			if strings.HasPrefix(k, p) {
				vars[k] = v
				break
			}
		}
	}
	return domainsView{
		PrimaryToken: caddyroutes.PrimaryToken,
		PrimaryHost:  env["APP_DOMAIN"],
		Domains:      s.settings.Get().Domains,
		Vars:         vars,
	}
}

// handleGetDomains returns the domains editor's payload: the additional domains
// plus the primary domain they are added to.
func (s *Server) handleGetDomains(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.domainsView())
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
	seen := map[string]bool{}
	for i := range in {
		in[i].Name = strings.TrimSpace(in[i].Name)
		in[i].Domain = strings.TrimSpace(in[i].Domain)
		if in[i].Name == "" || in[i].Domain == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "each domain needs a name and a host"})
			return
		}
		if strings.ContainsAny(in[i].Domain, " \t") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a host cannot contain spaces: " + in[i].Domain})
			return
		}
		// The editor keys its rows on the host, generation skips a host that is
		// already routed, and two rows on one host would fight over the same
		// generated route. One entry per host.
		if seen[in[i].Domain] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "that host is already in the list: " + in[i].Domain})
			return
		}
		seen[in[i].Domain] = true

		// Directives come from a key/value editor, which is free to hand back the
		// blank row someone opened and did not fill in. Drop those rather than
		// writing an empty Caddy sub-directive into every app's override.
		clean := map[string]string{}
		for k, v := range in[i].Directives {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			clean[k] = strings.TrimSpace(v)
		}
		if len(clean) == 0 {
			clean = nil
		}
		in[i].Directives = clean
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
	writeJSON(w, http.StatusOK, s.domainsView())
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
