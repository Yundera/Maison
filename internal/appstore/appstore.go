// Package appstore fetches CasaOS-compatible app stores (GitHub zip archives),
// extracts them, and builds a merged catalog of installable apps keyed by app
// id. Layout ported from casa-img: <root>/Apps/<name>/docker-compose.yml plus
// category-list.json and recommend-list.json.
package appstore

import (
	"archive/zip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/xcasaos"
)

// CatalogApp is one installable app from a store.
type CatalogApp struct {
	ID          string   `json:"id"`   // compose project name (Apps/<name>)
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

	composePath string // absolute path to the app's compose file
}

// Manager holds the merged catalog across all configured stores.
type Manager struct {
	urls     []string
	cacheDir string

	mu        sync.RWMutex
	catalog   map[string]*CatalogApp
	order     []string // stable catalog order
	cats      []string
	recommend []string
}

// New creates a Manager for the given store URLs, caching under cacheDir.
func New(urls []string, cacheDir string) *Manager {
	return &Manager{
		urls:     urls,
		cacheDir: cacheDir,
		catalog:  map[string]*CatalogApp{},
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
	m.urls = append([]string(nil), urls...)
	m.mu.Unlock()
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
	raw, err := os.ReadFile(app.composePath)
	if err != nil {
		return nil, nil, err
	}
	return app, raw, nil
}

// GetFrom returns app id as it stands in store storeURL, along with its raw
// compose bytes. The app need not be in the merged catalog at all — storeURL may
// be a store the user has never added — which is what lets a deep link
// (/store/<id>?store=<url>) address an app in an unlisted store. When storeURL is
// empty the merged catalog answers (Get).
//
// An already-extracted copy of the store answers as-is — including "no such app",
// so a bad id fails fast; only a store that has never been fetched is downloaded
// here. Browsing must not pay for a sync: stores run to tens of MB and a
// re-download would stall the detail page for minutes. Configured stores are kept
// fresh by the hourly Refresh; an unlisted store is fetched once, on the first
// deep link that names it, and thereafter refreshed only on demand (RefreshStore)
// or when the update flow syncs it (AppComposeFrom).
func (m *Manager) GetFrom(ctx context.Context, storeURL, id string) (*CatalogApp, []byte, error) {
	if strings.TrimSpace(storeURL) == "" {
		return m.Get(id)
	}
	root, err := findAppsRoot(m.workdir(storeURL))
	if err != nil {
		if root, err = m.syncStore(ctx, storeURL); err != nil {
			return nil, nil, err
		}
	}
	return appIn(root, storeURL, id)
}

// AppComposeFrom returns the raw docker-compose.yml bytes for app id as it
// currently stands in store storeURL. Unlike GetFrom it always syncs the store
// first: the update flow diffs the store's live version against what's installed,
// so a stale extracted copy would report "up to date" when it isn't.
func (m *Manager) AppComposeFrom(ctx context.Context, storeURL, id string) ([]byte, error) {
	if strings.TrimSpace(storeURL) == "" {
		_, raw, err := m.Get(id)
		return raw, err
	}
	root, err := m.syncStore(ctx, storeURL)
	if err != nil {
		return nil, err
	}
	_, raw, err := appIn(root, storeURL, id)
	return raw, err
}

// appIn finds app id in an extracted store root and reads its compose file.
func appIn(root, storeURL, id string) (*CatalogApp, []byte, error) {
	apps, _, _ := parseStore(root, storeURL)
	for _, a := range apps {
		if a.ID == id {
			raw, err := os.ReadFile(a.composePath)
			if err != nil {
				return nil, nil, err
			}
			return a, raw, nil
		}
	}
	return nil, nil, fmt.Errorf("app %q not found in store %s", id, storeURL)
}

// Refresh downloads and reparses every configured store.
func (m *Manager) Refresh(ctx context.Context) error {
	catalog := map[string]*CatalogApp{}
	var order []string
	catSet := map[string]bool{}
	var recommend []string

	for _, u := range m.URLs() {
		root, err := m.syncStore(ctx, u)
		if err != nil {
			// One bad store shouldn't sink the rest.
			continue
		}
		apps, cats, rec := parseStore(root, u)
		for _, a := range apps {
			if _, exists := catalog[a.ID]; exists {
				continue // first store wins on id collision
			}
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
	cats := make([]string, 0, len(catSet))
	for c := range catSet {
		cats = append(cats, c)
	}
	sort.Strings(cats)

	m.mu.Lock()
	m.catalog = catalog
	m.order = order
	m.cats = cats
	m.recommend = recommend
	m.mu.Unlock()
	return nil
}

// RefreshStore forces a re-download of a single store — dropping its cached
// validators so the conditional GET in syncStore can't come back 304 — then
// rebuilds the merged catalog. Other stores are re-synced too but skip their
// download when unchanged.
func (m *Manager) RefreshStore(ctx context.Context, storeURL string) error {
	clearValidators(m.workdir(storeURL))
	return m.Refresh(ctx)
}

// StartAutoRefresh refreshes now and then every interval until ctx is done.
func (m *Manager) StartAutoRefresh(ctx context.Context, interval time.Duration) {
	go func() {
		_ = m.Refresh(ctx)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = m.Refresh(ctx)
			}
		}
	}()
}

// syncStore brings the extracted copy of a store up to date and returns its
// store root (the parent of the Apps/ folder). An unchanged store costs one
// conditional GET that comes back 304 with no body — see fetch.
func (m *Manager) syncStore(ctx context.Context, storeURL string) (string, error) {
	workdir := m.workdir(storeURL)

	var lastErr error
	for _, dl := range storeZipCandidates(storeURL) {
		root, err := m.fetch(ctx, dl, workdir)
		if err != nil {
			lastErr = err
			continue
		}
		return root, nil
	}
	// Every candidate failed: fall back to any previously extracted copy.
	if root, ferr := findAppsRoot(workdir); ferr == nil {
		return root, nil
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
func (m *Manager) fetch(ctx context.Context, u, workdir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}

	// Only make the request conditional when there is actually a copy on disk to
	// fall back on, so a 304 can always be honoured by reusing it.
	if _, err := findAppsRoot(workdir); err == nil {
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
		return findAppsRoot(workdir)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("store %s: http %d", u, resp.StatusCode)
	}

	if err := extractStream(resp.Body, workdir); err != nil {
		return "", err
	}
	writeValidators(workdir, validators{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	})
	return findAppsRoot(workdir)
}

// extractStream spools r (a zip) to a temp file and extracts it into dest,
// replacing any prior copy. The spool file is what keeps the zip out of the heap.
func extractStream(r io.Reader, dest string) error {
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
	_ = os.RemoveAll(dest)
	return os.Rename(tmp, dest)
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
	u, err := url.Parse(storeURL)
	if err != nil {
		sum := md5.Sum([]byte(storeURL))
		return filepath.Join(m.cacheDir, hex.EncodeToString(sum[:]))
	}
	sum := md5.Sum([]byte(strings.ToLower(u.Path)))
	return filepath.Join(m.cacheDir, u.Host, hex.EncodeToString(sum[:]))
}

func headETag(ctx context.Context, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return "", err
	}
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	return resp.Header.Get("ETag"), nil
}

// download fetches a zip and extracts it into dest (replacing any prior copy).
func download(ctx context.Context, u, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("store %s: http %d", u, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(strings.NewReader(string(body)), int64(len(body)))
	if err != nil {
		return err
	}

	tmp := dest + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := extractZip(zr, tmp); err != nil {
		return err
	}
	_ = os.RemoveAll(dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
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
		out, err := os.Create(target)
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

// findAppsRoot locates the directory containing an "Apps" subfolder.
func findAppsRoot(dir string) (string, error) {
	var found string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() && d.Name() == "Apps" {
			found = filepath.Dir(path)
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("no Apps/ directory under %s", dir)
	}
	return found, nil
}

func parseStore(root, storeURL string) (apps []*CatalogApp, cats, recommend []string) {
	appsDir := filepath.Join(root, "Apps")
	entries, err := os.ReadDir(appsDir)
	if err != nil {
		return nil, nil, nil
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
		apps = append(apps, catalogApp(e.Name(), si, composePath, storeURL))
	}

	cats = readCategories(root)
	recommend = readRecommend(root)
	return apps, cats, recommend
}

func catalogApp(id string, si *xcasaos.StoreInfo, composePath, storeURL string) *CatalogApp {
	name := xcasaos.Localized(si.Title)
	if name == "" {
		name = id
	}
	return &CatalogApp{
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
		composePath: composePath,
	}
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
