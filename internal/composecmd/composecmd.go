// Package composecmd invokes the `docker compose` plugin out-of-process. Using
// the plugin (rather than linking the compose engine) keeps the binary small and
// the behaviour identical to Docker's own tooling.
package composecmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Up brings a project up in detached mode from the given compose files.
func Up(ctx context.Context, dir, project string, files, env []string) error {
	cmd := exec.CommandContext(ctx, "docker", upArgs(project, files)...)
	cmd.Dir = dir
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("compose up: %w: %s", err, out)
	}
	return nil
}

// Down removes a project the way it was brought up: `docker compose -p <project>
// down`, which removes the project's containers and the networks compose created
// for it. It is the uninstall, the mirror of Up.
//
// It takes no compose file on purpose. By the time an uninstall removes the
// containers the app folder may already be gone (a backup engine can take it by
// renaming), and compose finds a project from its labels alone. Without -f,
// compose would look for a compose file in the working directory and its parents
// and act on whatever it found, so it runs from "/" with COMPOSE_FILE stripped:
// only the project label decides what is removed.
//
// Volumes are not removed (no -v): named volumes are left alone, as they always
// were for an uninstall.
func Down(ctx context.Context, project string) error {
	cmd := exec.CommandContext(ctx, "docker", downArgs(project)...)
	cmd.Dir = "/"
	cmd.Env = withoutComposeFile(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("compose down: %w: %s", err, out)
	}
	return nil
}

func downArgs(project string) []string {
	return []string{"compose", "-p", project, "down"}
}

func withoutComposeFile(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "COMPOSE_FILE=") {
			out = append(out, kv)
		}
	}
	return out
}

// upArgs always passes --remove-orphans. A store update that drops or renames a
// service otherwise leaves the old service's container behind under the project
// label, still holding its container_name. When the new file hands that name to
// another service, compose creates the replacement under a temporary
// `<id>_<name>` and the final rename fails. Behind AppShield that is a broken
// app, not a cosmetic one: the auth-registrar attests the client_id from the
// container name, so the shield registers as `<id>_<name>` and Dex rejects its
// redirect_uri.
//
// It is safe because nothing else Maison starts carries the compose project
// label: init steps (dockerx.RunOnce) and backup engines are unlabeled, hooks
// are plain shell, and an extension is its own project, not a service of its
// parent.
func upArgs(project string, files []string) []string {
	args := []string{"compose", "-p", project}
	for _, f := range files {
		args = append(args, "-f", f)
	}
	return append(args, "up", "-d", "--remove-orphans")
}
