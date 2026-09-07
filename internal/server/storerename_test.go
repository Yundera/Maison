package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/yundera/maison/internal/config"
)

// Renaming a store is display-only, but it has to outlive the process: the name
// goes into settings.json beside the source list, keyed by the store's URL, and
// is applied to the manager at boot. A rename that lived only in memory would be
// gone at the next restart, which is the one thing an operator naming their
// stores would not expect.
func TestRenameStoreSourcePersists(t *testing.T) {
	state := t.TempDir()
	cfg := config.Config{
		DataRoot:     t.TempDir(),
		StateDirPath: state,
		StoreURLs:    []string{"https://store.invalid/archive/refs/heads/main.zip"},
	}
	h := New(cfg, fstest.MapFS{})

	rename := func(h http.Handler, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/store/sources/name", strings.NewReader(body))
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := rename(h, `{"url":"https://store.invalid/archive/refs/heads/main.zip","name":"Lab store"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename -> %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var got sourcesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
	}
	if len(got.Sources) != 1 || got.Sources[0].Name != "Lab store" || !got.Sources[0].Custom {
		t.Fatalf("sources = %+v, want the custom name back", got.Sources)
	}

	b, err := os.ReadFile(filepath.Join(state, "settings.json"))
	if err != nil {
		t.Fatalf("settings.json: %v", err)
	}
	var file struct {
		StoreNames map[string]string `json:"store_names"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}
	if file.StoreNames["https://store.invalid/archive/refs/heads/main.zip"] != "Lab store" {
		t.Fatalf("settings.json store_names = %v, want the rename persisted", file.StoreNames)
	}

	// A second server over the same state directory is what a restart looks like.
	rec = httptest.NewRecorder()
	New(cfg, fstest.MapFS{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/store/sources", nil))
	got = sourcesResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode after restart: %v (body %q)", err, rec.Body.String())
	}
	if len(got.Sources) != 1 || got.Sources[0].Name != "Lab store" {
		t.Fatalf("after restart sources = %+v, want the name kept", got.Sources)
	}

	// Clearing it must reach the file too: a nil map read as "not supplied" would
	// leave the old name on disk and bring it back at the next boot.
	if rec := rename(h, `{"url":"https://store.invalid/archive/refs/heads/main.zip","name":"  "}`); rec.Code != http.StatusOK {
		t.Fatalf("clear -> %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	b, _ = os.ReadFile(filepath.Join(state, "settings.json"))
	file.StoreNames = nil
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatalf("settings.json: %v", err)
	}
	if _, ok := file.StoreNames["https://store.invalid/archive/refs/heads/main.zip"]; ok {
		t.Fatalf("settings.json store_names = %v, want the cleared name gone", file.StoreNames)
	}

	// A store that is not configured cannot be named: the name is keyed by URL and
	// one that matches nothing is a setting that never applies to anything.
	if rec := rename(h, `{"url":"https://other.invalid/store.zip","name":"Nope"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("rename of an unconfigured store -> %d, want 404", rec.Code)
	}
}
