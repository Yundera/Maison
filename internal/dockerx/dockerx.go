// Package dockerx wraps the Docker Engine API for the operations Maison needs:
// discovering compose projects, per-project lifecycle, and an event stream for
// live status. The heavier compose install engine is layered on top separately.
package dockerx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// Compose label keys set by `docker compose`.
const (
	labelProject    = "com.docker.compose.project"
	labelService    = "com.docker.compose.service"
	labelWorkingDir = "com.docker.compose.project.working_dir"
	labelConfigFile = "com.docker.compose.project.config_files"
)

// Client is a thin wrapper over the Docker API client.
type Client struct {
	cli *client.Client
}

// New connects to the Docker engine (honouring DOCKER_HOST, default socket).
func New() (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Client{cli: cli}, nil
}

// Ping checks connectivity to the daemon.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.cli.Ping(ctx)
	return err
}

// Port is a published container port mapping.
type Port struct {
	Private uint16
	Public  uint16
}

// Container is a compose-managed container, flattened for our needs.
type Container struct {
	ID          string
	Project     string
	Service     string
	WorkingDir  string
	ConfigFiles string
	State       string // running, exited, created, ...
	Health      string // healthy | unhealthy | starting | "" (no health check)
	Ports       []Port
}

// parseHealth extracts a container health check verdict from the ContainerList
// summary Status string (e.g. "Up 2 minutes (healthy)"). Docker doesn't surface
// health as its own field on the list summary, so we read the parenthetical.
func parseHealth(status string) string {
	switch {
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	default:
		return ""
	}
}

// ListProjectContainers returns all containers that belong to a compose project.
func (c *Client) ListProjectContainers(ctx context.Context) ([]Container, error) {
	list, err := c.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(list))
	for _, ct := range list {
		project := ct.Labels[labelProject]
		if project == "" {
			continue
		}
		var ports []Port
		for _, p := range ct.Ports {
			if p.PublicPort > 0 {
				ports = append(ports, Port{Private: p.PrivatePort, Public: p.PublicPort})
			}
		}
		out = append(out, Container{
			ID:          ct.ID,
			Project:     project,
			Service:     ct.Labels[labelService],
			WorkingDir:  ct.Labels[labelWorkingDir],
			ConfigFiles: ct.Labels[labelConfigFile],
			State:       ct.State,
			Health:      parseHealth(ct.Status),
			Ports:       ports,
		})
	}
	return out, nil
}

func (c *Client) projectContainerIDs(ctx context.Context, project string) ([]string, error) {
	f := filters.NewArgs(filters.Arg("label", labelProject+"="+project))
	list, err := c.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list))
	for _, ct := range list {
		ids = append(ids, ct.ID)
	}
	if len(ids) == 0 {
		return nil, errors.New("no containers for project " + project)
	}
	return ids, nil
}

