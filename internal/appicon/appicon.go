// Package appicon owns an app's icon *file* — the copy Maison keeps inside the
// app folder, beside its docker-compose.yml.
//
// A store app declares its icon as a URL, almost always on the store's CDN. Left
// at that, every dashboard tile is a request to a third party: the grid loses its
// icons when the box is offline, when the CDN is blocked, and permanently when
// the store repo moves or the file behind that URL is deleted. So the icon is
// copied in at install exactly as the compose file and the seed tree are (see
// docs/app-model.md) — an installed app carries everything it needs to render
// itself — and the dashboard points its tiles at /api/apps/<app>/icon.
//
// The copy is the app's own: it travels with the app's backup, survives the store
// being removed, and is refreshed only when the app is installed or updated.
package appicon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// FileBase is the icon's name inside the app folder, sans extension. It is
// dot-prefixed like everything else Maison owns in there (.env, .seed, .init), so
// it cannot collide with a file the app itself keeps at the root of its folder.
const FileBase = ".icon"

const (
	// maxBytes caps what is copied or downloaded. An app icon is a few tens of KB;
	// this is loose enough for a detailed PNG and tight enough that a store cannot
	// fill the data disk through the icon field.
	maxBytes = 4 << 20
	// fetchTimeout bounds the download of one icon. It is deliberately short: an
	// install must not hang on an unreachable CDN, and the tile has a working
	// fallback (the URL itself) if this never succeeds.
	fetchTimeout = 15 * time.Second
)

// exts are the image extensions accepted, in the order Path looks for them. An
// extension is what tells the browser how to render the file — the server hands
// it to http.ServeFile — so anything not on this list is not stored at all.
var exts = []string{".png", ".svg", ".jpg", ".jpeg", ".webp", ".gif", ".ico", ".avif"}

// contentTypeExt maps a response's Content-Type to an extension, for a URL whose
// path carries none (".../icon", a query-string image route).
var contentTypeExt = map[string]string{
	"image/png":                ".png",
	"image/svg+xml":            ".svg",
	"image/jpeg":               ".jpg",
	"image/webp":               ".webp",
	"image/gif":                ".gif",
	"image/avif":               ".avif",
	"image/x-icon":             ".ico",
	"image/vnd.microsoft.icon": ".ico",
}

// Path returns the app folder's icon file, or "" when it holds none.
func Path(appDir string) string {
	for _, ext := range exts {
		p := filepath.Join(appDir, FileBase+ext)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// URL is where the dashboard reads an app's copied icon from. Relative to the
// origin so it resolves both on the dashboard host and on an app host Maison is
// standing in for (see the launch gate).
func URL(project string) string {
	return "/api/apps/" + url.PathEscape(project) + "/icon"
}

// Write copies the app's icon into appDir as FileBase+ext, replacing whatever
// copy was there.
//
// The source is preferably storeDir — the app's folder in the extracted store,
// which ships the icon beside the compose file — because that is a local read of
// bytes already downloaded. Only when the store has no such file does it fall
// back to fetching iconURL, which is the case for a store whose icon points at
// somebody else's server.
//
// Finding no icon at all is not an error: the app simply keeps none, and the
// dashboard falls back to the URL in its compose. Errors are for a source that
// existed and could not be read.
func Write(ctx context.Context, appDir, storeDir, iconURL string) error {
	data, ext := fromStore(storeDir, iconURL)
	if data == nil {
		var err error
		data, ext, err = fetch(ctx, iconURL)
		if err != nil {
			return err
		}
	}
	if data == nil {
		return nil // nothing to copy — leave the folder as it is
	}
	// Clear first: the new icon may have a different extension from the old one,
	// and two .icon.* files would make Path's answer depend on its own ordering.
	if err := Clear(appDir); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(appDir, FileBase+ext), data, 0o644)
}

// Clear removes the app folder's icon copy, whatever extension it has. The
// dashboard then falls back to the icon URL in the app's compose — which is also
// how an operator asks for the copy to be taken again (EnsureIcons refills it).
func Clear(appDir string) error {
	for _, ext := range exts {
		if err := os.Remove(filepath.Join(appDir, FileBase+ext)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// fromStore reads the icon out of the app's folder in the extracted store: the
// file the icon URL names, falling back to a plain icon.<ext> — the CasaOS store
// layout, where every app ships one. Returns nil when the store has no icon file,
// which is not a failure (Write then fetches).
func fromStore(storeDir, iconURL string) ([]byte, string) {
	if storeDir == "" {
		return nil, ""
	}
	var names []string
	if base := urlBase(iconURL); base != "" {
		names = append(names, base)
	}
	for _, ext := range exts {
		names = append(names, "icon"+ext)
	}
	for _, name := range names {
		ext := extOf(name)
		if ext == "" {
			continue
		}
		// filepath.Base pins the read inside storeDir: the name is derived from a
		// URL the store wrote, and "../../etc/x.png" is a name.
		b, err := readCapped(filepath.Join(storeDir, filepath.Base(name)))
		if err != nil {
			continue
		}
		return b, ext
	}
	return nil, ""
}

// fetch downloads iconURL. A non-http(s) URL (or none) yields no icon and no
// error — there is nothing to fetch, and an app without an icon is ordinary.
func fetch(ctx context.Context, iconURL string) ([]byte, string, error) {
	u, err := url.Parse(strings.TrimSpace(iconURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch %s: %w", u, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch %s: %s", u, res.Status)
	}
	ext := extOf(urlBase(iconURL))
	if ext == "" {
		ct, _, _ := strings.Cut(res.Header.Get("Content-Type"), ";")
		ext = contentTypeExt[strings.ToLower(strings.TrimSpace(ct))]
	}
	if ext == "" {
		return nil, "", fmt.Errorf("fetch %s: not a recognised image", u)
	}
	// One byte over the cap is read so an oversized icon is refused rather than
	// silently truncated into a broken file.
	b, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("fetch %s: %w", u, err)
	}
	if len(b) > maxBytes {
		return nil, "", fmt.Errorf("fetch %s: icon is larger than %d bytes", u, maxBytes)
	}
	return b, ext, nil
}

// readCapped reads a file, refusing one over maxBytes.
func readCapped(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	if st.Size() > maxBytes {
		return nil, fmt.Errorf("%s: icon is larger than %d bytes", path, maxBytes)
	}
	return os.ReadFile(path)
}

// urlBase is the last path segment of a URL (or of a bare filename).
func urlBase(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Path != "" {
		raw = u.Path
	}
	return path.Base(raw)
}

// extOf returns name's extension when it is one we store, lowercased, else "".
func extOf(name string) string {
	ext := strings.ToLower(path.Ext(name))
	for _, e := range exts {
		if ext == e {
			return ext
		}
	}
	return ""
}
