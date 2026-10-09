package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/feedback"
)

// sinkBox builds the router against a fake sink that answers the descriptor with
// desc and keeps the last submission in *got.
func sinkBox(t *testing.T, desc string, got *[]byte) http.Handler {
	t.Helper()
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			*got, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if desc == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.WriteString(w, desc)
	}))
	t.Cleanup(sink.Close)
	return New(config.Config{
		DataRoot: t.TempDir(),
		Feedback: feedback.Sink{URL: sink.URL, Token: "tok"},
	}, fstest.MapFS{})
}

func TestABoxWithNoSinkOffersNoFeedback(t *testing.T) {
	h := bareBox(t)
	code, body := call(t, h, http.MethodGet, "/api/feedback", "")
	if code != http.StatusOK || !strings.Contains(body, `"enabled":false`) {
		t.Errorf("GET = %d %s", code, body)
	}
	code, _ = call(t, h, http.MethodPost, "/api/feedback", `{"message":"hi"}`)
	if code != http.StatusNotFound {
		t.Errorf("POST = %d, want 404", code)
	}
}

// The sink decides: one that does not describe itself is the feature switched off,
// even though the deployment configured it.
func TestASinkThatSaysNoHidesTheFeature(t *testing.T) {
	var got []byte
	h := sinkBox(t, "", &got)
	code, body := call(t, h, http.MethodGet, "/api/feedback", "")
	if code != http.StatusOK || !strings.Contains(body, `"enabled":false`) {
		t.Errorf("GET = %d %s", code, body)
	}
}

func TestTheDescriptorReachesTheDashboardButTheTokenDoesNot(t *testing.T) {
	var got []byte
	h := sinkBox(t, `{"operator":"Acme","privacyNote":"we read it"}`, &got)
	code, body := call(t, h, http.MethodGet, "/api/feedback", "")
	if code != http.StatusOK {
		t.Fatalf("GET = %d %s", code, body)
	}
	var st feedbackState
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || st.Operator != "Acme" || st.PrivacyNote != "we read it" || st.MaxLength != feedback.DefaultMaxLength {
		t.Errorf("state = %+v", st)
	}
	if strings.Contains(body, "tok") || strings.Contains(body, "127.0.0.1") {
		t.Errorf("the sink's credentials leaked into the response: %s", body)
	}
}

func TestFeedbackIsForwardedWithMaisonsOwnContext(t *testing.T) {
	var got []byte
	h := sinkBox(t, `{"operator":"Acme"}`, &got)
	// source and version in the body are ignored: they are Maison's to state.
	code, body := call(t, h, http.MethodPost, "/api/feedback",
		`{"message":"love it","category":"idea","page":"/store","appId":"jellyfin","source":"forged"}`)
	if code != http.StatusAccepted {
		t.Fatalf("POST = %d %s", code, body)
	}
	var sub feedback.Submission
	if err := json.Unmarshal(got, &sub); err != nil {
		t.Fatalf("sink got %q: %v", got, err)
	}
	if sub.Message != "love it" || sub.Context.Source != "maison" || sub.Context.AppID != "jellyfin" || sub.Context.Page != "/store" {
		t.Errorf("sink got %+v", sub)
	}
}

func TestAnInvalidSubmissionIsABadRequestNotABadGateway(t *testing.T) {
	var got []byte
	h := sinkBox(t, `{"operator":"Acme"}`, &got)
	for _, b := range []string{`{"message":""}`, `{"message":"x","category":"rant"}`, `not json`} {
		if code, body := call(t, h, http.MethodPost, "/api/feedback", b); code != http.StatusBadRequest {
			t.Errorf("POST %s = %d %s, want 400", b, code, body)
		}
	}
	if got != nil {
		t.Errorf("an invalid submission reached the sink: %s", got)
	}
}
