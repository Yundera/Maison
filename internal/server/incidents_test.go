package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yundera/maison/internal/config"
)

// bareBox builds the whole router on a scratch tree, with no Docker and no apps —
// the same shape every other test in this package uses. Which handler answered is
// then identifiable from the error it produces.
func bareBox(t *testing.T) http.Handler {
	t.Helper()
	return New(config.Config{DataRoot: t.TempDir()}, fstest.MapFS{})
}

func call(t *testing.T, h http.Handler, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// The register is not a Docker feature. A box whose daemon is unreachable can still
// fill its disk, and that is exactly when someone needs to be told.
func TestTheRegisterWorksOnABoxWithNoDocker(t *testing.T) {
	h := bareBox(t)
	code, body := call(t, h, http.MethodGet, "/api/incidents", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/incidents = %d, want 200: %s", code, body)
	}
	var got struct {
		Open   []map[string]any `json:"open"`
		Recent []map[string]any `json:"recent"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("payload does not decode: %v (%s)", err, body)
	}
	// Not null. Encoding "no incidents" as null makes the field's type depend on its
	// length, and the client that forgets the null check fails at the first list
	// operation instead of rendering nothing. See orEmpty.
	if !strings.Contains(body, `"open":[]`) || !strings.Contains(body, `"recent":[]`) {
		t.Errorf("empty lists should marshal as [], got %s", body)
	}
}

// The round trip an external reporter makes: assert a condition, see it, clear it.
func TestAnOutsideReporterCanOpenAndClearAnIncident(t *testing.T) {
	h := bareBox(t)

	code, body := call(t, h, http.MethodPost, "/api/incidents",
		`{"id":"cert.expiring:nsl.sh","kind":"cert.expiring","severity":"warning","title":"Certificate expires in 3 days"}`)
	if code != http.StatusOK {
		t.Fatalf("POST /api/incidents = %d: %s", code, body)
	}

	_, body = call(t, h, http.MethodGet, "/api/incidents", "")
	if !strings.Contains(body, "Certificate expires in 3 days") {
		t.Fatalf("the reported incident is not in the register: %s", body)
	}

	// Re-posting the same ID is the steady state for a caller on a cron: it must not
	// open a second record.
	call(t, h, http.MethodPost, "/api/incidents",
		`{"id":"cert.expiring:nsl.sh","kind":"cert.expiring","title":"Certificate expires in 2 days"}`)
	var snap struct {
		Open []map[string]any `json:"open"`
	}
	_, body = call(t, h, http.MethodGet, "/api/incidents", "")
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Open) != 1 {
		t.Errorf("re-reporting one condition opened %d records", len(snap.Open))
	}

	code, body = call(t, h, http.MethodDelete, "/api/incidents/cert.expiring:nsl.sh", "")
	if code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", code, body)
	}
	_, body = call(t, h, http.MethodGet, "/api/incidents", "")
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Open) != 0 {
		t.Errorf("the incident should be closed, %d still open", len(snap.Open))
	}
}

// An incident with no dedup key is a log line, and accepting one would put a record in
// the register that nothing can ever clear.
func TestAnIncidentWithoutAnIDOrKindIsRejected(t *testing.T) {
	h := bareBox(t)
	for _, body := range []string{`{}`, `{"id":"x"}`, `{"kind":"x"}`, `not json`} {
		if code, got := call(t, h, http.MethodPost, "/api/incidents", body); code != http.StatusBadRequest {
			t.Errorf("POST %s = %d, want 400: %s", body, code, got)
		}
	}
}

// The button's whole job is to tell the person configuring the relay what is wrong
// with it, so "no relay at all" has to be a sentence and not a 500.
func TestTheTestButtonSaysWhenThereIsNoRelay(t *testing.T) {
	h := bareBox(t)
	code, body := call(t, h, http.MethodPost, "/api/notifications/test", "")
	if code != http.StatusBadRequest {
		t.Fatalf("POST /api/notifications/test = %d, want 400: %s", code, body)
	}
	if !strings.Contains(body, "no mail server configured") {
		t.Errorf("the error should name the problem, got %s", body)
	}
}

// chi resolves a static segment ahead of a parameter at the same depth, and these
// routes sit beside /api/apps/{id}/{action}. The same precedence
// TestBackupRoutesBeatTheActionCatchAll pins for the backup routes.
func TestIncidentRoutesAreNotShadowed(t *testing.T) {
	h := bareBox(t)
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/api/incidents", "", http.StatusOK},
		{http.MethodPut, "/api/incidents/mute", `{"kind":"disk.full","muted":true}`, http.StatusOK},
		{http.MethodPost, "/api/incidents/nothing.here/ack", "", http.StatusBadRequest},
	} {
		if code, got := call(t, h, c.method, c.path, c.body); code != c.want {
			t.Errorf("%s %s = %d, want %d: %s", c.method, c.path, code, c.want, got)
		}
	}
}

// A mute is about the inbox, not the evidence, so the settings page has to be able to
// read back what is muted.
func TestAMuteIsVisibleInTheSnapshot(t *testing.T) {
	h := bareBox(t)
	code, body := call(t, h, http.MethodPut, "/api/incidents/mute", `{"kind":"disk.full","muted":true}`)
	if code != http.StatusOK {
		t.Fatalf("PUT mute = %d: %s", code, body)
	}
	_, body = call(t, h, http.MethodGet, "/api/incidents", "")
	if !strings.Contains(body, `"disk.full":true`) {
		t.Errorf("the snapshot does not report the mute: %s", body)
	}
	if code, _ := call(t, h, http.MethodPut, "/api/incidents/mute", `{"muted":true}`); code != http.StatusBadRequest {
		t.Errorf("muting nothing in particular should be a 400, got %d", code)
	}
}
