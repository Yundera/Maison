package apps

import (
	"context"
	"sort"

	"github.com/yundera/maison/internal/dockerx"
)

// Untracked is a container no app accounts for: a compose project Maison neither
// installed nor recognises (no x-casaos or x-compose-app), or a container started
// outside Compose altogether. Listed for completeness only — Maison never acts on
// one, since there is no app whose lifecycle it would be acting within.
type Untracked struct {
	ID    string `json:"id"` // short (12-char) id, as `docker ps` prints it
	Name  string `json:"name"`
	Image string `json:"image"`
	// State is Docker's machine state (running, exited, created, …); Status is its
	// human summary of it ("Up 2 hours", "Exited (1) 3 days ago").
	State  string `json:"state"`
	Status string `json:"status"`
	// Project and Service are the compose labels; both empty for a container
	// started with a plain `docker run`.
	Project string `json:"project,omitempty"`
	Service string `json:"service,omitempty"`
}

// Untracked lists every container on the host that belongs to no app in the
// listing. It reads Docker directly rather than through the listing's cache: it
// backs a page someone opened on purpose, not the grid's constant refresh.
func (r *Registry) Untracked(ctx context.Context) ([]Untracked, error) {
	if r.dx == nil {
		return nil, errNoDocker
	}
	all, err := r.dx.ListAllContainers(ctx)
	if err != nil {
		return nil, err
	}
	list, _ := r.List(ctx)
	return untracked(all, list), nil
}

// untracked keeps the containers whose compose project is not an app's id,
// grouped by project (non-compose containers last) and then by name.
func untracked(all []dockerx.Container, list []App) []Untracked {
	tracked := make(map[string]bool, len(list))
	for _, a := range list {
		tracked[a.ID] = true
	}
	out := []Untracked{}
	for _, c := range all {
		if c.Project != "" && tracked[c.Project] {
			continue
		}
		id := c.ID
		if len(id) > 12 {
			id = id[:12]
		}
		out = append(out, Untracked{
			ID:      id,
			Name:    c.Name,
			Image:   c.Image,
			State:   c.State,
			Status:  c.Status,
			Project: c.Project,
			Service: c.Service,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Project == "") != (b.Project == "") {
			return a.Project != ""
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return a.Name < b.Name
	})
	return out
}
