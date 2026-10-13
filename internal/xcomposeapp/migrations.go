package xcomposeapp

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Data is the part of an app's manifest that describes its *data*: which shape the
// data is converted to, and the steps that convert it. See docs/migrations.md.
//
// It is read apart from App, and from the base compose only. Apart, because Parse's
// error is discarded by every caller and degrades to "no x-compose-app" — the right
// answer for a title or a route, and the wrong one here: a migrations block Maison
// cannot read must refuse an update, not become "no migrations". From the base only,
// because the data's history belongs to the app, not to a box: an override cannot
// declare, skip or reorder a step.
type Data struct {
	// Version is data_version: 1 when the app declares none.
	Version    int
	Migrations []Migration
}

// Migration is one step of an app's data history: it turns data in shape ID-1 into
// data in shape ID. Exactly one of Image (a container), Script (a hook-like shell
// script) or Blocked (a refusal) is set.
type Migration struct {
	ID   int    `yaml:"id"`
	Name string `yaml:"name,omitempty"`

	// The container form: the `docker run` surface of an init step.
	Image      string   `yaml:"image,omitempty"`
	Entrypoint StrList  `yaml:"entrypoint,omitempty"`
	Command    StrList  `yaml:"command,omitempty"`
	User       string   `yaml:"user,omitempty"`
	Env        EnvList  `yaml:"environment,omitempty"`
	Volumes    []string `yaml:"volumes,omitempty"`
	Network    string   `yaml:"network,omitempty"`

	// The script form: a path inside the store app's folder, and the images it
	// will `docker run`, pulled ahead with the app's own.
	Script string   `yaml:"script,omitempty"`
	Images []string `yaml:"images,omitempty"`

	// Blocked is the refusal form: Markdown shown to the operator, and an update
	// that would cross this step is refused before anything is pulled.
	Blocked string `yaml:"blocked,omitempty"`

	// Timeout is a Go duration, DefaultMigrationTimeout when empty.
	Timeout string `yaml:"timeout,omitempty"`
}

// DefaultMigrationTimeout bounds a step that declares no timeout: a dump/restore
// of a large database is legitimately long, a hang is not.
const DefaultMigrationTimeout = 2 * time.Hour

// Label is how the step is named to the operator: its name, else its id.
func (m Migration) Label() string {
	if m.Name != "" {
		return m.Name
	}
	return fmt.Sprintf("step %d", m.ID)
}

// ParseData reads data_version and migrations from a base compose's x-compose-app
// block. A missing block is data version 1 with no steps — every app that predates
// the feature. schema_version is deliberately not consulted: a build that cannot
// render a newer manifest must still know which data it would be starting.
//
// The result is validated (see Validate), and any error is returned rather than
// defaulted, for the reason given on Data.
func ParseData(ext map[string]any) (Data, error) {
	d := Data{Version: 1}
	if ext == nil {
		return d, nil
	}
	b, err := yaml.Marshal(ext)
	if err != nil {
		return d, err
	}
	var raw struct {
		DataVersion *int        `yaml:"data_version"`
		Migrations  []Migration `yaml:"migrations"`
	}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return d, fmt.Errorf("x-compose-app data_version/migrations: %w", err)
	}
	if raw.DataVersion != nil {
		d.Version = *raw.DataVersion
	}
	d.Migrations = raw.Migrations
	return d, d.Validate()
}

// Validate checks the store author's half of the contract: a data_version of at
// least 1, and exactly one well-formed step for each of 2 … data_version.
func (d Data) Validate() error {
	if d.Version < 1 {
		return fmt.Errorf("data_version must be at least 1, got %d", d.Version)
	}
	seen := make(map[int]bool, len(d.Migrations))
	for _, m := range d.Migrations {
		if m.ID < 2 || m.ID > d.Version {
			return fmt.Errorf("migration id %d is outside 2…%d (data_version)", m.ID, d.Version)
		}
		if seen[m.ID] {
			return fmt.Errorf("migration id %d is declared twice", m.ID)
		}
		seen[m.ID] = true
		if err := m.validate(); err != nil {
			return fmt.Errorf("migration %d: %w", m.ID, err)
		}
	}
	for id := 2; id <= d.Version; id++ {
		if !seen[id] {
			return fmt.Errorf("data_version is %d but no migration has id %d", d.Version, id)
		}
	}
	return nil
}

func (m Migration) validate() error {
	forms := 0
	for _, set := range []bool{m.Image != "", m.Script != "", strings.TrimSpace(m.Blocked) != ""} {
		if set {
			forms++
		}
	}
	if forms != 1 {
		return errors.New("needs exactly one of image, script or blocked")
	}
	container := m.Entrypoint != nil || m.Command != nil || m.User != "" || m.Env != nil || m.Volumes != nil || m.Network != ""
	switch {
	case m.Image != "":
		if m.Images != nil {
			return errors.New("images belongs to a script step; a container step pulls its own image")
		}
	case m.Script != "":
		if container {
			return errors.New("a script step takes no container keys (entrypoint, command, user, environment, volumes, network)")
		}
		if !filepath.IsLocal(m.Script) {
			return fmt.Errorf("script %q must be a relative path inside the app's folder", m.Script)
		}
	default: // blocked
		if container || m.Images != nil || m.Timeout != "" {
			return errors.New("a blocked step takes nothing but id, name and blocked")
		}
	}
	if m.Timeout != "" {
		if d, err := time.ParseDuration(m.Timeout); err != nil || d <= 0 {
			return fmt.Errorf("timeout %q is not a positive Go duration", m.Timeout)
		}
	}
	return nil
}

// ErrDataNewer marks data written by a newer version than the compose on hand:
// starting or updating to that compose would run old code on new data.
var ErrDataNewer = errors.New("the app's data is newer than this version")

// Pending returns the steps that take data at version from to d.Version, in order.
// It returns ErrDataNewer when from is above d.Version. d must be valid.
func (d Data) Pending(from int) ([]Migration, error) {
	if from > d.Version {
		return nil, fmt.Errorf("%w: data is at version %d, this version expects %d", ErrDataNewer, from, d.Version)
	}
	byID := make(map[int]Migration, len(d.Migrations))
	for _, m := range d.Migrations {
		byID[m.ID] = m
	}
	var out []Migration
	for id := from + 1; id <= d.Version; id++ {
		out = append(out, byID[id])
	}
	return out, nil
}
