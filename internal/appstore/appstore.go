// Package appstore fetches CasaOS-compatible app stores (zip archives over
// HTTP), extracts them, and builds a merged catalog of installable apps keyed by
// app id. Layout ported from casa-img: <root>/<apps>/<name>/docker-compose.yml
// plus category-list.json and recommend-list.json.
//
// <apps> is named by the store reference (ref.go) and defaults to Apps, so where
// a store keeps its apps is the store's business rather than a constant in here.
// Nothing in this package knows which forge a store is hosted on, with the single
// exception of storeZipCandidates — see the note on it.
package appstore

import (
	"archive/zip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yundera/maison/internal/asset"
	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/xcasaos"
)

// CatalogApp is one installable app from a store.
type CatalogApp struct {
	ID          string   `json:"id"`   // catalog id: the app's directory name
	Name        string   `json:"name"` // display title
	Tagline     string   `json:"tagline"`
	Description string   `json:"description"`
	Icon        string   `json:"icon"`
	Thumbnail   string   `json:"thumbnail"`
	Screenshots []string `json:"screenshots"`
	Category    string   `json:"category"`
	Developer   string   `json:"developer"`
	Author      string   `json:"author"`
	MinMemory   int      `json:"min_memory,omitempty"`
	StoreURL    string   `json:"store"`
	// AppsPath is the folder inside the archive this app was found in. It rides
	// back to the client so a pinned install goes to the same place the app was
	// read from, rather than to whatever the default happens to be.
	AppsPath string `json:"apps_path,omitempty"`
	// StoreName is what the store calls itself (store.json), falling back to its
	// URL. Shown wherever a store has to be named to a person.
	StoreName string `json:"store_name,omitempty"`
	// Primary is true for the copy that won the merge — the one the id alone
	// resolves to. Two stores may both ship an app under the same folder name, and
	// only one of them can answer a bare id; the others are still in the payload,
	// reachable by naming their store, and marked false here so a merged view can
	// leave them out and show each app once.
	Primary bool `json:"primary"`

	composePath string // absolute path to the app's compose file
	// iconRel is the icon as the compose declared it when that was a file beside
	// the compose, kept because Icon above has by then been rewritten into the URL
	// the browser fetches. The installer copies the file, not the URL — it is
	// reading bytes already on this disk, and the copy has to survive the store the
	// URL points into being refreshed or removed.
	iconRel string
}

// Dir is the app's folder inside the extracted store — the directory holding its
// docker-compose.yml, and beside it whatever else the store ships for the app
// (its seed tree, its icon). The installer reads it to copy the seed tree in.
func (a *CatalogApp) Dir() string { return filepath.Dir(a.composePath) }

// IconRel is the app's icon as a path inside Dir, or "" when the compose declared
// a URL instead. See the field.
func (a *CatalogApp) IconRel() string { return a.iconRel }

// Source is one configured store, named for display.
type Source struct {
	URL  string `json:"url"`
	Name string `json:"name"`
}

// Manager holds the merged catalog across all configured stores.
type Manager struct {
	urls     []string
	cacheDir string

	// syncMu serializes the download/extract half of a refresh. Two refreshes of
	// the same store share a workdir and a `<workdir>.tmp` staging directory, so
	// running them concurrently means one wiping the other's half-extracted tree.
	// The nightly refresh, the boot refresh and the ⟳ button can all overlap, so
	// this is not hypothetical.
	syncMu sync.Mutex

	// filesMu guards the extracted store trees against the rename that swaps a new
	// copy in. A CatalogApp holds a *path*, not an open file, and the path is the
	// same string before and after a swap — so without this a read landing between
	// the two renames reports "no such file" for an app that exists in both copies.
	// Writers hold it for two renames; readers for one os.ReadFile.
	filesMu sync.RWMutex

	mu      sync.RWMutex
	catalog map[string]*CatalogApp
	order   []string // stable catalog order
	// all is every store's copy of every app, including the ones that lost an id
	// collision — the merged catalog above holds only the winners. Kept so a
	// filter that names one store can show that store's own copy, and so an
	// install from it goes to the store it was read from.
	all       []*CatalogApp
	cats      []string
	recommend []string
	// names is what each store called itself at the last refresh, by URL. Cached
	// rather than read on demand: naming a source otherwise costs a directory walk
	// to find the store root, per source, every time the menu opens.
	names map[string]string
}

