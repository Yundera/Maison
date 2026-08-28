package appstore

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/yundera/maison/internal/asset"
)

// AssetURL is where the dashboard reads one of a store app's compose-relative
// assets from — the browser cannot read the extracted store tree, so a file
// beside the compose has to be handed out over the API to be rendered.
//
// The shape mirrors the other store endpoints: the app id in the path, the store
// locator and apps folder in the query (see storeRef in the server package), so
// an asset of an app in an unlisted store addresses exactly as the app itself
// does. The locator is always written out, even for an app that is in the merged
// catalog, because a tile must not stop resolving because some other store later
// won the same id.
func AssetURL(ref Ref, rel string) string {
	segs := strings.Split(rel, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	u := "/api/store/" + url.PathEscape(ref.ID) + "/asset/" + strings.Join(segs, "/")
	q := url.Values{}
	if !ref.Merged() {
		q.Set("store", ref.URL)
	}
	// Only when it is not the default: the URL is read by people debugging a
	// missing tile, and an apps_path that says "Apps" says nothing.
	if ref.AppsPath != "" && ref.AppsPath != DefaultAppsPath {
		q.Set("apps_path", ref.AppsPath)
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// OpenAsset opens one compose-relative asset of the app named by ref, for the
// handler that serves it.
//
// It never fetches. Every other read of an unlisted store may download it on
// first sight, which is right for a deep link a person followed; an asset request
// is an <img> the page emitted, and a store sync runs to tens of MB and ninety
// seconds. An asset of a store this box has not extracted is simply not found.
//
// The file is opened rather than located because the store tree is replaced by
// rename under filesMu: handing back a path would leave the caller opening it
// after the swap, which is the one moment the path means nothing. An open file
// survives the rename.
func (m *Manager) OpenAsset(ref Ref, rel string) (*os.File, os.FileInfo, error) {
	rel, ok := asset.Rel(rel)
	if !ok {
		return nil, nil, fmt.Errorf("%q is not an app asset", rel)
	}
	// The id is joined onto a directory below, unlike everywhere else in this
	// package where it is matched against a parsed store's app names. It comes off
	// the request path, so it gets the same treatment the asset name just got.
	if ref.ID == "" || strings.ContainsAny(ref.ID, `/\`) || strings.Contains(ref.ID, "..") {
		return nil, nil, fmt.Errorf("app %q not found", ref.ID)
	}

	m.filesMu.RLock()
	defer m.filesMu.RUnlock()

	dir, err := m.appDir(ref)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("%s: not a regular file", rel)
	}
	if st.Size() > asset.MaxBytes {
		f.Close()
		return nil, nil, fmt.Errorf("%s: asset is larger than %d bytes", rel, asset.MaxBytes)
	}
	return f, st, nil
}

// appDir is the folder holding the compose of the app named by ref, in a store
// already on disk. Callers hold filesMu.
func (m *Manager) appDir(ref Ref) (string, error) {
	if ref.Merged() {
		m.mu.RLock()
		app := m.catalog[ref.ID]
		m.mu.RUnlock()
		if app == nil {
			return "", fmt.Errorf("app %q not found", ref.ID)
		}
		return app.Dir(), nil
	}
	root, err := findStoreRoot(m.workdir(ref.URL), ref.Apps())
	if err != nil {
		return "", err
	}
	// Straight from the layout rather than by parsing the store: an asset request
	// only needs to know where the app's folder is, and parseStore walks every app
	// in the store and loads every compose to answer that.
	dir := filepath.Join(root, filepath.FromSlash(ref.Apps()), ref.ID)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("app %q not found in %s/ of store %s", ref.ID, ref.Apps(), ref.URL)
	}
	return dir, nil
}
