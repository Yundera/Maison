package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yundera/maison/internal/apps"
)

// credentialChangeTimeout bounds a change end to end: hook, recreate, readiness
// wait and, on failure, the revert.
const credentialChangeTimeout = 5 * time.Minute

func credentialIndex(r *http.Request) (int, bool) {
	i, err := strconv.Atoi(chi.URLParam(r, "index"))
	return i, err == nil && i >= 0
}

// handleGenerateCredential returns a fresh password meeting the credential's
// policy. Nothing is written — the operator reviews it and saves it through
// handleChangeCredential. POST + no-store for the reason handleShowCredentials
// gives.
func (s *Server) handleGenerateCredential(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	idx, ok := credentialIndex(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid credential index"})
		return
	}
	v, err := s.apps.GenerateCredential(chi.URLParam(r, "id"), idx)
	if err != nil {
		writeCredentialError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"password": v})
}

type changeCredentialBody struct {
	Password string `json:"password"`
}

// handleChangeCredential replaces a credential's password and applies it to the
// app (see apps.ChangeCredential). The change runs detached from the request: a
// browser that navigates away mid-recreate must not cancel the revert.
func (s *Server) handleChangeCredential(w http.ResponseWriter, r *http.Request) {
	if !s.requireApps(w) {
		return
	}
	idx, ok := credentialIndex(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid credential index"})
		return
	}
	var body changeCredentialBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), credentialChangeTimeout)
	defer cancel()
	err := s.apps.ChangeCredential(ctx, chi.URLParam(r, "id"), idx, body.Password)
	s.broadcastApps()
	if err != nil {
		writeCredentialError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeCredentialError maps a change's error to a status. The messages are
// already redacted by apps.ChangeCredential.
func writeCredentialError(w http.ResponseWriter, err error) {
	var pe *apps.PolicyError
	switch {
	case errors.As(err, &pe):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "rules": pe.Rules})
	case errors.Is(err, os.ErrNotExist):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such credential"})
	case errors.Is(err, apps.ErrNotChangeable):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	case errors.Is(err, apps.ErrAppNotRunning),
		errors.Is(err, apps.ErrBackupInFlight), errors.Is(err, apps.ErrUpdating):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}