// StartProject starts every container in a project.
func (c *Client) StartProject(ctx context.Context, project string) error {
	ids, err := c.projectContainerIDs(ctx, project)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := c.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// StopProject stops every container in a project.
func (c *Client) StopProject(ctx context.Context, project string) error {
	ids, err := c.projectContainerIDs(ctx, project)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := c.cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// RestartProject restarts every container in a project.
func (c *Client) RestartProject(ctx context.Context, project string) error {
	ids, err := c.projectContainerIDs(ctx, project)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := c.cli.ContainerRestart(ctx, id, container.StopOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// RemoveProject stops and removes every container in a project (best-effort),
// deleting anonymous volumes. Named app data under DATA_ROOT is left intact.
//
// onProgress, when non-nil, is called after each container with how many of the
// project's containers are done — a stop can block for the container's whole
// stop-grace period, so the uninstall bar needs per-container granularity to
// keep moving.
func (c *Client) RemoveProject(ctx context.Context, project string, onProgress func(done, total int)) error {
	ids, err := c.projectContainerIDs(ctx, project)
	if err != nil {
		return err
	}
	if onProgress == nil {
		onProgress = func(int, int) {}
	}
	onProgress(0, len(ids))
	for i, id := range ids {
		_ = c.cli.ContainerStop(ctx, id, container.StopOptions{})
		if err := c.cli.ContainerRemove(ctx, id, container.RemoveOptions{
			RemoveVolumes: true,
			Force:         true,
		}); err != nil {
			return err
		}
		onProgress(i+1, len(ids))
	}
	return nil
}

// PullImage pulls an image, invoking onProgress with an aggregate download
// percentage (0-100 across all layers) and the latest status string.
func (c *Client) PullImage(ctx context.Context, ref string, onProgress func(pct float64, status string)) error {
	rc, err := c.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return err
	}
	defer rc.Close()

	type detail struct {
		Current int64 `json:"current"`
		Total   int64 `json:"total"`
	}
	type msg struct {
		Status         string `json:"status"`
		ID             string `json:"id"`
		ProgressDetail detail `json:"progressDetail"`
		Error          string `json:"error"`
	}
	layers := map[string]detail{}
	dec := json.NewDecoder(rc)
	for {
		var m msg
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
		if m.ID != "" && m.ProgressDetail.Total > 0 {
			layers[m.ID] = m.ProgressDetail
		}
		if onProgress != nil {
			var cur, tot int64
			for _, d := range layers {
				cur += d.Current
				tot += d.Total
			}
			pct := 0.0
			if tot > 0 {
				pct = float64(cur) / float64(tot) * 100
			}
			onProgress(pct, m.Status)
		}
	}
}

// FirstContainerID returns one container id for a project (the log/stats target).
func (c *Client) FirstContainerID(ctx context.Context, project string) (string, error) {
	ids, err := c.projectContainerIDs(ctx, project)
	if err != nil {
		return "", err
	}
	return ids[0], nil
}

// Service is one compose service's container, with its live state and health.
type Service struct {
	Service string `json:"service"`
	ID      string `json:"container_id"`
	State   string `json:"state"`  // running, exited, created, ...
	Health  string `json:"health"` // "", starting, healthy, unhealthy
}

// ProjectServices lists every container in a compose project as a service,
// including its health-check status — so the UI can reflect a multi-service
// stack rather than a single container.
func (c *Client) ProjectServices(ctx context.Context, project string) ([]Service, error) {
	f := filters.NewArgs(filters.Arg("label", labelProject+"="+project))
	list, err := c.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, errors.New("no containers for project " + project)
	}
	out := make([]Service, 0, len(list))
	for _, ct := range list {
		out = append(out, Service{
			Service: ct.Labels[labelService],
			ID:      ct.ID,
			State:   ct.State,
			Health:  parseHealth(ct.Status),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out, nil
}

// ContainerHealth returns one container's health-check status ("" when it has
// none). It reads the ContainerList summary rather than a full inspect so the
// live stats loop can poll it cheaply.
func (c *Client) ContainerHealth(ctx context.Context, id string) string {
	f := filters.NewArgs(filters.Arg("id", id))
	list, err := c.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil || len(list) == 0 {
		return ""
	}
	return parseHealth(list[0].Status)
}

// ContainerIDForService resolves the container id for a specific service within
// a project (the per-service log/stats target).
func (c *Client) ContainerIDForService(ctx context.Context, project, service string) (string, error) {
	f := filters.NewArgs(
		filters.Arg("label", labelProject+"="+project),
		filters.Arg("label", labelService+"="+service),
	)
	list, err := c.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "", errors.New("no container for service " + service)
	}
	return list[0].ID, nil
}

// ContainerLogs returns the (multiplexed) log stream for a container.
func (c *Client) ContainerLogs(ctx context.Context, id, tail string, follow bool) (io.ReadCloser, error) {
	return c.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Tail:       tail,
	})
}

// ContainerStatsStream returns a streaming stats reader for a container.
func (c *Client) ContainerStatsStream(ctx context.Context, id string) (io.ReadCloser, error) {
	resp, err := c.cli.ContainerStats(ctx, id, true)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// WatchContainers calls onEvent (debounced by the caller) whenever a container
// lifecycle event occurs, until ctx is cancelled.
func (c *Client) WatchContainers(ctx context.Context, onEvent func()) {
	f := filters.NewArgs(filters.Arg("type", string(events.ContainerEventType)))
	msgs, errs := c.cli.Events(ctx, events.ListOptions{Filters: f})
	for {
		select {
		case <-ctx.Done():
			return
		case <-msgs:
			onEvent()
		case err := <-errs:
			if err != nil {
				// Stream broke (daemon restart etc). Give up; caller may restart.
				return
			}
		}
	}
}