// New creates a Manager for the given store URLs, caching under cacheDir.
func New(urls []string, cacheDir string) *Manager {
	return &Manager{
		urls:     canonicalURLs(urls),
		cacheDir: cacheDir,
		catalog:  map[string]*CatalogApp{},
		names:    map[string]string{},
	}
}

// URLs returns the configured store URLs.
func (m *Manager) URLs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.urls...)
}

// SetURLs replaces the store URL list (caller should Refresh afterwards).
func (m *Manager) SetURLs(urls []string) {
	m.mu.Lock()
	m.urls = canonicalURLs(urls)
	m.mu.Unlock()
}

// canonicalURLs normalises a source list at the boundary, so one store cannot
// enter the manager under two spellings and get two cache directories.
func canonicalURLs(urls []string) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if c := CanonicalURL(u); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// Sources returns the configured stores, each named as it names itself. A store
// that has never been read successfully has no name of its own and is listed by
// URL — which is also the fallback for one that ships no store.json, since a
// store with nothing to say about itself is best identified by where it came
// from rather than by a guess made from its URL's shape.
func (m *Manager) Sources() []Source {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Source, 0, len(m.urls))
	for _, u := range m.urls {
		name := m.names[u]
		if name == "" {
			name = u
		}
		out = append(out, Source{URL: u, Name: name})
	}
	return out
}

// Catalog returns all apps sorted by name.
func (m *Manager) Catalog() []*CatalogApp {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*CatalogApp, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.catalog[id])
	}
	return out
}

// CatalogAll returns every store's copy of every app, sorted by name — the
// merged catalog plus the copies that lost an id collision, each carrying the
// store it came from and a Primary flag saying whether it won.
//
// Catalog() is the merged view and stays the one that answers a bare id. This is
// for the caller that has to show what each configured store actually offers: a
// store whose every app is shadowed contributes nothing to Catalog(), and a UI
// built on that alone cannot tell it from a store that failed to load.
func (m *Manager) CatalogAll() []*CatalogApp {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]*CatalogApp(nil), m.all...)
}

// Categories returns the distinct category list.
func (m *Manager) Categories() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.cats...)
}

// Recommend returns featured app ids.
func (m *Manager) Recommend() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.recommend...)
}

// Get returns an app by id and its raw compose bytes.
func (m *Manager) Get(id string) (*CatalogApp, []byte, error) {
	m.mu.RLock()
	app := m.catalog[id]
	m.mu.RUnlock()
	if app == nil {
		return nil, nil, fmt.Errorf("app %q not found", id)
	}
	m.filesMu.RLock()
	raw, err := os.ReadFile(app.composePath)
	m.filesMu.RUnlock()
	if err != nil {
		return nil, nil, err
	}
	return app, raw, nil
}

// GetFrom returns the app named by ref, along with its raw compose bytes. The
// app need not be in the merged catalog at all — the reference may name a store
// the user has never added — which is what lets a deep link
// (/store/<locator>/-/<apps>/<id>) address an app in an unlisted store. A
// reference with no locator is answered by the merged catalog (Get).
//
// An already-extracted copy of the store answers as-is — including "no such app",
// so a bad id fails fast; only a store that has never been fetched is downloaded
// here. Browsing must not pay for a sync: stores run to tens of MB and a
// re-download would stall the detail page for minutes. Configured stores are kept
// fresh by the hourly Refresh; an unlisted store is fetched once, on the first
// deep link that names it, and thereafter refreshed only on demand (RefreshStore)
// or when the update flow syncs it (AppComposeFrom).
func (m *Manager) GetFrom(ctx context.Context, ref Ref) (*CatalogApp, []byte, error) {
	if ref.Merged() {
		return m.Get(ref.ID)
	}
	root, err := findStoreRoot(m.workdir(ref.URL), ref.Apps())
	if err != nil {
		m.syncMu.Lock()
		root, err = m.syncStore(ctx, ref.URL, ref.Apps())
		m.syncMu.Unlock()
		// root != "" with a non-nil err means a cached copy was served; there is
		// none here (findStoreRoot just failed), but branch on root so this stays
		// correct if that ever changes.
		if root == "" {
			return nil, nil, err
		}
	}
	return m.appIn(root, ref)
}

