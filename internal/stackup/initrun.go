package stackup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/dockerx"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// InitStateDir records which `when: once` steps an app has already run. It sits
// inside the app folder, so it travels with the app's backup and an app restored
// from one does not re-seed a database it already has.
const InitStateDir = ".init"

// Runner runs one one-shot container. *dockerx.Client implements it; the
// indirection is what lets this package be tested without a daemon.
type Runner interface {
	RunOnce(ctx context.Context, spec dockerx.RunSpec) (string, error)
}

// newRunner connects to the daemon the same way everything else here does, via
// DOCKER_HOST. It is a variable so tests can substitute a fake, and it is called
// lazily so an app that declares no init steps never touches Docker outside
// `docker compose`.
var newRunner = func() (Runner, error) { return dockerx.New() }

// RunInit runs the app's init steps for one phase, threading each step's
// captured stdout into captures for everything rendered afterwards.
//
// This is the one part of the setup surface that still runs someone else's code,
// and it is deliberate: seeding filebrowser's database needs filebrowser's own
// binary, and obscuring an rclone password needs rclone. What it does not need
// is a shell — the step names an image, an argv and its mounts, so there is no
// PATH to be missing from, no `$(...)` to swallow a failure, and no quoting.
//
// A pre_up step that fails is fatal, like the pre_up hook: the app's stack must
// not start on a database that was never seeded. A post_up step is the caller's
// to log and swallow, for the same reason a post_up hook is.
func RunInit(ctx context.Context, cfg config.Config, project, dir, phase string, steps []xcomposeapp.InitStep, captures map[string]string) error {
	var due []xcomposeapp.InitStep
	for _, s := range steps {
		if stepPhase(s) == phase {
			due = append(due, s)
		}
	}
	if len(due) == 0 {
		return nil
	}
	runner, err := newRunner()
	if err != nil {
		return fmt.Errorf("connect to docker: %w", err)
	}
	for _, step := range due {
		if err := runStep(ctx, runner, cfg, project, dir, step, captures); err != nil {
			return fmt.Errorf("init %q: %w", stepName(step), err)
		}
	}
	return nil
}

func runStep(ctx context.Context, runner Runner, cfg config.Config, project, dir string, step xcomposeapp.InitStep, captures map[string]string) error {
	if strings.TrimSpace(step.Image) == "" {
		return fmt.Errorf("no image")
	}
	envFile := readEnvFile(dir)
	render := func(s string) (string, error) {
		return envinject.RenderStrict(s, cfg, project, envFile, captures)
	}

	run, marker, err := stepDue(cfg, project, dir, step, envFile)
	if err != nil || !run {
		return err
	}

	spec := dockerx.RunSpec{}
	if spec.Image, err = render(step.Image); err != nil {
		return err
	}
	if spec.User, err = render(step.User); err != nil {
		return err
	}
	if spec.Network, err = render(step.Network); err != nil {
		return err
	}
	if spec.Entrypoint, err = renderAll(render, step.Entrypoint); err != nil {
		return err
	}
	if spec.Cmd, err = renderAll(render, step.Command); err != nil {
		return err
	}
	if spec.Env, err = renderAll(render, step.Env); err != nil {
		return err
	}
	if spec.Binds, err = renderBinds(render, cfg, step.Volumes); err != nil {
		return err
	}

	out, err := runner.RunOnce(ctx, spec)
	if err != nil {
		return err
	}
	if step.Capture != "" {
		// Trimmed, because a value printed by a tool arrives with the newline the
		// tool's println added, and it is about to be substituted into a config
		// file where that newline would be a syntax error.
		captures[step.Capture] = strings.TrimSpace(out)
	}
	if marker != "" {
		if err := writeMarker(marker); err != nil {
			return err
		}
	}
	return nil
}

// stepDue evaluates the step's guard. It returns the marker path to write on
// success for a `once` step, and "" for the other guards, which keep no state.
func stepDue(cfg config.Config, project, dir string, step xcomposeapp.InitStep, envFile []byte) (run bool, marker string, err error) {
	when := strings.TrimSpace(step.When)
	if when == "" {
		when = xcomposeapp.WhenOnce
	}
	switch {
	case when == xcomposeapp.WhenAlways:
		return true, "", nil

	case when == xcomposeapp.WhenOnce:
		name := stepName(step)
		if name == "" {
			return false, "", fmt.Errorf("when: once needs a name to remember the step by")
		}
		marker = filepath.Join(dir, InitStateDir, markerName(name))
		if _, err := os.Stat(marker); err == nil {
			return false, "", nil
		}
		return true, marker, nil

	case strings.HasPrefix(when, xcomposeapp.WhenAbsentPrefix):
		// Maison evaluates this one, so the path resolves container-side —
		// unlike the step's volumes, which the daemon resolves.
		raw := strings.TrimPrefix(when, xcomposeapp.WhenAbsentPrefix)
		path, err := resolvePath(envinject.Render(raw, cfg, project, envFile), cfg)
		if err != nil {
			return false, "", fmt.Errorf("when: %w", err)
		}
		if _, err := os.Stat(path); err == nil {
			return false, "", nil
		}
		return true, "", nil
	}
	return false, "", fmt.Errorf("when: %q is not %q, %q or %q<path>",
		step.When, xcomposeapp.WhenOnce, xcomposeapp.WhenAlways, xcomposeapp.WhenAbsentPrefix)
}

// renderAll renders each element of a list, failing on the first unresolved
// reference — an init step that runs with an empty argument is the shell failure
// mode this replaces.
func renderAll(render func(string) (string, error), in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		v, err := render(s)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// renderBinds renders each volume and maps its SOURCE to a host path.
//
// The daemon resolves a bind source, not this process, so `/DATA/...` has to be
// spelled the way the host spells it — the same rule a hook's `docker run -v`
// lives under, and the opposite of everything Maison writes itself.
func renderBinds(render func(string) (string, error), cfg config.Config, volumes []string) ([]string, error) {
	if len(volumes) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(volumes))
	for _, v := range volumes {
		rendered, err := render(v)
		if err != nil {
			return nil, err
		}
		src, rest, ok := strings.Cut(rendered, ":")
		if !ok {
			return nil, fmt.Errorf("volume %q: want source:target[:mode]", v)
		}
		out = append(out, envinject.RewriteToHostPath(src, cfg)+":"+rest)
	}
	return out, nil
}

func stepPhase(s xcomposeapp.InitStep) string {
	if strings.TrimSpace(s.Phase) == xcomposeapp.PhasePostUp {
		return xcomposeapp.PhasePostUp
	}
	return xcomposeapp.PhasePreUp
}

func stepName(s xcomposeapp.InitStep) string {
	if n := strings.TrimSpace(s.Name); n != "" {
		return n
	}
	return strings.TrimSpace(s.Image)
}

// markerName makes a step name safe to use as a filename, so a step called
// "seed db" or an image reference with a slash in it cannot write outside
// InitStateDir.
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
