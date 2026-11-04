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

const (
	storeA = "https://a.invalid/archive/main.zip"
	storeB = "https://b.invalid/archive/main.zip"
	storeC = "https://c.invalid/archive/main.zip"
)

// installedWithRef lays down an installed app whose override records a store-ref,
// the only thing sourceRows reads about it.
func installedWithRef(t *testing.T, cfg config.Config, id, ref string) {
	t.Helper()
	dir := filepath.Join(cfg.AppsDir(), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	over := "x-compose-app:\n  store-ref: " + ref + "\n"
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.override.yml"), []byte(over), 0o644); err != nil {
		t.Fatal(err)
	}
}

func getSources(t *testing.T, h http.Handler, method, body string) sourcesResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, "/api/store/sources", nil)
	} else {
		req = httptest.NewRequest(method, "/api/store/sources", strings.NewReader(body))
	}
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s /api/store/sources -> %d (body %q)", method, rec.Code, rec.Body.String())
	}
	var got sourcesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
	}
	return got
}

func rowFor(rows []sourceRow, url, apps string) *sourceRow {
	for i := range rows {
		if rows[i].URL == url && rows[i].AppsPath == apps {
			return &rows[i]
		}
	}
	return nil
}

func usedBy(r *sourceRow) []string {
	var ids []string
	for _, u := range r.UsedBy {
		ids = append(ids, u.ID)
	}
	return ids
}

// The list is configured ∪ effective: a store an installed app's ref names is
// listed even when it is not a source, because every update check fetches it.
func TestStoreSourcesListEffectiveStores(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir(), StateDirPath: t.TempDir(), StoreURLs: []string{storeA}}
	installedWithRef(t, cfg, "alpha", "a.invalid/archive/main.zip/-/Apps/alpha")
	installedWithRef(t, cfg, "beta", "b.invalid/archive/main.zip/-/Apps/beta")
	installedWithRef(t, cfg, "gamma", "c.invalid/archive/main.zip/-/Custom/gamma")
	installedWithRef(t, cfg, "merged", "FileBrowser") // no store: resolves against the configured ones

	got := getSources(t, New(cfg, fstest.MapFS{}), http.MethodGet, "").Sources
	if len(got) != 3 {
		t.Fatalf("rows = %+v, want A (configured) + B + C (effective)", got)
	}
	if got[0].URL != storeA || !got[0].Configured || strings.Join(usedBy(&got[0]), ",") != "alpha" {
		t.Fatalf("row 0 = %+v, want configured A used by alpha", got[0])
	}
	b := rowFor(got, storeB, "")
	if b == nil || b.Configured || !b.Addable || strings.Join(usedBy(b), ",") != "beta" {
		t.Fatalf("B = %+v, want effective-only, addable, used by beta", b)
	}
	c := rowFor(got, storeC, "Custom")
	if c == nil || c.Configured || c.Addable || strings.Join(usedBy(c), ",") != "gamma" {
		t.Fatalf("C = %+v, want effective-only on Custom, not addable, used by gamma", c)
	}
}

// Removing a source that apps still use keeps it listed (as effective-only) and
// says why, instead of looking like the store is gone.
func TestRemovingAStillUsedSourceWarns(t *testing.T) {
	cfg := config.Config{DataRoot: t.TempDir(), StateDirPath: t.TempDir(), StoreURLs: []string{storeA, storeB}}
	installedWithRef(t, cfg, "alpha", "a.invalid/archive/main.zip/-/Apps/alpha")
	h := New(cfg, fstest.MapFS{})

	got := getSources(t, h, http.MethodDelete, `{"url":"`+storeA+`"}`)
	a := rowFor(got.Sources, storeA, "")
	if a == nil || a.Configured || strings.Join(usedBy(a), ",") != "alpha" {
		t.Fatalf("A = %+v, want it still listed, effective-only, used by alpha", a)
	}
	if !strings.Contains(got.Warning, "alpha still updates from this store") {
		t.Fatalf("warning = %q, want it to name the app still using the store", got.Warning)
	}

	// Removing one nobody uses says nothing about apps.
	got = getSources(t, h, http.MethodDelete, `{"url":"`+storeB+`"}`)
	if strings.Contains(got.Warning, "still update") {
		t.Fatalf("warning = %q, want no usage warning for an unused store", got.Warning)
	}
	if rowFor(got.Sources, storeB, "") != nil {
		t.Fatalf("B still listed after removal with no app using it: %+v", got.Sources)
	}
}
