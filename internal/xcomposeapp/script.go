package xcomposeapp

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// ScriptsDir is the store app's folder of script files, which a script entry
// names with `file: scripts/<name>.sh`. It is copied with the app on install and
// update, like the seed tree.
const ScriptsDir = "scripts"

// Default timeouts of a script entry, by where it is declared. A migration may
// legitimately dump and restore a large database; a step or a credential change
// that runs this long is hung.
const (
	DefaultStepTimeout             = 10 * time.Minute
	DefaultMigrationTimeout        = 2 * time.Hour
	DefaultCredentialChangeTimeout = 2 * time.Minute
)

// Script is a script entry: the one shape of app-supplied code Maison runs, used
// alike by a step, a migration step and a credential change. Where it is declared
// decides only when it runs; what it can do is the same everywhere. See
// docs/x-compose-app.md (The script entry).
type Script struct {
	// Script is inline bash.
	Script string `yaml:"script,omitempty"`
	// File is a path under the store app's scripts/ folder, in place of Script.
	File string `yaml:"file,omitempty"`
	// Images are the images the script runs, pulled with the app's own before
	// anything is stopped. Optional.
	Images []string `yaml:"images,omitempty"`
	// Timeout is a Go duration; empty takes the default of where it is declared.
	Timeout string `yaml:"timeout,omitempty"`
	// Lenient runs the script without `-euo pipefail`. Never read from YAML: only
	// a CasaOS install command translated into a step sets it, because those were
	// written for a plain `bash -c`.
	Lenient bool `yaml:"-"`
}

// IsZero reports a script entry that declares nothing to run.
func (s Script) IsZero() bool {
	return strings.TrimSpace(s.Script) == "" && strings.TrimSpace(s.File) == ""
}

// Validate checks the entry's own keys: exactly one of script and file, a file
// that stays under scripts/, and a positive timeout.
func (s Script) Validate() error {
	hasScript, hasFile := strings.TrimSpace(s.Script) != "", strings.TrimSpace(s.File) != ""
	switch {
	case hasScript && hasFile:
		return errors.New("declares both script and file")
	case !hasScript && !hasFile:
		return errors.New("declares neither script nor file")
	}
	if hasFile {
		if !filepath.IsLocal(s.File) || !strings.HasPrefix(filepath.ToSlash(filepath.Clean(s.File)), ScriptsDir+"/") {
			return fmt.Errorf("file %q is not under %s/", s.File, ScriptsDir)
		}
	}
	if s.Timeout != "" {
		if d, err := time.ParseDuration(s.Timeout); err != nil || d <= 0 {
			return fmt.Errorf("timeout %q is not a positive duration", s.Timeout)
		}
	}
	return nil
}

// TimeoutOr is the entry's timeout, or def when it declares none. Call Validate
// first: an unparseable value also falls back to def.
func (s Script) TimeoutOr(def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s.Timeout); err == nil && d > 0 {
		return d
	}
	return def
}

// Where a step runs, and how often.
const (
	// AtSetup runs the step before the stack starts: after secrets and
	// variables, before the seed tree and files. A failure is fatal.
	AtSetup = "setup"
	// AtStarted runs it after `docker compose up` returns. A failure is logged
	// and retried on the next up.
	AtStarted = "started"

	// RunOnce runs the step until it succeeds once, remembered in .init/<name>.
	RunOnce = "once"
	// RunAlways runs it on every up.
	RunAlways = "always"
)

// Step is a script entry run around the app's stack on every up.
type Step struct {
	Name   string `yaml:"name,omitempty"`
	At     string `yaml:"at,omitempty"`
	Run    string `yaml:"run,omitempty"`
	Script `yaml:",inline"`
}

// RunMode is Run, defaulting to RunOnce.
func (s Step) RunMode() string {
	if s.Run == "" {
		return RunOnce
	}
	return s.Run
}

// Validate checks the step's own keys and its script entry.
func (s Step) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("has no name")
	}
	if s.At != AtSetup && s.At != AtStarted {
		return fmt.Errorf("at %q is not %s or %s", s.At, AtSetup, AtStarted)
	}
	if m := s.RunMode(); m != RunOnce && m != RunAlways {
		return fmt.Errorf("run %q is not %s or %s", s.Run, RunOnce, RunAlways)
	}
	return s.Script.Validate()
}

// ValidateSteps checks every step and that names are unique — a `once` marker is
// keyed by name, so two steps sharing one would skip each other.
func ValidateSteps(steps []Step) []error {
	var errs []error
	seen := map[string]bool{}
	for i, s := range steps {
		label := s.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i+1)
		}
		if err := s.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("step %s: %w", label, err))
		}
		if s.Name != "" {
			if seen[s.Name] {
				errs = append(errs, fmt.Errorf("step %s: name used twice", s.Name))
			}
			seen[s.Name] = true
		}
	}
	return errs
}
