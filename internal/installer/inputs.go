package installer

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/yundera/maison/internal/xcomposeapp"
)

// Options are what an install request carries besides the app reference.
type Options struct {
	// From, when set, restores that backup as the app's folder first.
	From BackupRef
	// AcceptRequirements installs an app this box falls short of, after the
	// operator was told (see StartInstall).
	AcceptRequirements bool
	// Inputs are the values the operator typed for the app's declared inputs
	// (x-compose-app.inputs), keyed by variable name. Blank values are skipped:
	// the install dialog may be left empty and filled in later from the tile.
	Inputs map[string]string
}

// ErrBadInput is returned for an install that carries a value the app did not
// ask for, or one .env could not hold.
var ErrBadInput = errors.New("invalid install input")

// checkInputs refuses values for keys the app does not declare — the install
// dialog only offers declared ones, so anything else is a client bug or someone
// trying to set an arbitrary variable through the store — and values .env cannot
// hold (a newline would read back as a second, bogus entry).
func checkInputs(declared []xcomposeapp.Input, got map[string]string) error {
	if len(got) == 0 {
		return nil
	}
	ok := make(map[string]bool, len(declared))
	for _, in := range declared {
		ok[in.Env] = true
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !ok[k] {
			return fmt.Errorf("%w: %s is not one of this app's inputs", ErrBadInput, k)
		}
		if strings.ContainsAny(got[k], "\r\n") {
			return fmt.Errorf("%w: %s may not span several lines", ErrBadInput, k)
		}
	}
	return nil
}

// filledInputs returns the non-blank values, trimmed.
func filledInputs(got map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range got {
		if v = strings.TrimSpace(v); v != "" {
			out[k] = v
		}
	}
	return out
}
