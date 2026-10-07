// Package cleanup decides what Settings › Resources › Storage may remove from
// Docker, and what it must not.
//
// It is deliberately NOT `docker system prune --all`. That command deletes every
// stopped container and then every image no container uses — and on a PCS a
// stopped container is an app the operator switched off, not rubbish. Maison's Stop
// keeps an app's containers (dockerx.StopProject), so a prune would delete the app,
// then its image; starting it again means a full re-pull, which fails outright if
// the pinned tag has since left the registry. An unmanaged stack — one Maison knows
// only through its containers — would vanish from the dashboard entirely.
//
// So the plan splits in two, by how sure it can be:
//
//   - The routine clean, one button, no choices: images nothing uses and no
//     installed app names, the build cache, and networks left behind by compose
//     projects that no container is attached to. None of that is anyone's state.
//     It never removes a container and never touches a volume.
//
//   - Orphans, listed for the operator to tick: containers of a compose project
//     whose folder is gone (an uninstall that failed half-way, a stack deleted by
//     hand), and stopped containers that no compose project owns. These are
//     *probably* rubbish, and removing one is not reversible, so a person decides.
//
// Everything here is a pure function of a snapshot. The server rebuilds the plan
// at the moment it acts rather than trusting the one it showed, and the daemon's
// own refusals (an image a container now uses, a network something joined) are the
// last guard against whatever happened in between.
package cleanup

import (
	"regexp"
	"sort"
	"strings"

	"github.com/distribution/reference"

	"github.com/yundera/maison/internal/dockerx"
)

// Reasons an orphan is offered.
const (
	// ReasonFolderGone: a compose project whose working directory no longer exists.
	// Nothing can bring it up, update it or uninstall it any more.
	ReasonFolderGone = "folder_gone"
	// ReasonStandalone: a stopped container that no compose project owns — a
	// leftover `docker run`.
	ReasonStandalone = "standalone"
)

// Input is the box's state, as the planner needs it.
type Input struct {
	Images     []dockerx.ImageInfo
	Containers []dockerx.ContainerInfo
	Networks   []dockerx.NetworkInfo

	// Referenced is every image reference an installed app's compose names, with
	// ${VAR} already resolved where the app's .env could resolve it. An image named
	// here is kept even when no container uses it: that is a managed app whose
	// containers were removed, and `compose up` would otherwise have to re-pull.
	Referenced []string

	// KeepProjects are compose projects whose app declares it cannot be uninstalled
	// (x-compose-app `lifecycle.uninstallable: false`). Never orphans, whatever their
	// folder says: removing their containers would be an uninstall by another name.
	KeepProjects map[string]bool

	// KeepNetworks are network names never removed, used or not — the app network
	// every store app joins by name.
	KeepNetworks map[string]bool

	// FolderGone reports whether a compose working directory, as the HOST spells
	// it, is known to be gone. It must answer false when it cannot tell — a folder
	// outside the data root is invisible from in here, and "cannot see it" is not
	// "it is gone".
	FolderGone func(hostDir string) bool

	// Self is this process's own container id, or a prefix of it. Its project is
	// never an orphan: removing it would take the dashboard down mid-request.
	Self string
}

// Image is an image the routine clean removes.
type Image struct {
	ID   string   `json:"id"`
	Tags []string `json:"tags"`
	// Bytes is what removing it alone gives back: its size less the layers other
	// images share. An estimate — two candidates sharing a layer free that layer
	// only together — so the result reports the filesystem's real change instead.
	Bytes int64 `json:"bytes"`
}

// Network is a network the routine clean removes.
type Network struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// OrphanContainer is one container of an orphan.
type OrphanContainer struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Service string `json:"service,omitempty"`
	State   string `json:"state"`
	Image   string `json:"image"`
}

