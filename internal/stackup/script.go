package stackup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/yundera/maison/internal/config"
	"github.com/yundera/maison/internal/envinject"
	"github.com/yundera/maison/internal/xcomposeapp"
)

// ScriptsStateDir holds the store app's scripts/ folder inside the app folder,
// refreshed by every install and update from the same store sync as the compose —
// so a compose and the files its script entries name are always from the same
// commit. `file: scripts/x.sh` runs <app>/.scripts/x.sh.
const ScriptsStateDir = ".scripts"

// ScriptOpts is what the caller of a script entry adds to the shared contract.
type ScriptOpts struct {
	// Label names the entry in errors: "step seed-db", "migration 2", …
	Label string
	// DefaultTimeout applies when the entry declares none.
	DefaultTimeout time.Duration
	// Extra are KEY=VALUE pairs on top of the app's environment, winning over
	// its .env: MIGRATION_ID for a migration, the new and old value for a
	// credential change.
	Extra []string
	// Outputs are what earlier scripts of the same operation wrote to
	// $MAISON_OUTPUT, exported to this one.
	Outputs map[string]string
}

// ScriptResult is what a script handed on.
type ScriptResult struct {
	// Output is what it wrote to $MAISON_OUTPUT: values for later scripts and for
	// the seed tree and `files` of the same operation, never persisted.
	Output map[string]string
	// Env is what it wrote to $MAISON_ENV, after the keys the app's .env already
	// had were dropped — i.e. exactly what was added to the .env.
	Env map[string]string
}

// RunScript runs one script entry — a step, a migration step or a credential
// change, which differ only in when they run. See docs/x-compose-app.md (The
// script entry).
//
// It runs `bash -euo pipefail` in Maison's own container against the HOST Docker
// daemon, in the app folder, with the app's environment (see scriptEnv). $APP_DIR
// and $DATA_ROOT are host paths, valid in the shell too: when the deployment
// mounts the data folder elsewhere, Maison aliases the host path inside its
// container at startup (envinject.EnsureHostPathAlias). Nothing in the script
// text is rewritten.
//
// The commands it may call are the curated set in hookBinDir; anything else fails
// the script with a message naming the sanctioned alternative — see hookshell.go
// for why that is an allowlist, and why the verdict comes from a file rather than
// an exit status.
func RunScript(ctx context.Context, cfg config.Config, project, dir string, s xcomposeapp.Script, o ScriptOpts) (ScriptResult, error) {
	var res ScriptResult
	if err := s.Validate(); err != nil {
		return res, err
	}
	timeout := s.TimeoutOr(o.DefaultTimeout)
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	work, err := os.MkdirTemp("", "maison-script-")
	if err != nil {
		return res, fmt.Errorf("script workspace: %w", err)
	}
	defer os.RemoveAll(work)
	rejected := filepath.Join(work, "rejected")
	preamble := filepath.Join(work, "preamble.sh")
	output := filepath.Join(work, "output")
	envOut := filepath.Join(work, "env")
	for path, body := range map[string]string{rejected: "", preamble: hookPreamble(), output: "", envOut: ""} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return res, fmt.Errorf("script workspace: %w", err)
		}
	}

	args := []string{"-euo", "pipefail"}
	if s.Lenient {
		args = nil
	}
	if strings.TrimSpace(s.File) != "" {
		path := scriptPath(dir, s.File)
		if _, err := os.Stat(path); err != nil {
			return res, fmt.Errorf("file %s: %w", s.File, err)
		}
		args = append(args, path)
	} else {
		args = append(args, "-c", s.Script)
	}

	cmd := exec.CommandContext(ctx, "/bin/bash", args...)
	cmd.Dir = dir
	env := scriptEnv(cfg, project, dir)
	for k, v := range o.Outputs {
		env = append(env, k+"="+v)
	}
	cmd.Env = append(append(env, o.Extra...),
		"BASH_ENV="+preamble,
		hookRejectedVar+"="+rejected,
		"MAISON_OUTPUT="+output,
		"MAISON_ENV="+envOut,
	)
	out, runErr := cmd.CombinedOutput()

	// Checked before runErr, and regardless of it: the failures this catches are
	// the ones that do not show up as a non-zero exit. A missing command inside
	// `"$(...)"` leaves the substitution empty and, outside `set -e`'s reach, the
	// script exits 0 — which is how an app ships an empty secret and installs green.
	if names := rejectedCommands(rejected); len(names) > 0 {
		return res, fmt.Errorf("script called %s, which app scripts may not use — run it in a pinned container instead (see %s): %s",
			strings.Join(names, ", "), hookDocRef, out)
	}
	if runErr != nil {
		return res, timedOut(ctx, fmt.Errorf("%w: %s", runErr, out), timeout)
	}

	outB, _ := os.ReadFile(output)
	res.Output = envinject.EnvFileVars(outB)
	envB, _ := os.ReadFile(envOut)
	if res.Env, err = addMissingEnv(dir, envinject.EnvFileVars(envB)); err != nil {
		return res, fmt.Errorf("write $MAISON_ENV to .env: %w", err)
	}
	return res, nil
}