// AppComposeFrom returns the raw docker-compose.yml bytes for the app named by
// ref as it currently stands in its store. Unlike GetFrom it always syncs the store
// first: the update flow diffs the store's live version against what's installed,
// so a stale extracted copy would report "up to date" when it isn't. That is also
// why an unreachable origin is an error here even when a cached copy exists —
// "couldn't check" must not render as "up to date".
func (m *Manager) AppComposeFrom(ctx context.Context, ref Ref) ([]byte, error) {
	_, raw, err := m.AppSyncedFrom(ctx, ref)
	return raw, err
}

// AppSyncedFrom is AppComposeFrom with the app itself, for a caller that needs
// more of the store's copy than its compose bytes — the update flow refreshes
// the app's seed tree from CatalogApp.Dir() at the same time, and both have to
// come from the same sync or the two halves could disagree.
func (m *Manager) AppSyncedFrom(ctx context.Context, ref Ref) (*CatalogApp, []byte, error) {
	if ref.Merged() {
		return m.Get(ref.ID)
	}
	m.syncMu.Lock()
	root, err := m.syncStore(ctx, ref.URL, ref.Apps())
	m.syncMu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	return m.appIn(root, ref)
}

// appIn finds app id in an extracted store root and reads its compose file. It
// walks the tree on disk, so it runs under filesMu like any other reader.
func (m *Manager) appIn(root string, ref Ref) (*CatalogApp, []byte, error) {
	m.filesMu.RLock()
	defer m.filesMu.RUnlock()

	apps, _, _ := parseStore(root, ref.URL, ref.Apps())
	for _, a := range apps {
		if a.ID == ref.ID {
			raw, err := os.ReadFile(a.composePath)
			if err != nil {
				return nil, nil, err
			}
			return a, raw, nil
		}
	}
	return nil, nil, fmt.Errorf("app %q not found in %s/ of store %s", ref.ID, ref.Apps(), ref.URL)
}

// Refresh downloads and reparses every configured store.
//
// A store that cannot be reached does not sink the rest: the catalog is rebuilt
// from every store that did answer (plus any usable cached copy of the ones that
// didn't), and the failures come back joined in the error. Callers that only
// display a catalog can ignore it; the ones driven by a user action — the ⟳
// button, adding a source — report it, so a reload that never reached the origin
// doesn't look like it worked.
func (m *Manager) Refresh(ctx context.Context) error {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()

	catalog := map[string]*CatalogApp{}
	var order []string
	var all []*CatalogApp
	catSet := map[string]bool{}
	var recommend []string
	var errs []error
	names := map[string]string{}

	// A configured source names no apps folder — only a reference can — so the
	// merged catalog is built from the default layout.
	for _, u := range m.URLs() {
		root, err := m.syncStore(ctx, u, DefaultAppsPath)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", u, err))
		}
		if root == "" {
			continue // nothing on disk to fall back on either
		}
		apps, cats, rec := parseStore(root, u, DefaultAppsPath)
		if n := readStoreName(root); n != "" {
			names[u] = n
		}
		for _, a := range apps {
			all = append(all, a)
			if won, exists := catalog[a.ID]; exists {
				// First store wins on id collision. The loser stays in `all` — it is
				// still installable by naming its store — but it cannot answer the bare
				// id. Logged because it is otherwise invisible: a store whose apps all
				// lost looks, from the catalog alone, like a store that never loaded.
				log.Printf("appstore: %s: app %q shadowed by the copy in %s", u, a.ID, won.StoreURL)
				continue
			}
			a.Primary = true
			catalog[a.ID] = a
			order = append(order, a.ID)
		}
		for _, c := range cats {
			catSet[c] = true
		}
		recommend = append(recommend, rec...)
	}

	sort.Slice(order, func(i, j int) bool {
		return strings.ToLower(catalog[order[i]].Name) < strings.ToLower(catalog[order[j]].Name)
	})
	// Same order as the merged catalog, with the store URL breaking the tie two
	// copies of one app would otherwise leave to sort.Slice's instability.
	sort.Slice(all, func(i, j int) bool {
		ni, nj := strings.ToLower(all[i].Name), strings.ToLower(all[j].Name)
		if ni != nj {
			return ni < nj
		}
		return all[i].StoreURL < all[j].StoreURL
	})
	cats := make([]string, 0, len(catSet))
	for c := range catSet {
		cats = append(cats, c)
	}
	sort.Strings(cats)

	m.mu.Lock()
	m.catalog = catalog
	m.order = order
	m.all = all
	m.cats = cats
	m.recommend = recommend
	m.names = names
	m.mu.Unlock()
	return errors.Join(errs...)
}

