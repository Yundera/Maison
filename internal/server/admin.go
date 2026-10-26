package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yundera/maison/internal/appstore"
	"github.com/yundera/maison/internal/usersettings"
)

// THE ADMIN SOCKET
//
// The deployment's way in: a unix socket inside the container, served by the same
// process as the dashboard, that the `maison` CLI talks to (cmd/maison/cli.go). A
// self-check script runs `docker exec maison-app maison settings set …`; nothing
// else can reach it.
//
// That is the reason it exists rather than more routes on the HTTP API. The API has
// no authentication of its own — on a PCS it sits behind AppShield, and every app on
// the shared network can still call it directly — so it must never be able to change
// who owns a setting, or write a secret the deployment relies on. A socket that is
// not on the network and not on the /DATA mount is reachable only by someone who
// can already `docker exec` into this container.
//
// It also makes Maison the only writer of its own state: the script never edits
// settings.json behind the running process, so nothing it writes is lost to the
// next save from memory, and a value is validated by the code that will use it. An
// image that does not know a setting says so, with a non-zero exit, in the
// self-check log — the version check SMTP_* and FEEDBACK_* never had.

// startAdmin listens on the admin socket at path. A failure is logged, not fatal: the
// dashboard works without it, and a standalone `go run` may have no writable /run.
func (s *Server) startAdmin(path string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("admin socket: %v (the maison CLI will not be available)", err)
		return
	}
	// A socket file left by a previous run of this container refuses the bind.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("admin socket: removing stale %s: %v", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		log.Printf("admin socket: %v (the maison CLI will not be available)", err)
		return
	}
	if err := os.Chmod(path, 0o600); err != nil {
		log.Printf("admin socket: %v", err)
	}
	srv := &http.Server{Handler: s.adminRouter(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("admin socket: %v", err)
		}
	}()
}

func (s *Server) adminRouter() http.Handler {
	r := chi.NewRouter()
	r.Get("/settings", s.handleAdminGetSettings)
	r.Put("/settings/{name}", s.handleAdminManage)
	r.Delete("/settings/{name}", s.handleAdminRelease)
	r.Put("/secrets/{name}", s.handleAdminSetSecret)
	r.Delete("/secrets/{name}", s.handleAdminDeleteSecret)
	r.Post("/stores/ensure", s.handleAdminEnsureStore)
	r.Post("/backup/engines/{id}/approve", s.handleAdminApproveEngine)
	// The same handlers as the HTTP API, unchanged: the deployment's tools reach them
	// here so that a password typed during its onboarding goes straight from its
	// process to the engine, never through the network. `maison backup …` in the CLI.
	r.Get("/backup/status", s.handleBackupStatus)
	r.Post("/backup/engines/{id}/init", s.handleInitEngine)
	r.Post("/backup/engines/{id}/recover", s.handleRecoverEngine)
	return r
}

// handleAdminGetSettings is the effective settings, secrets masked, owners named.
func (s *Server) handleAdminGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, publicSettings(s.settings.Get()))
}

// handleAdminManage makes a setting the deployment's: PUT /settings/{name}?owner=…
// with the value as the JSON body.
func (s *Server) handleAdminManage(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		adminError(w, http.StatusBadRequest, err)
		return
	}
	before := s.managedValue(name)
	if err := s.settings.Manage(name, r.URL.Query().Get("owner"), raw, s.secrets.Has); err != nil {
		adminError(w, adminStatus(err), err)
		return
	}
	// A deployment re-asserts its settings on every self-check; only a value that
	// actually changed has anything to apply.
	if s.managedValue(name) != before {
		s.afterManagedChange(name)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "managed", "setting": name})
}

// handleAdminRelease hands a setting back to the user, deleting the secrets its
// managed value referenced.
func (s *Server) handleAdminRelease(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	before := s.managedValue(name)
	refs, err := s.settings.Release(name)
	if err != nil {
		adminError(w, adminStatus(err), err)
		return
	}
	for _, ref := range refs {
		if err := s.secrets.Delete(ref); err != nil {
			log.Printf("admin: releasing %s, deleting secret %s: %v", name, ref, err)
		}
	}
	// Releasing a setting that is not managed changes nothing — the deployment does it
	// on every self-check while its feature is off.
	if s.managedValue(name) != before {
		s.afterManagedChange(name)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "released", "setting": name})
}

// handleAdminSetSecret stores a secret; the body is the value, one trailing newline
// (what `echo` and a heredoc add) ignored.
func (s *Server) handleAdminSetSecret(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		adminError(w, http.StatusBadRequest, err)
		return
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if err := s.secrets.Set(chi.URLParam(r, "name"), value); err != nil {
		adminError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "set"})
}

func (s *Server) handleAdminDeleteSecret(w http.ResponseWriter, r *http.Request) {
	if err := s.secrets.Delete(chi.URLParam(r, "name")); err != nil {
		adminError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleAdminEnsureStore adds a store source if it is not already listed.
//
// Stores are the user's (they add and remove them freely); the deployment only
// makes sure its own is there, every self-check. Already present is a no-op — no
// re-download — since this runs every night on every box.
func (s *Server) handleAdminEnsureStore(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		adminError(w, http.StatusServiceUnavailable, errors.New("store unavailable"))
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.URL) == "" {
		adminError(w, http.StatusBadRequest, errors.New("url required"))
		return
	}
	url := appstore.CanonicalURL(body.URL)
	urls := s.store.URLs()
	for _, u := range urls {
		// The same store under another spelling counts as present: a box whose user
		// added it as a clone URL must not get the archive URL listed beside it.
		if appstore.SameStore(u, url) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "present"})
			return
		}
	}
	urls = append(urls, url)
	s.store.SetURLs(urls)
	if err := s.settings.Set(usersettings.Settings{StoreSources: urls}); err != nil {
		adminError(w, http.StatusInternalServerError, err)
		return
	}
	// The catalog follows in the background: the self-check must not wait on a
	// download, and the nightly refresh retries a failure.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.store.Refresh(ctx); err != nil {
			log.Printf("admin: store %s added, refresh: %v", url, err)
		}
	}()
	writeJSON(w, http.StatusOK, map[string]string{"status": "added"})
}

// afterManagedChange applies what a setting change means beyond the stored value.
// Routing is the only one with a side effect: every running app is republished
// under the new setting, as the dashboard's own editor does.
func (s *Server) afterManagedChange(name string) {
	if name == "routing" && s.apps != nil {
		go s.apps.Republish(context.Background())
	}
}

// managedValue is a setting's current value as JSON, for telling a re-assertion
// from a change.
func (s *Server) managedValue(name string) string {
	st := s.settings.Get()
	var v any
	switch name {
	case "routing":
		v = st.Routing
	case "smtp":
		v = st.SMTP
	case "feedback":
		v = st.Feedback
	case "onboarding":
		v = st.Onboarding
	case "capabilities":
		v = st.Capabilities
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func adminStatus(err error) int {
	if errors.Is(err, usersettings.ErrUnknownSetting) {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

func adminError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
