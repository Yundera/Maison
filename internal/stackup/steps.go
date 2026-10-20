package stackup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// InitStateDir records which `run: once` steps an app has already run. It sits
// inside the app folder, so it travels with the app's backup and an app restored
// from one does not re-seed a database it already has. (The name predates steps;
// it is kept so the markers installs already carry stay valid.)
const InitStateDir = ".init"

// RunSteps runs the app's steps declared `at` the given point, in declared order,
// threading what each writes to $MAISON_OUTPUT into outputs — for the later steps,
// and for the seed tree and `files` rendered after them.
//
// At AtSetup the first failure is returned: the stack must not start on a
// precondition that does not hold. At AtStarted every step runs and the failures
// are returned together; the caller logs them rather than taking a running stack
// down. A failed step records no `once` marker either way, so the next up retries
// it.
func RunSteps(ctx context.Context, cfg config.Config, project, dir, at string, steps []xcomposeapp.Step, outputs map[string]string) error {
	var errs []error
	for _, step := range steps {
		if step.At != at {
			continue
		}
		if err := runStep(ctx, cfg, project, dir, step, outputs); err != nil {
			err = fmt.Errorf("step %s: %w", stepLabel(step), err)
			if at == xcomposeapp.AtSetup {
				return err
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func runStep(ctx context.Context, cfg config.Config, project, dir string, step xcomposeapp.Step, outputs map[string]string) error {
	if err := step.Validate(); err != nil {
		return err
	}
	var marker string
	if step.RunMode() == xcomposeapp.RunOnce {
		marker = filepath.Join(dir, InitStateDir, markerName(step.Name))
		if fileExists(marker) {
			return nil
		}
	}
	res, err := RunScript(ctx, cfg, project, dir, step.Script, ScriptOpts{
		Label:          "step " + step.Name,
		DefaultTimeout: xcomposeapp.DefaultStepTimeout,
		Outputs:        outputs,
	})
	if err != nil {
		return err
	}
	for k, v := range res.Output {
		outputs[k] = v
	}
	if marker != "" {
		return writeMarker(marker)
	}
	return nil
}

func stepLabel(s xcomposeapp.Step) string {
	if n := strings.TrimSpace(s.Name); n != "" {
		return n
	}
	return "(unnamed)"
}

// markerName makes a step name safe to use as a filename, so a step called
// "seed db" or one with a slash in it cannot write outside InitStateDir.
func markerName(name string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, name)
	return strings.Trim(safe, ".-")
}

func writeMarker(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), DefaultFolderMode); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0o644)
}
