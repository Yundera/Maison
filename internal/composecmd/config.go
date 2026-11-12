package composecmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Config resolves a project the way `docker compose up` would — merging the
// files, interpolating the variables, applying the override tags — and returns
// the result. It is what the settings window shows as the "effective" compose:
// exactly what the two files add up to, which is otherwise invisible.
//
// With interpolate false, every ${VAR} is left as written (`--no-interpolate`):
// the merge alone, which is what the settings window shows by default — the files
// are written with variables, so their merge reads best with them too.
func Config(ctx context.Context, dir, project string, files, env []string, interpolate bool) (string, error) {
	extra := []string{}
	if !interpolate {
		extra = append(extra, "--no-interpolate")
	}
	out, err := config(ctx, dir, project, files, env, extra...)
	if err != nil {
		return "", err
	}
	return out, nil
}

// Validate parses a project without resolving it fully, returning the error
// Compose itself reports. It is the guard in front of every override save: a
// typo'd YAML or an unknown key is caught here rather than by watching the stack
// fail to come back up.
func Validate(ctx context.Context, dir, project string, files, env []string) error {
	_, err := config(ctx, dir, project, files, env, "--quiet")
	return err
}

func config(ctx context.Context, dir, project string, files, env []string, flags ...string) (string, error) {
	args := []string{"compose", "-p", project}
	for _, f := range files {
		args = append(args, "-f", f)
	}
	args = append(args, "config")
	args = append(args, flags...)

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = dir
	cmd.Env = env

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s", composeError(err))
	}
	return string(out), nil
}

// composeError surfaces what compose printed on stderr — "services.app.ports.0:
// invalid port" is the whole point of validating, and it is far more useful than
// "exit status 15".
func composeError(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
			return msg
		}
	}
	return err.Error()
}

// Images lists the images `docker compose up` would need from a registry for a
// project: every service's resolved image, base + override merged and variables
// interpolated, exactly as compose itself resolves them.
//
// Services compose would build rather than pull (a `build:` section, or
// pull_policy build) are left out, and so are pull_policy never — compose is
// forbidden to pull those, so a missing one is not something a pull can fix.
func Images(ctx context.Context, dir, project string, files, env []string) ([]string, error) {
	out, err := config(ctx, dir, project, files, env, "--format", "json")
	if err != nil {
		return nil, err
	}
	return imagesFromConfig([]byte(out))
}

func imagesFromConfig(raw []byte) ([]string, error) {
	var doc struct {
		Services map[string]struct {
			Image      string          `json:"image"`
			Build      json.RawMessage `json:"build"`
			PullPolicy string          `json:"pull_policy"`
		} `json:"services"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("compose config: %w", err)
	}
	seen := map[string]bool{}
	var images []string
	for _, svc := range doc.Services {
		if svc.Image == "" || len(svc.Build) > 0 && string(svc.Build) != "null" {
			continue
		}
		switch svc.PullPolicy {
		case "build", "never":
			continue
		}
		if !seen[svc.Image] {
			seen[svc.Image] = true
			images = append(images, svc.Image)
		}
	}
	sort.Strings(images)
	return images, nil
}
