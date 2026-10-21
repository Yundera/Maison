package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yundera/maison/internal/appenv"
	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/routes"
	"github.com/yundera/maison/internal/routes/dialect"
	"github.com/yundera/maison/internal/routing"
	"github.com/yundera/maison/internal/stackup"
	"github.com/yundera/maison/internal/usersettings"
)

// handleGetSettings returns the settings, with every secret masked (publicSettings)
// and the deployment-owned settings named in `managed`.
func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, publicSettings(s.settings.Get()))
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in usersettings.Settings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := s.prepareUserEdit(&in); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := s.settings.Set(in); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, publicSettings(s.settings.Get()))
}

// routingView is the routing editor's payload.
//
// It carries more than the setting it edits, because the setting on its own does
// not say what it does: the effective setting (what the box runs on, defaults
// filled in), the .env.app variables a domain template may interpolate, and a
// preview — the override and the addresses Maison would generate for a sample app,
// rendered by the same code that renders every real app, so the editor never
// carries a copy of the generator.
type routingView struct {
	// Setting is the stored setting, nil when the box runs on its default.
	Setting *routing.Setting `json:"setting"`
	// Effective is what the box runs on (routing.Effective).
	Effective routing.Setting `json:"effective"`
	// ManagedBy names the deployment that owns the setting, when one does: the
	// editor is then read-only (PUT answers 409).
	ManagedBy string `json:"managedBy,omitempty"`
	// Dialects are the label grammars this build carries.
	Dialects []string `json:"dialects"`
	// Vars are the .env.app variables a domain template may interpolate.
	//
	// Deliberately not all of .env.app: that file also carries the seeded admin
	// password, which has no business in this endpoint. Only the routing family
	// travels.
	Vars map[string]string `json:"vars"`
	// SeenHost is the address the dashboard was first opened at — what a ports
	// setting without url-host uses.
	SeenHost string `json:"seenHost,omitempty"`
	// PublicWarning is a ports setting publishing on every address of a box that
	// has a public IP: every app is then on the internet.
	PublicWarning bool `json:"publicWarning"`
	// Preview is what the effective setting generates for a sample app.
	Preview routingPreview `json:"preview"`
}

// routingPreview is the generator's output for previewApp.
type routingPreview struct {
	App      string            `json:"app"`
	Override string            `json:"override"`
	Routes   []routing.Entry   `json:"routes"`
	Vars     map[string]string `json:"vars"`
	Error    string            `json:"error,omitempty"`
}

// previewApp is the sample the editor previews on: one service, one route, the
// shape of most of the store.
const previewApp = "myapp"

const previewCompose = `name: myapp
services:
  myapp:
    image: example/myapp:1.0
    expose: ["80"]
x-compose-app:
  routes:
    - upstream-port: 80
`

// routingVarPrefixes selects the .env.app keys that are about *where an app is
// reached*, which are the only ones a domain template has any use for. See
// internal/appenv's default file: they are one commented section of it.
var routingVarPrefixes = []string{"APP_DOMAIN", "APP_PUBLIC_IP", "APP_NET", "domain"}

// routingView builds the editor's payload for setting draft (nil = the stored one).
func (s *Server) routingView(draft *routing.Setting) routingView {
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
	cur := s.settings.Get()
	stored := cur.Routing
	if draft != nil {
		stored = draft
	}
	eff := routing.Effective(stored, env, cur.SeenHost)
	owner, _ := cur.IsManaged("routing")
	return routingView{
		Setting:       cur.Routing,
		Effective:     eff,
		ManagedBy:     owner,
		Dialects:      dialect.Names(),
		Vars:          vars,
		SeenHost:      cur.SeenHost,
		PublicWarning: eff.Renderer == routing.Ports && eff.Bind == routing.DefaultBind && env["APP_PUBLIC_IP"] != "",
		Preview:       preview(eff, env),
	}
}

// preview runs the generator on previewCompose under setting eff.
func preview(eff routing.Setting, env map[string]string) routingPreview {
	resolve := routing.EnvResolver(env)
	rts := []routing.Route{{Name: previewApp, Service: previewApp, Port: 80, Scheme: "http", Path: "/"}}
	entries := routing.Table(rts, eff, resolve, func(routing.Route) int { return eff.PortRange[0] })
	d, _ := dialect.Get(eff.Dialect)
	out, err := routes.Sync([]byte(previewCompose), nil, routes.Plan{
		Renderer: eff.Renderer, Dialect: d, Domains: eff.Domains, Entries: entries,
		Network: "pcs", NetworkRef: stackup.AppNetRef,
	}, resolve)
	p := routingPreview{App: previewApp, Override: string(out), Routes: entries, Vars: routing.Vars(entries, eff)}
	if err != nil {
		p.Error = err.Error()
	}
	return p
}

// handleGetRouting returns the routing editor's payload.
func (s *Server) handleGetRouting(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.routingView(nil))
}

// handlePreviewRouting renders the preview for a setting being edited, without
// saving it.
func (s *Server) handlePreviewRouting(w http.ResponseWriter, r *http.Request) {
	var in routing.Setting
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	n, err := routing.Normalize(in)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.routingView(&n))
}

// handlePutRouting replaces the routing setting — `null` goes back to the
// deployment's default — then republishes the running apps under it.
//
// Routing gets its own endpoint instead of riding the generic settings PUT
// because that one auto-saves on a keystroke debounce, and this one recreates
// every container on the box. The republish runs in the background: it is a
// compose up per app, the tiles carry their own busy state, and the operator
// shouldn't be staring at a hung request while it happens.
func (s *Server) handlePutRouting(w http.ResponseWriter, r *http.Request) {
	if owner, managed := s.settings.Get().IsManaged("routing"); managed {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "routing is managed by " + owner})
		return
	}
	var in *routing.Setting
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if in != nil {
		n, err := routing.Normalize(*in)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		in = &n
	}
	if err := s.settings.SetRouting(in); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, usersettings.ErrManaged) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	if s.apps != nil {
		go s.apps.Republish(context.Background())
	}
	writeJSON(w, http.StatusOK, s.routingView(nil))
}

// handleRoutes is every app's route table: what an operator's own router reads
// when the box runs the external renderer (the same content as <state>/routes.json).
func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	out := map[string][]routing.Entry{}
	if s.apps != nil {
		for _, id := range s.apps.ManagedIDs() {
			if t := stackup.Table(s.cfg, id, filepath.Join(s.cfg.AppsDir(), id)); len(t) > 0 {
				out[id] = t
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"renderer": s.cfg.Routing().Renderer, "apps": out})
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
