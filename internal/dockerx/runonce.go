package dockerx

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// RunSpec describes a one-shot container: an image run to completion for its
// effect on a volume or for what it prints.
//
// Paths in Binds are HOST paths. The daemon resolves them, not this process, so
// a caller mapping a declared /DATA/... path must convert it first (see
// envinject.HostPath / RewriteToHostPath) — the same rule a hook's `docker run
// -v` lives under.
type RunSpec struct {
	Image      string
	Entrypoint []string
	Cmd        []string
	User       string
	Env        []string
	Binds      []string
	Network    string
}

// RunOnce runs a container to completion and returns what it wrote to stdout.
//
// It is the declarative form of the `docker run --rm` that five store apps do
// from a shell hook: seed a database with the app's own binary, or compute a
// value (an rclone-obscured password, a bcrypt hash) for a config file. Those
// are the cases that genuinely need a container rather than a shell, which is
// why this exists while the rest of the setup surface does not shell out at all.
//
// Stdout is returned separately from stderr so a captured value is the value and
// not the image's chatter. A non-zero exit is an error carrying both streams:
// the whole point of moving this out of a hook is that a failure is loud.
//
// The container is always removed, including when the run fails — a half-removed
// one-shot would be picked up as an app container by the discovery in this
// package. AutoRemove is deliberately not used: it races the log read, and the
// logs are the return value.
func (c *Client) RunOnce(ctx context.Context, spec RunSpec) (string, error) {
	if strings.TrimSpace(spec.Image) == "" {
		return "", fmt.Errorf("no image")
	}
	if _, _, err := c.cli.ImageInspectWithRaw(ctx, spec.Image); err != nil {
		// Not present locally. An init image is small and usually the app's own,
		// already pulled by the install, so this is the uncommon path.
		if err := c.PullImage(ctx, spec.Image, nil); err != nil {
			return "", fmt.Errorf("pull %s: %w", spec.Image, err)
		}
	}

	created, err := c.cli.ContainerCreate(ctx,
		&container.Config{
			Image:      spec.Image,
			Cmd:        spec.Cmd,
			Entrypoint: spec.Entrypoint,
			User:       spec.User,
			Env:        spec.Env,
		},
		&container.HostConfig{
			Binds:       spec.Binds,
			NetworkMode: container.NetworkMode(spec.Network),
		}, nil, nil, "")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = c.cli.ContainerRemove(context.WithoutCancel(ctx), created.ID,
			container.RemoveOptions{Force: true, RemoveVolumes: true})
	}()

	if err := c.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", err
	}
	statusCh, errCh := c.cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	var code int64
	select {
	case err := <-errCh:
		if err != nil {
			return "", err
		}
	case st := <-statusCh:
		code = st.StatusCode
	}

	stdout, stderr, logErr := c.containerOutput(ctx, created.ID)
	if code != 0 {
		return stdout, fmt.Errorf("exited %d: %s", code, firstLines(stderr, stdout))
	}
	if logErr != nil {
		return "", logErr
	}
	return stdout, nil
}

// containerOutput reads a finished container's two streams apart. Docker
// multiplexes them into one framed stream unless the container had a TTY, which
// this never gives it — precisely so the two stay separable.
func (c *Client) containerOutput(ctx context.Context, id string) (stdout, stderr string, err error) {
	rc, err := c.cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return "", "", err
	}
	defer rc.Close()

	var out, errBuf bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &errBuf, rc); err != nil {
		return "", "", err
	}
	return out.String(), errBuf.String(), nil
}

// firstLines picks the most useful failure text available: what the container
// complained about, or failing that what it printed.
func firstLines(stderr, stdout string) string {
	text := strings.TrimSpace(stderr)
	if text == "" {
		text = strings.TrimSpace(stdout)
	}
	if text == "" {
		return "no output"
	}
	const max = 500
	if len(text) > max {
		return text[:max] + "…"
	}
	return text
}