// RefreshStore forces a re-download of a single store — dropping its cached
// validators so the conditional GET in syncStore can't come back 304 — then
// rebuilds the merged catalog. Other stores are re-synced too but skip their
// download when unchanged.
//
// Only storeURL's own outcome is reported. Refresh reports every store's, and a
// second, unrelated broken source must not make this store's reload look failed.
func (m *Manager) RefreshStore(ctx context.Context, storeURL string) error {
	storeURL = CanonicalURL(storeURL)
	clearValidators(m.workdir(storeURL))

	// Sync the named store on its own first so its error is the one that surfaces.
	// This costs no extra download: on success the validators are back on disk, so
	// the Refresh below re-checks this store with a conditional GET that 304s.
	m.syncMu.Lock()
	_, err := m.syncStore(ctx, storeURL, DefaultAppsPath)
	m.syncMu.Unlock()

	_ = m.Refresh(ctx)
	return err
}

// StartDailyRefresh refreshes once at startup (so a box that has been off for a
// week doesn't browse a week-old catalog until 03:00) and then once a day at
// hour:minute in the process's local timezone — i.e. container time, which is
// whatever TZ the container was started with.
//
// The delay is recomputed before every wait instead of a fixed 24h ticker: a DST
// shift or a clock correction would otherwise drift the run off the wall-clock
// time and never come back to it.
func (m *Manager) StartDailyRefresh(ctx context.Context, hour, minute int) {
	go func() {
		_ = m.Refresh(ctx)
		for {
			t := time.NewTimer(untilNext(time.Now(), hour, minute))
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
				_ = m.Refresh(ctx)
			}
		}
	}()
}

// untilNext is the delay from now to the next hour:minute in now's location.
func untilNext(now time.Time, hour, minute int) time.Duration {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Sub(now)
}

// syncStore brings the extracted copy of a store up to date and returns its
// store root (the directory holding appsPath). An unchanged store costs one
// conditional GET that comes back 304 with no body — see fetch.
//
// When every download candidate fails, a previously extracted copy is returned
// *together with* the error rather than instead of it: a box with no connectivity
// keeps a usable catalog, while a caller that asked for a sync can still tell the
// user it never reached the origin. Callers therefore branch on root == "" for
// "nothing to show", not on err != nil.
//
// The caller must hold syncMu.
func (m *Manager) syncStore(ctx context.Context, storeURL, appsPath string) (string, error) {
	workdir := m.workdir(storeURL)

	var lastErr error
	for _, dl := range storeZipCandidates(storeURL) {
		root, err := m.fetch(ctx, dl, workdir, appsPath)
		if err != nil {
			lastErr = err
			continue
		}
		return root, nil
	}
	// Every candidate failed: fall back to any previously extracted copy.
	if root, ferr := findStoreRoot(workdir, appsPath); ferr == nil {
		return root, lastErr
	}
	return "", lastErr
}

