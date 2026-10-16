// Package composefile is a light reader for docker-compose.yml files — just
// enough to extract an app's metadata and service names without pulling in the
// full compose engine.
package composefile

import (
	"errors"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/yundera/maison/internal/xcasaos"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// File is a minimal view of a compose project file.
type File struct {
	Name        string             `yaml:"name,omitempty"`
	Services    map[string]Service `yaml:"services"`
	XCasaOS     map[string]any     `yaml:"x-casaos"`
	XComposeApp map[string]any     `yaml:"x-compose-app"`
}

// Service is a minimal view of one compose service.
type Service struct {
	Image string `yaml:"image,omitempty"`
}

// Load parses a compose file from disk.
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse parses compose YAML bytes.
func Parse(b []byte) (*File, error) {
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// ComposeApp returns this file's own x-compose-app block, parsed, or an error if
// absent. It never falls back to x-casaos: it is for reading what Maison itself
// writes into an override (store-ref) or declares only natively (lifecycle).
// An app's metadata is App / Resolve.
func (f *File) ComposeApp() (*xcomposeapp.App, error) {
	return xcomposeapp.Parse(f.XComposeApp)
}

// App resolves the app's metadata from this file alone. See Resolve.
func (f *File) App() (*xcomposeapp.App, error) {
	return Resolve(f)
}

// Resolve returns an app's metadata from its compose files, in the order they are
// passed to `docker compose` (base first; nil entries are skipped).
//
// x-compose-app is Maison's definition of an app, and the two blocks never mix.
// The base decides which one the app speaks: its x-compose-app if it carries one,
// otherwise its x-casaos block translated whole (xcasaos.ComposeApp) — a compose
// carrying both is read as x-compose-app and its x-casaos ignored. A base whose
// x-compose-app declares a schema_version this build predates counts as carrying
// none.
//
// The later files — the override Maison and the operator write (store-ref, tips,
// a pinned webui-host) — layer their x-compose-app keys on top, key by key, in
// either case: that is operator state, not the app's authoring, and it is what
// keeps Maison's own bookkeeping from turning a CasaOS app into an empty native
// one. Their x-casaos blocks are never read.
//
// It returns xcomposeapp.ErrNoExtension when no file says anything.
func Resolve(files ...*File) (*xcomposeapp.App, error) {
	var base *File
	var over []*File
	for _, f := range files {
		switch {
		case f == nil:
		case base == nil:
			base = f
		default:
			over = append(over, f)
		}
	}
	if base == nil {
		return nil, xcomposeapp.ErrNoExtension
	}
	layer := func(ext map[string]any) (*xcomposeapp.App, error) {
		for _, f := range over {
			ext = merge(ext, f.XComposeApp)
		}
		return xcomposeapp.Parse(ext)
	}
	if base.XComposeApp != nil {
		a, err := layer(base.XComposeApp)
		if !errors.Is(err, xcomposeapp.ErrUnsupportedVersion) {
			return a, err
		}
	}
	var legacy *xcasaos.StoreInfo
	var ext map[string]any
	if si, err := xcasaos.Parse(base.XCasaOS); err == nil {
		legacy, ext = si, si.ComposeApp()
	}
	a, err := layer(ext)
	if err == nil && legacy != nil {
		a.Legacy = &xcomposeapp.Legacy{WebUIPort: legacy.WebUIPort}
	}
	return a, err
}

// merge layers over's keys on top of base (over wins). Either map may be nil.
func merge(base, over map[string]any) map[string]any {
	if over == nil {
		return base
	}
	if base == nil {
		return over
	}
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

// Data returns the app's data_version and migrations (see xcomposeapp.ParseData).
// Only meaningful on a base compose: an override has no say over the data's history.
func (f *File) Data() (xcomposeapp.Data, error) {
	return xcomposeapp.ParseData(f.XComposeApp)
}
