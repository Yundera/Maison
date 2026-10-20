package stackup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yundera/maison/internal/composefile"
	"github.com/yundera/maison/internal/incident"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// DataVersionFile is the marker that records which data_version an app's data is
// in. It lives in the app folder, beside .init/, so it travels with the data it
// describes: a rollback or a restore brings back a marker that agrees with the data
// and the compose it came with. See docs/migrations.md.
//
// The compose declares where the data is going; the marker records where it is.
const DataVersionFile = ".data-version"

// ReadDataVersion returns the app's marker. ok is false when there is none — every
// install that predates migrations.
func ReadDataVersion(dir string) (v int, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, DataVersionFile))
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	v, err = strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || v < 1 {
		return 0, false, fmt.Errorf("%s holds %q, not a data version", DataVersionFile, strings.TrimSpace(string(b)))
	}
	return v, true, nil
}

// WriteDataVersion records v as the app's data version, atomically: a reader sees
// the old value or the new one, never a torn file.
func WriteDataVersion(dir string, v int) error {
	tmp, err := os.CreateTemp(dir, DataVersionFile+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.WriteString(strconv.Itoa(v) + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, DataVersionFile))
}

// BaseVersion reads data_version from the app's base compose — the store's file in
// dir, never the override. Only the version: the installed compose is not re-judged
// against this build's manifest rules (see xcomposeapp.ParseVersion).
func BaseVersion(dir string) (int, error) {
	f, err := composefile.Load(filepath.Join(dir, "docker-compose.yml"))
	if err != nil {
		return 0, err
	}
	return xcomposeapp.ParseVersion(f.XComposeApp)
}

// InstalledDataVersion is where the data in dir is: the marker, or — for an
// install that has none — the data_version of the compose on disk, which is 1 when
// it declares none. That baseline is only true of the compose the data was last run
// with, so install and update read it before the store's compose replaces that
// file. ok is false for a folder holding neither: nothing is installed there.
func InstalledDataVersion(dir string) (v int, ok bool, err error) {
	if v, ok, err := ReadDataVersion(dir); err != nil || ok {
		return v, ok, err
	}
	cv, err := BaseVersion(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("data version of the installed compose: %w", err)
	}
	return cv, true, nil
}

// DataVersionError is a start refused because the app's data and its compose
// disagree about which version the data is in.
type DataVersionError struct {
	Data, Compose int
}

func (e *DataVersionError) Error() string {
	if e.Data > e.Compose {
		return fmt.Sprintf("the app's data is at version %d, newer than this compose (%d): starting it would run older code on data it was not written for", e.Data, e.Compose)
	}
	return fmt.Sprintf("the app's data is at version %d but this compose expects %d: its migrations have not run, and only an update runs them", e.Data, e.Compose)
}

// CheckDataVersion refuses a stack whose data is not in the shape its compose
// expects. Through Maison's own flows the two always agree — install and update
// write the marker, a restore brings back a marker and a compose together — so
// this fires on a folder edited by hand or a compose swapped outside Maison: the
// cases where starting anyway runs code against data it was not written for.
//
// With no marker it cannot fail: the baseline is the compose on disk itself, so
// every install that predates migrations starts exactly as before.
func CheckDataVersion(dir string) error {
	v, ok, err := ReadDataVersion(dir)
	if err != nil || !ok {
		return err
	}
	cv, err := BaseVersion(dir)
	if err != nil {
		return fmt.Errorf("data version of the compose: %w", err)
	}
	if v != cv {
		return &DataVersionError{Data: v, Compose: cv}
	}
	return nil
}

// dataIncidentID is one per app: the marker and the compose disagree once, not
// once per attempt to start.
func dataIncidentID(project string) string { return "app.data:" + project }

func dataIncident(project string, err error) incident.Report {
	return incident.Report{
		ID:       dataIncidentID(project),
		Kind:     incident.KindAppStackup,
		Severity: incident.Warning,
		Title:    project + " was not started: its data does not match its version",
		Detail: err.Error() + "\n\nNothing was started. " + DataVersionFile + " in the app's folder records the data's version; " +
			"the compose's x-compose-app data_version is the version it expects. Update the app, or restore a backup taken with this version.",
		Args: map[string]string{"app": project},
	}
}