// fetch conditionally downloads the store zip at u and extracts it into workdir.
//
// Freshness is a conditional GET rather than the HEAD-then-GET the CasaOS
// reference uses: we replay the ETag / Last-Modified of the copy we already have
// and let the origin decide. An unchanged store answers 304 with no body, so the
// hourly refresh of an idle box costs one round-trip and touches no disk. The
// validators are persisted next to the extracted copy, so this survives a
// restart — CasaOS keeps them in a struct field and therefore re-downloads the
// whole store on every boot.
//
// The body streams to a temp file and is opened from there. Buffering it in
// memory instead would cost ~2x the zip (tens of MB per store) on every refresh,
// which on a small host is the difference between an 18 MB resident process and
// a 300 MB spike.
func (m *Manager) fetch(ctx context.Context, u, workdir, appsPath string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}

	// Only make the request conditional when there is actually a copy on disk to
	// fall back on, so a 304 can always be honoured by reusing it.
	if _, err := findStoreRoot(workdir, appsPath); err == nil {
		if v := readValidators(workdir); v.ETag != "" || v.LastModified != "" {
			if v.ETag != "" {
				req.Header.Set("If-None-Match", v.ETag)
			}
			if v.LastModified != "" {
				req.Header.Set("If-Modified-Since", v.LastModified)
			}
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return findStoreRoot(workdir, appsPath)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("store %s: http %d", u, resp.StatusCode)
	}

	if err := m.extractStream(resp.Body, workdir); err != nil {
		return "", err
	}
	writeValidators(workdir, validators{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	})
	return findStoreRoot(workdir, appsPath)
}

// extractStream spools r (a zip) to a temp file and extracts it into dest,
// replacing any prior copy. The spool file is what keeps the zip out of the heap.
func (m *Manager) extractStream(r io.Reader, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	spool, err := os.CreateTemp(filepath.Dir(dest), ".store-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(spool.Name())
	defer spool.Close()

	if _, err := io.Copy(spool, r); err != nil { //nolint:gosec // store content, size-bounded by the origin
		return err
	}
	zr, err := zip.OpenReader(spool.Name())
	if err != nil {
		return err
	}
	defer zr.Close()

	tmp := dest + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := extractZip(&zr.Reader, tmp); err != nil {
		return err
	}
	return m.swapIn(tmp, dest)
}

// swapIn replaces dest with tmp, moving the old copy aside instead of deleting it
// first. Recursively deleting a store is thousands of unlinks, and for every one
// of them dest does not exist — a reader resolving an app's compose path in that
// window (a browse, an install) fails on a store that is perfectly fine. Here the
// old copy is only unlinked once the new one is already in place, and the two
// renames that stand in for it are taken under filesMu, so no reader observes the
// gap between them. The slow delete happens after the lock is released.
func (m *Manager) swapIn(tmp, dest string) error {
	old := dest + ".old"
	_ = os.RemoveAll(old)

	m.filesMu.Lock()
	err := os.Rename(dest, old)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.filesMu.Unlock()
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Rename(old, dest) // put the previous copy back rather than leaving none
		m.filesMu.Unlock()
		return err
	}
	m.filesMu.Unlock()

	_ = os.RemoveAll(old)
	return nil
}

// validators are the HTTP cache validators of the store copy currently extracted
// in a workdir, persisted so a restart doesn't re-download an unchanged store.
type validators struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

// validatorPath is a sibling of workdir, not a file inside it: extractStream
// swaps the workdir wholesale with a rename, which would take the file with it.
func validatorPath(workdir string) string { return workdir + ".validators.json" }

func readValidators(workdir string) validators {
	var v validators
	b, err := os.ReadFile(validatorPath(workdir))
	if err != nil {
		return v
	}
	_ = json.Unmarshal(b, &v)
	return v
}