// scriptPath is where `file: scripts/x.sh` lives in the app folder.
func scriptPath(dir, file string) string {
	rel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(file)), xcomposeapp.ScriptsDir+"/")
	return filepath.Join(dir, ScriptsStateDir, filepath.FromSlash(rel))
}

// addMissingEnv writes to the app's .env each key it does not hold yet, and
// returns what it wrote. A key already there — even empty — is the app's or the
// operator's and is never overwritten: that is what lets a script generate a
// keypair once, or put an empty setting in front of the user, and run again.
func addMissingEnv(dir string, vars map[string]string) (map[string]string, error) {
	if len(vars) == 0 {
		return nil, nil
	}
	have := envinject.EnvFileVars(readEnvFile(dir))
	add := map[string]string{}
	for k, v := range vars {
		if _, ok := have[k]; !ok {
			add[k] = v
		}
	}
	if len(add) == 0 {
		return nil, nil
	}
	if err := envinject.ValidateVars(varsOf(add)); err != nil {
		return nil, err
	}
	return add, envinject.EnsureVars(filepath.Join(dir, ".env"), add)
}

func varsOf(m map[string]string) []envinject.Var {
	out := make([]envinject.Var, 0, len(m))
	for k, v := range m {
		out = append(out, envinject.Var{Key: k, Value: v})
	}
	return out
}

// scriptEnv is the app's interpolation environment (base vars overlaid with its
// persisted .env, so a script sees the same values its compose does) plus what a
// script needs to reach the host daemon, its own app folder and its own stack:
// COMPOSE_PROJECT_NAME and COMPOSE_FILE make `compose …` (and a bare `docker
// compose …`) act on the app's project from anywhere.
func scriptEnv(cfg config.Config, project, dir string) []string {
	env := envinject.Env(cfg, project)
	for k, v := range envinject.EnvFileVars(readEnvFile(dir)) {
		env = append(env, k+"="+v)
	}
	return append(env,
		"PATH="+hookPath(),
		"DOCKER_HOST=unix:///var/run/docker.sock",
		"AppID="+project,
		"APP_DIR="+envinject.HostPath(dir, cfg),
		"COMPOSE_PROJECT_NAME="+project,
		"COMPOSE_FILE="+strings.Join(composeFiles(filepath.Join(dir, "docker-compose.yml"), filepath.Join(dir, "docker-compose.override.yml")), string(os.PathListSeparator)),
	)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// timedOut names the entry's own deadline when that is what ended it, rather than
// leaving a bare "signal: killed" or "context deadline exceeded".
func timedOut(ctx context.Context, err error, timeout time.Duration) error {
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("did not finish within its %s timeout: %w", timeout, err)
	}
	return err
}
