package appstore

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// storeZip builds a minimal store archive in the default layout: a top-level
// folder (as a forge's archives have) containing an Apps/ directory, which is
// what findStoreRoot looks for.
//
// The compose file carries an x-casaos block because parseStore drops any app
// without one — a fixture of bare `services: {}` extracts fine but yields an
// empty catalog, which silently passes any test that only counts HTTP transfers.
func storeZip(t *testing.T, appName string) []byte {
	t.Helper()
	return storeZipAt(t, DefaultAppsPath, appName)
}

// storeZipAt is storeZip with the apps folder chosen, for the stores that do not
// use the CasaOS layout.
func storeZipAt(t *testing.T, appsPath, appName string) []byte {
	t.Helper()
	return storeZipNamed(t, appsPath, appName, "")
}

// storeZipNamed is storeZipAt with a store.json carrying storeName. An empty name
// ships no store.json at all, which is the store that has to fall back to its URL.
func storeZipNamed(t *testing.T, appsPath, appName, storeName string) []byte {
	t.Helper()
	compose := "name: " + appName + "\n" +
		"services:\n" +
		"  app:\n" +
		"    image: example/" + appName + ":1\n" +
		"x-casaos:\n" +
		"  main: app\n" +
		"  title:\n" +
		"    en_us: " + appName + "\n"

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("AppStore-main/" + appsPath + "/" + appName + "/docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(compose)); err != nil {
		t.Fatal(err)
	}
	if storeName != "" {
		mw, err := zw.Create("AppStore-main/store.json")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mw.Write([]byte(`{"name": "` + storeName + `"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// storeServer serves a zip with an ETag, honours If-None-Match with a 304, and
// counts how many times it actually sent a body.
type storeServer struct {
	*httptest.Server
	etag   string
	body   []byte
	bodies int // times the full zip was transferred
	gets   int // total GETs (including 304s)
}

func newStoreServer(t *testing.T, etag string, body []byte) *storeServer {
	t.Helper()
	s := &storeServer{etag: etag, body: body}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.gets++
		w.Header().Set("ETag", s.etag)
		if r.Header.Get("If-None-Match") == s.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		s.bodies++
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(s.body)
	}))
	t.Cleanup(s.Close)
	return s
}

// An unchanged store must not be re-downloaded — the conditional GET comes back
// 304 — and that must survive a restart, which is where the reference CasaOS
// implementation re-downloads (it keeps the ETag in memory only).
func TestSyncStoreSkipsUnchangedDownload(t *testing.T) {
	srv := newStoreServer(t, `"v1"`, storeZip(t, "demo"))
	cache := t.TempDir()
	ctx := context.Background()

	m := New([]string{srv.URL}, cache)
	if err := m.Refresh(ctx); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if srv.bodies != 1 {
		t.Fatalf("first refresh: got %d body transfers, want 1", srv.bodies)
	}

	// Same process, store unchanged: 304, no body.
	if err := m.Refresh(ctx); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if srv.bodies != 1 {
		t.Fatalf("unchanged store re-downloaded: %d body transfers, want 1", srv.bodies)
	}

	// Restart: a brand-new Manager over the same cache dir must still get a 304,
	// because the validators were persisted to disk rather than held in memory.
	restarted := New([]string{srv.URL}, cache)
	if err := restarted.Refresh(ctx); err != nil {
		t.Fatalf("refresh after restart: %v", err)
	}
	if srv.bodies != 1 {
		t.Fatalf("restart re-downloaded the store: %d body transfers, want 1", srv.bodies)
	}
	if srv.gets != 3 {
		t.Fatalf("got %d GETs, want 3 (one per refresh)", srv.gets)
	}
}

// A store whose content actually changed must be re-downloaded and re-parsed.
func TestSyncStoreRefetchesWhenChanged(t *testing.T) {
	srv := newStoreServer(t, `"v1"`, storeZip(t, "demo"))
	cache := t.TempDir()
	ctx := context.Background()

	m := New([]string{srv.URL}, cache)
	if err := m.Refresh(ctx); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	srv.etag = `"v2"`
	srv.body = storeZip(t, "other")
	if err := m.Refresh(ctx); err != nil {
		t.Fatalf("refresh after change: %v", err)
	}
	if srv.bodies != 2 {
		t.Fatalf("changed store not re-downloaded: %d body transfers, want 2", srv.bodies)
	}
}

// RefreshStore is the user hitting "refresh": it must bypass the 304 and refetch
// even when the origin still considers the store unchanged.
func TestRefreshStoreForcesDownload(t *testing.T) {
	srv := newStoreServer(t, `"v1"`, storeZip(t, "demo"))
	cache := t.TempDir()
	ctx := context.Background()

	m := New([]string{srv.URL}, cache)
	if err := m.Refresh(ctx); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if err := m.RefreshStore(ctx, srv.URL); err != nil {
		t.Fatalf("forced refresh: %v", err)
	}
	if srv.bodies != 2 {
		t.Fatalf("forced refresh served from cache: %d body transfers, want 2", srv.bodies)
	}
}

// An origin that sends no validators at all must still work: every refresh is an
// unconditional GET, and no stale validator file is left behind to poison it.
func TestSyncStoreWithoutValidators(t *testing.T) {
	body := storeZip(t, "demo")
	var gets int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets++
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			t.Errorf("sent a conditional request with no validators on record")
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	m := New([]string{srv.URL}, t.TempDir())
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := m.Refresh(ctx); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	if gets != 2 {
		t.Fatalf("got %d GETs, want 2", gets)
	}
}

// The nightly refresh must land on the next 03:00 wall-clock time, whether that
// is later today or tomorrow — never on "24h from whenever the process booted".
func TestUntilNext(t *testing.T) {
	loc := time.FixedZone("TST", 2*3600)
	cases := []struct {
		name string
		now  time.Time
		want time.Duration
	}{
		{"before", time.Date(2026, 7, 28, 1, 30, 0, 0, loc), 90 * time.Minute},
		{"after", time.Date(2026, 7, 28, 3, 30, 0, 0, loc), 23*time.Hour + 30*time.Minute},
		{"exactly on it", time.Date(2026, 7, 28, 3, 0, 0, 0, loc), 24 * time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := untilNext(c.now, 3, 0); got != c.want {
				t.Fatalf("untilNext(%s) = %s, want %s", c.now, got, c.want)
			}
		})
	}
}

// An unreachable store must be reported, not swallowed: the ⟳ button and the
// add-source flow both decide what to tell the user from this error.
func TestRefreshReportsUnreachableStore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	m := New([]string{srv.URL}, t.TempDir())
	if err := m.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh reported success for a store that 404s")
	}
}

// ...but one dead store must not empty the catalog: the healthy ones still parse,
// and the failure rides alongside them.
func TestRefreshKeepsHealthyStoresWhenOneFails(t *testing.T) {
	good := newStoreServer(t, `"v1"`, storeZip(t, "jellyfin"))
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	m := New([]string{good.URL, bad.URL}, t.TempDir())
	if err := m.Refresh(context.Background()); err == nil {
		t.Fatal("want the failing store reported")
	}
	if got := len(m.Catalog()); got != 1 {
		t.Fatalf("catalog has %d apps, want the 1 from the healthy store", got)
	}
}

// A store that goes offline after a successful sync keeps serving its cached copy
// — but the refresh still says it never reached the origin, so a ⟳ on an offline
// box doesn't render as a successful reload.
func TestRefreshReportsStaleFallback(t *testing.T) {
	body := storeZip(t, "jellyfin")
	up := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	m := New([]string{srv.URL}, t.TempDir())
	ctx := context.Background()
	if err := m.Refresh(ctx); err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	up = false
	if err := m.Refresh(ctx); err == nil {
		t.Fatal("offline origin reported as a successful refresh")
	}
	if got := len(m.Catalog()); got != 1 {
		t.Fatalf("catalog has %d apps, want the cached copy to survive", got)
	}
}

// Two stores shipping the same app folder is normal — a fork of a store, or the
// same store at two branches while one is being tested. Only one of them can answer
// the bare id, but the other must not be *dropped*: it stays in CatalogAll, marked
// non-primary, so the panel can browse it by naming its store and install that
// store's copy rather than the copy that happened to be listed first.
func TestCollidingAppIsKeptPerStore(t *testing.T) {
	first := newStoreServer(t, `"v1"`, storeZipNamed(t, DefaultAppsPath, "MetaWatch", "First Store"))
	second := newStoreServer(t, `"v1"`, storeZipNamed(t, DefaultAppsPath, "MetaWatch", "Second Store"))

	m := New([]string{first.URL, second.URL}, t.TempDir())
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	merged := m.Catalog()
	if len(merged) != 1 {
		t.Fatalf("merged catalog has %d apps, want the colliding id to appear once", len(merged))
	}
	if merged[0].StoreName != "First Store" {
		t.Fatalf("merged copy came from %q, want the first store to win", merged[0].StoreName)
	}

	all := m.CatalogAll()
	if len(all) != 2 {
		t.Fatalf("CatalogAll has %d apps, want both stores' copies", len(all))
	}
	byStore := map[string]*CatalogApp{}
	for _, a := range all {
		byStore[a.StoreName] = a
	}
	if a := byStore["First Store"]; a == nil || !a.Primary {
		t.Fatalf("first store's copy = %+v, want it present and primary", a)
	}
	if a := byStore["Second Store"]; a == nil || a.Primary {
		t.Fatalf("second store's copy = %+v, want it present and NOT primary", a)
	}
}

// The catalog must never be observable as "missing" while a refresh swaps in a new
// copy of a store — that window is what makes a browse or an install fail on a
// store that is perfectly fine (see swapIn).
func TestConcurrentRefreshKeepsCatalogReadable(t *testing.T) {
	srv := newStoreServer(t, `"v1"`, storeZip(t, "jellyfin"))
	m := New([]string{srv.URL}, t.TempDir())
	ctx := context.Background()
	if err := m.Refresh(ctx); err != nil {
		t.Fatalf("seed refresh: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10; i++ {
			// Force a real re-download and swap each time.
			if err := m.RefreshStore(ctx, srv.URL); err != nil {
				t.Errorf("refresh %d: %v", i, err)
				return
			}
		}
	}()

	for {
		select {
		case <-done:
			return
		default:
		}
		if _, _, err := m.Get("jellyfin"); err != nil {
			t.Fatalf("app unreadable mid-swap: %v", err)
		}
	}
}

// A store is free to keep its apps somewhere other than Apps/, and a reference
// says where. Without this the folder in a deep link would be parsed, sent, and
// then quietly ignored in favour of the default — which resolves a different app
// than the one the link named, or none at all.
func TestGetFromReadsTheAppsFolderTheReferenceNames(t *testing.T) {
	srv := newStoreServer(t, `"v1"`, storeZipAt(t, "catalog/apps", "demo"))
	m := New(nil, t.TempDir())
	ctx := context.Background()

	app, raw, err := m.GetFrom(ctx, NewRef(srv.URL, "catalog/apps", "demo"))
	if err != nil {
		t.Fatalf("GetFrom: %v", err)
	}
	if app.ID != "demo" {
		t.Errorf("ID = %q, want demo", app.ID)
	}
	if app.AppsPath != "catalog/apps" {
		t.Errorf("AppsPath = %q, want catalog/apps — the client needs it back to pin an install", app.AppsPath)
	}
	if !bytes.Contains(raw, []byte("image: example/demo:1")) {
		t.Errorf("compose bytes are not the app's: %s", raw)
	}

	// The default is still the default: the same store read without a folder must
	// not find an app, rather than finding some other one.
	if _, _, err := m.GetFrom(ctx, NewRef(srv.URL, "", "demo")); err == nil {
		t.Error("GetFrom with the default folder found an app in a store that has none there")
	}
}

// A store says what it is called; Maison does not guess. The label used to be
// derived as "owner/repo" from the URL path, which is one forge's layout — it
// means nothing for a store served from anywhere else, and it renders two refs of
// the same repository identically, which is precisely when a person most needs to
// know which one they are about to install from.
func TestStoreNameComesFromTheStoreOrFallsBackToItsURL(t *testing.T) {
	named := newStoreServer(t, `"v1"`, storeZipNamed(t, DefaultAppsPath, "demo", "Example App Store"))
	anon := newStoreServer(t, `"v1"`, storeZip(t, "demo"))
	ctx := context.Background()

	m := New([]string{named.URL}, t.TempDir())
	if err := m.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := m.Sources(); len(got) != 1 || got[0].Name != "Example App Store" {
		t.Errorf("Sources() = %+v, want the name from store.json", got)
	}
	if app, _, err := m.GetFrom(ctx, NewRef(named.URL, "", "demo")); err != nil {
		t.Fatalf("GetFrom: %v", err)
	} else if app.StoreName != "Example App Store" {
		t.Errorf("StoreName = %q, want the name from store.json", app.StoreName)
	}

	// No store.json: named by where it came from, not by a guess at its URL's shape.
	m2 := New([]string{anon.URL}, t.TempDir())
	if err := m2.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := m2.Sources(); len(got) != 1 || got[0].Name != anon.URL {
		t.Errorf("Sources() = %+v, want the URL %q", got, anon.URL)
	}
	if app, _, err := m2.GetFrom(ctx, NewRef(anon.URL, "", "demo")); err != nil {
		t.Fatalf("GetFrom: %v", err)
	} else if app.StoreName != anon.URL {
		t.Errorf("StoreName = %q, want the URL %q", app.StoreName, anon.URL)
	}
}