func writeValidators(workdir string, v validators) {
	if v.ETag == "" && v.LastModified == "" {
		// Origin sent neither: drop any stale file so the next refresh is a plain
		// unconditional GET rather than one carrying validators for older content.
		clearValidators(workdir)
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = os.WriteFile(validatorPath(workdir), b, 0o644)
}

func clearValidators(workdir string) { _ = os.Remove(validatorPath(workdir)) }

// storeZipCandidates maps the various GitHub URL forms a user might paste into
// the codeload archive URL(s) to actually fetch. Supported inputs:
//
//	https://github.com/owner/repo                       -> archive main (then master)
//	https://github.com/owner/repo.git                   -> archive main (then master)
//	https://github.com/owner/repo/tree/<branch>         -> archive <branch>
//	https://github.com/owner/repo/archive/....zip       -> unchanged
//
// Non-GitHub hosts and URLs already ending in .zip are passed through untouched.
// When the branch is implicit both "main" and "master" archives are returned so
// the repository's default branch is auto-detected at download time.
// storeZipCandidates expands a convenience spelling of a store URL into the
// archive URLs to try.
//
// This is the ONLY forge-specific code in Maison, and it is deliberately confined
// to convenience at the point a human pastes a URL into the add-source box: it
// turns a GitHub repo or /tree/<branch> link into the archive URL that link
// implies. It is not part of the addressing vocabulary — a store reference always
// carries a real locator — and it must not grow: any shorthand that expands to an
// archive URL is knowledge of one forge's path shapes, and Maison should not need
// to be taught a new one to host a store.
//
// A URL that already names an archive, or lives anywhere but github.com, is
// passed through untouched.
func storeZipCandidates(raw string) []string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return []string{raw}
	}
	host := strings.ToLower(u.Host)
	if host != "github.com" && host != "www.github.com" {
		return []string{raw} // direct zip or some other host: leave as-is
	}
	if strings.HasSuffix(strings.ToLower(u.Path), ".zip") {
		return []string{raw} // already an archive URL
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return []string{raw}
	}
	owner := parts[0]
	repo := strings.TrimSuffix(parts[1], ".git")
	if owner == "" || repo == "" {
		return []string{raw}
	}

	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	archive := func(branch string) string {
		return fmt.Sprintf("%s://github.com/%s/%s/archive/refs/heads/%s.zip", scheme, owner, repo, branch)
	}

	// .../tree/<branch>[/<subpath>...] — explicit branch (may contain slashes).
	if len(parts) >= 4 && parts[2] == "tree" {
		return []string{archive(strings.Join(parts[3:], "/"))}
	}
	// Repo root / clone URL: default branch unknown, try main then master.
	return []string{archive("main"), archive("master")}
}

func (m *Manager) workdir(storeURL string) string {
	storeURL = CanonicalURL(storeURL)
	u, err := url.Parse(storeURL)
	if err != nil {
		sum := md5.Sum([]byte(storeURL))
		return filepath.Join(m.cacheDir, hex.EncodeToString(sum[:]))
	}
	sum := md5.Sum([]byte(strings.ToLower(u.Path)))
	return filepath.Join(m.cacheDir, u.Host, hex.EncodeToString(sum[:]))
}

