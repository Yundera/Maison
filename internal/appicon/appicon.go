// Package appicon owns an app's icon *file* — the copy Maison keeps inside the
// app folder, beside its docker-compose.yml.
//
// An app names its icon beside its compose (see internal/asset), and the file is
// copied in at install exactly as the compose file and the seed tree are (see
// docs/app-model.md) — an installed app carries everything it needs to render
// itself — with the dashboard pointing its tiles at /api/apps/<app>/icon.
//
// The copy is what makes the tile independent of the store: resolving the icon
// into the extracted store tree at render time would work, right up until the
// store is refreshed, removed, or the app is restored onto a box that never had
// it. An icon named as a URL instead is downloaded once, here, for the same
// reason — a tile must not be a request to a third party that can be offline,
// blocked, or gone for good.
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

	"github.com/yundera/maison/internal/asset"
)

// FileBase is the icon's name inside the app folder, sans extension. It is
// dot-prefixed like everything else Maison owns in there (.env, .seed, .init), so
// it cannot collide with a file the app itself keeps at the root of its folder.
const FileBase = ".icon"

// fetchTimeout bounds the download of one icon. It is deliberately short: an
// install must not hang on an unreachable host, and an app whose icon never
// arrives is an ordinary app with no icon.
const fetchTimeout = 15 * time.Second

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
	for _, ext := range asset.Exts {
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
// rel is the icon as a path inside srcDir, for the ordinary case where the app
// names a file beside its compose; iconURL is the fallback for a compose that
// names somebody else's server instead. Disk wins: it is a read of bytes already
// on this box, it cannot fail halfway, and it is the only source that still works
// with no egress.
//
// Finding no icon at all is not an error: the app simply keeps none, and the
// dashboard falls back to the URL in its compose. Errors are for a source that
// existed and could not be read.
func Write(ctx context.Context, appDir, srcDir, rel, iconURL string) error {
	data, ext := fromDir(srcDir, rel)
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
	for _, ext := range asset.Exts {
		if err := os.Remove(filepath.Join(appDir, FileBase+ext)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// fromDir reads the icon out of srcDir — the app's folder in the extracted store
// at install, or the app's own folder when an already-installed app is having its
// copy taken. It tries the file the compose named, then a plain icon.<ext>: the
// CasaOS layout, where every app ships one under that name whether or not it says
// so. Returns nil when there is no such file, which is not a failure (Write then
// fetches).
func fromDir(srcDir, rel string) ([]byte, string) {
	if srcDir == "" {
		return nil, ""
	}
	var names []string
	if rel != "" {
		names = append(names, rel)
	}
	for _, ext := range asset.Exts {
		names = append(names, "icon"+ext)
	}
	for _, name := range names {
		// Re-validated rather than trusted: rel reaches here from a store's compose,
		// and asset.Rel is what keeps "../../etc/x.png" from being a name.
		clean, ok := asset.Rel(name)
		if !ok {
			continue
		}
		b, err := readCapped(filepath.Join(srcDir, filepath.FromSlash(clean)))
		if err != nil {
			continue
		}
		return b, asset.Ext(clean)
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
	ext := asset.Ext(urlBase(iconURL))
	if ext == "" {
		ct, _, _ := strings.Cut(res.Header.Get("Content-Type"), ";")
		ext = contentTypeExt[strings.ToLower(strings.TrimSpace(ct))]
	}
	if ext == "" {
		return nil, "", fmt.Errorf("fetch %s: not a recognised image", u)
	}
	// One byte over the cap is read so an oversized icon is refused rather than
	// silently truncated into a broken file.
	b, err := io.ReadAll(io.LimitReader(res.Body, asset.MaxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("fetch %s: %w", u, err)
	}
	if len(b) > asset.MaxBytes {
		return nil, "", fmt.Errorf("fetch %s: icon is larger than %d bytes", u, asset.MaxBytes)
	}
	return b, ext, nil
}

// readCapped reads a file, refusing one over asset.MaxBytes.
func readCapped(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	if st.Size() > asset.MaxBytes {
		return nil, fmt.Errorf("%s: icon is larger than %d bytes", path, asset.MaxBytes)
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
