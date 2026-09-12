// Package composecmd invokes the `docker compose` plugin out-of-process. Using
// the plugin (rather than linking the compose engine) keeps the binary small and
// the behaviour identical to Docker's own tooling.
package composecmd

import (
	"context"
	"fmt"
	"os/exec"
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