func extractZip(zr *zip.Reader, dest string) error {
	for _, f := range zr.File {
		target := filepath.Join(dest, f.Name) //nolint:gosec // sanitized below
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			continue // zip-slip guard
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, storeFileMode(f.Mode()))
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc) //nolint:gosec // store content, size-bounded by GitHub
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// storeFileMode is the permission an extracted store file gets: 0755 if the
// archive marked it executable, 0644 otherwise.
//
// Only the exec bit is carried over. A store is a downloaded zip, and honouring
// its modes wholesale would let one land a setuid or world-writable file in the
// cache; discarding them entirely (which os.Create did) loses the one bit that
// carries meaning — an app's seed tree ships scripts, and a script that arrives
// without +x is a broken install with no declaration to fix it.
func storeFileMode(m fs.FileMode) fs.FileMode {
	if m.Perm()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

// findStoreRoot locates the store root inside an extracted archive: the
// directory holding appsPath. The root is searched for rather than assumed
// because every forge wraps an archive in a directory whose name encodes the
// repo and the ref, and that name is not ours to predict.
//
// appsPath may itself be nested ("catalog/apps"), so the match is on the trailing
// segments of the walked path rather than on a single directory name.
func findStoreRoot(dir, appsPath string) (string, error) {
	want := filepath.FromSlash(strings.Trim(appsPath, "/"))
	if want == "" {
		want = DefaultAppsPath
	}
	suffix := string(os.PathSeparator) + want
	var found string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() && strings.HasSuffix(p, suffix) {
			found = strings.TrimSuffix(p, suffix)
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("no %s/ directory under %s", filepath.ToSlash(want), dir)
	}
	return found, nil
}

func parseStore(root, storeURL, appsPath string) (apps []*CatalogApp, cats, recommend []string) {
	if strings.TrimSpace(appsPath) == "" {
		appsPath = DefaultAppsPath
	}
	appsDir := filepath.Join(root, filepath.FromSlash(appsPath))
	entries, err := os.ReadDir(appsDir)
	if err != nil {
		return nil, nil, nil
	}
	// What the store calls itself, or where it came from. Resolved once here so
	// every app of one store carries the same answer.
	storeName := readStoreName(root)
	if storeName == "" {
		storeName = storeURL
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		composePath := filepath.Join(appsDir, e.Name(), "docker-compose.yml")
		if _, err := os.Stat(composePath); err != nil {
			composePath = filepath.Join(appsDir, e.Name(), "docker-compose.yaml")
			if _, err := os.Stat(composePath); err != nil {
				continue
			}
		}
		f, err := composefile.Load(composePath)
		if err != nil {
			continue
		}
		si, err := f.StoreInfo()
		if err != nil {
			continue
		}
		apps = append(apps, catalogApp(e.Name(), si, composePath, storeURL, appsPath, storeName))
	}

	cats = readCategories(root)
	recommend = readRecommend(root)
	return apps, cats, recommend
}

func catalogApp(id string, si *xcasaos.StoreInfo, composePath, storeURL, appsPath, storeName string) *CatalogApp {
	name := xcasaos.Localized(si.Title)
	if name == "" {
		name = id
	}
	app := &CatalogApp{
		ID:          id,
		Name:        name,
		Tagline:     xcasaos.Localized(si.Tagline),
		Description: xcasaos.Localized(si.Description),
		Icon:        si.Icon,
		Thumbnail:   si.Thumbnail,
		Screenshots: si.ScreenshotLink,
		Category:    si.Category,
		Developer:   si.Developer,
		Author:      si.Author,
		MinMemory:   si.MinMemory,
		StoreURL:    storeURL,
		AppsPath:    appsPath,
		StoreName:   storeName,
		composePath: composePath,
	}
	app.resolveAssets(NewRef(storeURL, appsPath, id))
	return app
}

// resolveAssets turns the app's compose-relative asset values into URLs that
// fetch the file out of the extracted store, and leaves absolute URLs alone.
//
// It happens here, at the one place a CatalogApp is built, so that everything
// downstream — the browse grid, the detail page, the install placeholder tile —
// gets a value it can put straight into an <img src> without knowing a store
// exists. A value that is neither a URL nor a resolvable relative path is dropped
// rather than passed on: it would render as a request to the dashboard for a path
// that means nothing there.
func (a *CatalogApp) resolveAssets(ref Ref) {
	resolve := func(raw string) string {
		if asset.IsURL(raw) {
			return raw
		}
		if rel, ok := asset.Rel(raw); ok {
			return AssetURL(ref, rel)
		}
		return ""
	}
	if rel, ok := asset.Rel(a.Icon); ok {
		a.iconRel = rel
	}
	a.Icon = resolve(a.Icon)
	a.Thumbnail = resolve(a.Thumbnail)
	shots := a.Screenshots[:0:0]
	for _, s := range a.Screenshots {
		if u := resolve(s); u != "" {
			shots = append(shots, u)
		}
	}
	a.Screenshots = shots
}

// readStoreName reads what a store calls itself, from store.json at the store
// root:
//
//	{ "name": "Yundera App Store" }
//
// Every field is optional and the file itself is optional; an unreadable or
// unparseable one is simply a store that did not say, and the caller falls back
// to the URL. A name has to be declared rather than derived, because deriving one
// means reading a URL's shape — "owner/repo" is a forge's path layout, it is
// wrong for a store served from anywhere else, and it silently collapses two refs
// of one repository into the same label.
func readStoreName(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "store.json"))
	if err != nil {
		return ""
	}
	var meta struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return ""
	}
	return strings.TrimSpace(meta.Name)
}

func readCategories(root string) []string {
	b, err := os.ReadFile(filepath.Join(root, "category-list.json"))
	if err != nil {
		return nil
	}
	var raw []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, c := range raw {
		if c.Name != "" {
			out = append(out, c.Name)
		}
	}
	return out
}

func readRecommend(root string) []string {
	b, err := os.ReadFile(filepath.Join(root, "recommend-list.json"))
	if err != nil {
		return nil
	}
	var raw []struct {
		AppID string `json:"appid"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if r.AppID != "" {
			out = append(out, r.AppID)
		}
	}
	return out
}
