package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/yundera/maison/internal/brand"
	"github.com/yundera/maison/internal/feedback"
)

// feedbackState is what the dashboard needs to decide whether to offer the form, and
// what to say on it. The sink's URL and token are deliberately not here: this API has
// no authentication of its own, and the token is what keeps every other container on
// the box's network from writing to the operator.
type feedbackState struct {
	Enabled     bool     `json:"enabled"`
	Operator    string   `json:"operator,omitempty"`
	PrivacyNote string   `json:"privacyNote,omitempty"`
	MaxLength   int      `json:"maxLength,omitempty"`
	SupportURL  string   `json:"supportUrl,omitempty"`
	Categories  []string `json:"categories,omitempty"`
	Version     string   `json:"version,omitempty"`
}

// handleGetFeedback reports whether feedback can be sent from here.
//
// Always 200: "not available" is an answer, not an error, and the dashboard asks on
// every load. A sink that is configured but failing is logged and reported as
// disabled — the user cannot do anything about it, and a form they cannot send is
// the outcome the feature is designed not to have.
func (s *Server) handleGetFeedback(w http.ResponseWriter, r *http.Request) {
	d, err := s.feedback.Descriptor(r.Context())
	if err != nil {
		if !errors.Is(err, feedback.ErrDisabled) {
			log.Printf("feedback: %v", err)
		}
		writeJSON(w, http.StatusOK, feedbackState{Enabled: false})
		return
	}
	writeJSON(w, http.StatusOK, feedbackState{
		Enabled:     true,
		Operator:    d.Operator,
		PrivacyNote: d.PrivacyNote,
		MaxLength:   d.Limit(),
		SupportURL:  d.SupportURL,
		Categories:  feedback.Categories,
		Version:     brand.Version,
	})
}

// feedbackInput is what the form sends. Source and version are Maison's to state,
// not the page's, so they are not accepted from it.
type feedbackInput struct {
	Message  string `json:"message"`
	Category string `json:"category"`
	Page     string `json:"page"`
	AppID    string `json:"appId"`
}

// handleSendFeedback forwards the form to the sink.
func (s *Server) handleSendFeedback(w http.ResponseWriter, r *http.Request) {
	if s.feedback == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "feedback is not configured"})
		return
	}
	var in feedbackInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	err := s.feedback.Send(r.Context(), feedback.Submission{
		Message:  in.Message,
		Category: in.Category,
		Context: feedback.Context{
			Source:  brand.Slug,
			Version: brand.Version,
			Page:    in.Page,
			AppID:   in.AppID,
		},
	})
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
	case errors.Is(err, feedback.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		log.Printf("feedback: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
}
