package xcomposeapp

import (
	"errors"
	"fmt"
	"strings"

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
// data in shape ID. It is either a script entry (script or file, with the images
// it runs pulled ahead with the app's own) or Blocked (a refusal).
type Migration struct {
	ID   int       `yaml:"id"`
	Name Localized `yaml:"name,omitempty"`

	// Script is the step's script entry; its Timeout defaults to
	// DefaultMigrationTimeout.
	Script `yaml:",inline"`

	// Blocked is the refusal form: Markdown shown to the operator, and an update
	// that would cross this step is refused before anything is pulled.
	Blocked Localized `yaml:"blocked,omitempty"`
}

// Label is how the step is named to the operator: its name, else its id.
func (m Migration) Label() string {
	if n := m.Name.Value(); n != "" {
		return n
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
	if err := checkMigrationsLocalized(ext); err != nil {
		return d, err
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

// ParseVersion reads only data_version from a base compose's x-compose-app block:
// 1 when the app declares none. It is for the compose already installed, whose
// version is all a start or an update plan needs from it. That file was checked when
// it came from the store, possibly by an older Maison with older rules, so nothing
// else in it is judged again here: a step in a format this build no longer accepts
// must not stop the app from starting, or from being updated out of that format.
func ParseVersion(ext map[string]any) (int, error) {
	v, ok := ext["data_version"]
	if !ok || v == nil {
		return 1, nil
	}
	n, ok := v.(int)
	if !ok || n < 1 {
		return 0, fmt.Errorf("x-compose-app data_version is %v, not a version of at least 1", v)
	}
	return n, nil
}

// checkMigrationsLocalized refuses a step whose name or blocked text is not a locale
// map with en_us. Unlike the rest of the block (localized.go) this is an error, not
// a drop: ParseData's error refuses the update, which is right here — dropping
// `blocked` would turn a refusal into a step with no body.
func checkMigrationsLocalized(ext map[string]any) error {
	steps, _ := ext["migrations"].([]any)
	for i, e := range steps {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		for _, k := range []string{"name", "blocked"} {
			if v := m[k]; v != nil && !validLocalized(v) {
				return fmt.Errorf("x-compose-app %s", localizedProblem(fmt.Sprintf("migrations[%d].%s", i, k)))
			}
		}
	}
	return nil
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
	if strings.TrimSpace(m.Blocked.Value()) != "" {
		if !m.Script.IsZero() || m.Images != nil || m.Timeout != "" {
			return errors.New("a blocked step takes nothing but id, name and blocked")
		}
		return nil
	}
	if m.Script.IsZero() {
		return errors.New("needs a script, a file or blocked")
	}
	return m.Script.Validate()
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