// Orphan is a group the operator may remove: a whole compose project, or one
// standalone container.
type Orphan struct {
	// Key identifies the group in a removal request: the project name, or the
	// container id for a standalone one.
	Key        string            `json:"key"`
	Project    string            `json:"project,omitempty"`
	Reason     string            `json:"reason"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Running    bool              `json:"running"`
	Containers []OrphanContainer `json:"containers"`
}

// Plan is what the Storage tab shows before anything is removed.
type Plan struct {
	Images      []Image   `json:"images"`
	ImagesBytes int64     `json:"images_bytes"`
	Networks    []Network `json:"networks"`
	Orphans     []Orphan  `json:"orphans"`
	// ImagesKept counts images the clean leaves because something uses or names them.
	ImagesKept int `json:"images_kept"`
}

// builtinNetworks are the daemon's own; it refuses to remove them anyway.
var builtinNetworks = map[string]bool{"bridge": true, "host": true, "none": true}

// Build computes the plan for one snapshot.
func Build(in Input) Plan {
	p := Plan{Images: []Image{}, Networks: []Network{}, Orphans: []Orphan{}}

	// ── images ────────────────────────────────────────────────────────────────
	used := map[string]bool{}
	for _, c := range in.Containers {
		if c.ImageID != "" {
			used[c.ImageID] = true
		}
	}
	names := newRefSet(in.Referenced)
	for _, im := range in.Images {
		if used[im.ID] || im.Containers > 0 || names.names(im) {
			p.ImagesKept++
			continue
		}
		bytes := im.Size
		if im.SharedSize > 0 && im.SharedSize <= im.Size {
			bytes -= im.SharedSize
		}
		p.Images = append(p.Images, Image{ID: im.ID, Tags: im.Tags, Bytes: bytes})
		p.ImagesBytes += bytes
	}
	sort.Slice(p.Images, func(i, j int) bool { return p.Images[i].Bytes > p.Images[j].Bytes })

	// ── networks ──────────────────────────────────────────────────────────────
	//
	// Only compose's own leftovers. A network created by hand carries no compose
	// label, and may be waiting for something the operator is about to start; a
	// compose network with nothing attached is recreated by the next `up` of its
	// project, so removing it costs nothing even when the project comes back.
	//
	// "Attached" includes STOPPED containers. `docker network prune` does not count
	// them, and a stopped container whose network was pruned fails to start — which
	// is exactly how an unmanaged stack, started by ContainerStart rather than
	// compose, would break.
	attached := map[string]bool{}
	for _, c := range in.Containers {
		for _, n := range c.Networks {
			attached[n] = true
		}
	}
	for _, n := range in.Networks {
		if builtinNetworks[n.Name] || in.KeepNetworks[n.Name] || attached[n.ID] {
			continue
		}
		if n.Scope != "" && n.Scope != "local" {
			continue // swarm-scoped: not this box's alone to remove
		}
		if n.Labels["com.docker.compose.network"] == "" {
			continue
		}
		p.Networks = append(p.Networks, Network{ID: n.ID, Name: n.Name})
	}
	sort.Slice(p.Networks, func(i, j int) bool { return p.Networks[i].Name < p.Networks[j].Name })

	// ── orphans ───────────────────────────────────────────────────────────────
	selfProjects := map[string]bool{}
	for _, c := range in.Containers {
		if in.Self != "" && c.Project != "" && strings.HasPrefix(c.ID, in.Self) {
			selfProjects[c.Project] = true
		}
	}
	projects := map[string]*Orphan{}
	for _, c := range in.Containers {
		oc := OrphanContainer{ID: c.ID, Name: c.Name, Service: c.Service, State: c.State, Image: c.Image}
		if c.Project == "" {
			// A standalone container: offered only once it has stopped. A running one
			// is doing something for someone, and Maison did not start it.
			if in.Self != "" && strings.HasPrefix(c.ID, in.Self) {
				continue
			}
			if c.State != "exited" && c.State != "dead" {
				continue
			}
			p.Orphans = append(p.Orphans, Orphan{
				Key: c.ID, Reason: ReasonStandalone, Containers: []OrphanContainer{oc},
			})
			continue
		}
		if in.KeepProjects[c.Project] || selfProjects[c.Project] {
			continue
		}
		if c.WorkingDir == "" || in.FolderGone == nil || !in.FolderGone(c.WorkingDir) {
			continue
		}
		o := projects[c.Project]
		if o == nil {
			o = &Orphan{Key: c.Project, Project: c.Project, Reason: ReasonFolderGone, WorkingDir: c.WorkingDir}
			projects[c.Project] = o
		}
		o.Containers = append(o.Containers, oc)
		if c.State == "running" || c.State == "restarting" {
			o.Running = true
		}
	}
	for _, o := range projects {
		sort.Slice(o.Containers, func(i, j int) bool { return o.Containers[i].Name < o.Containers[j].Name })
		p.Orphans = append(p.Orphans, *o)
	}
	sort.Slice(p.Orphans, func(i, j int) bool {
		a, b := p.Orphans[i], p.Orphans[j]
		if a.Reason != b.Reason {
			return a.Reason == ReasonFolderGone // projects first: they are the bigger catch
		}
		return orphanName(a) < orphanName(b)
	})
	return p
}

func orphanName(o Orphan) string {
	if o.Project != "" {
		return o.Project
	}
	if len(o.Containers) > 0 {
		return o.Containers[0].Name
	}
	return o.Key
}

// ── image references ─────────────────────────────────────────────────────────

// refSet answers "does any installed app name this image".
type refSet struct {
	exact    map[string]bool // normalized name:tag and name@digest
	prefixes []string        // what precedes an unresolved ${VAR}
}

func newRefSet(refs []string) refSet {
	s := refSet{exact: map[string]bool{}}
	for _, r := range refs {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if i := strings.IndexByte(r, '$'); i >= 0 {
			// A variable the app's .env did not resolve. Keep every image that
			// starts the same way — "ghcr.io/x/app:${TAG}" keeps all of x/app. Too
			// much kept costs space; too little costs a re-pull of an app's image.
			if pre := strings.TrimSpace(r[:i]); pre != "" {
				s.prefixes = append(s.prefixes, pre)
				if n, err := reference.ParseNormalizedNamed(strings.TrimRight(pre, ":@")); err == nil {
					s.prefixes = append(s.prefixes, n.Name())
				}
			}
			continue
		}
		for _, k := range refKeys(r) {
			s.exact[k] = true
		}
	}
	return s
}

// refKeys is every spelling a reference matches: its normalized name:tag (":latest"
// when it has none) and, when it pins a digest, its name@digest.
func refKeys(r string) []string {
	n, err := reference.ParseNormalizedNamed(r)
	if err != nil {
		return []string{r}
	}
	var keys []string
	if d, ok := n.(reference.Digested); ok {
		keys = append(keys, n.Name()+"@"+d.Digest().String())
		if _, tagged := n.(reference.Tagged); !tagged {
			return keys
		}
	}
	return append(keys, reference.TagNameOnly(n).String())
}

func (s refSet) names(im dockerx.ImageInfo) bool {
	for _, t := range append(append([]string{}, im.Tags...), im.Digests...) {
		for _, k := range refKeys(t) {
			if s.exact[k] {
				return true
			}
		}
		for _, pre := range s.prefixes {
			if strings.HasPrefix(t, pre) {
				return true
			}
			if n, err := reference.ParseNormalizedNamed(t); err == nil && strings.HasPrefix(n.String(), pre) {
				return true
			}
		}
	}
	return false
}

// varPattern matches ${NAME}, ${NAME:-default}, ${NAME-default} and $NAME.
var varPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?:(:?-)([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// ExpandRef resolves the variables in a compose image reference the way compose
// would, from env. A variable env does not set and the reference gives no default
// for is left in place — newRefSet then falls back to a prefix match on it.
func ExpandRef(ref string, env map[string]string) string {
	return varPattern.ReplaceAllStringFunc(ref, func(m string) string {
		g := varPattern.FindStringSubmatch(m)
		name, op, def := g[1], g[2], g[3]
		if name == "" {
			name = g[4]
		}
		v, ok := env[name]
		switch {
		case op == ":-" && v == "":
			return def
		case op == "-" && !ok:
			return def
		case ok:
			return v
		}
		return m
	})
}
