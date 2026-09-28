package installer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yundera/maison/internal/appstore"
	"github.com/yundera/maison/internal/composefile"
)

// The box-wide half of the Update tab: every app's update status in one pass, for
// the Settings → Updates page. CheckUpdate answers one app and syncs its store to
// do it; asking it thirty times would sync one store thirty times. CheckAll syncs
// each store once and reads every app of it from that one copy.

// Update states, one per app, in the order the Updates page groups them.
const (
	// StateAvailable: the store's compose differs from the installed one.
	StateAvailable = "available"
	// StateCurrent: byte-identical to the store's.
	StateCurrent = "current"
	// StateError: the app has a reference, but the check could not be completed —
	// the store is unreachable, or no longer ships the app. Never rendered as
	// "up to date", for the reason AppComposeFrom gives.
	StateError = "error"
	// StateUntracked: Maison manages the app, but it records no store to update from
	// (installed by CasaOS, before store references existed, or dropped in by hand).
	StateUntracked = "untracked"
	// StateUnmanaged: a stack Maison merely discovered. It has no strict base on
	// disk, so there is nothing a store update could replace.
	StateUnmanaged = "unmanaged"
)

// CheckTarget is the part of an app tile CheckAll needs. The installer holds no
// app registry, so the caller hands the list in.
type CheckTarget struct {
	ID        string
	Name      string
	Icon      string
	Managed   bool
	Protected bool
}

// ImageChange is one service whose image an update changes — the closest thing a
// byte-compared compose has to a version number. From is empty for a service the
// update adds, To for one it removes.
type ImageChange struct {
	Service string `json:"service"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
}

// Suggestion is a store app an untracked app looks like. It is only ever offered:
// linking an app to the wrong store app replaces it in place on the next update, so
// the operator confirms it (through SetUpdateRef, like any other retarget).
type Suggestion struct {
	Ref       string `json:"ref"`
	Name      string `json:"name"`
	StoreName string `json:"store_name,omitempty"`
	// ImagesMatch is true when the store app's main service runs the same image
	// repository as the installed app's. A name match without it is a weaker guess,
	// and the UI says so.
	ImagesMatch bool `json:"images_match"`
}

// AppUpdate is one row of the Updates page.
type AppUpdate struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Icon      string `json:"icon,omitempty"`
	Protected bool   `json:"protected,omitempty"`
	State     string `json:"state"`
	// Ref and StoreName say where a tracked app updates from.
	Ref       string        `json:"ref,omitempty"`
	StoreName string        `json:"store_name,omitempty"`
	Images    []ImageChange `json:"images,omitempty"`
	// Suggestion is set only for an untracked app with one unambiguous match.
	Suggestion *Suggestion `json:"suggestion,omitempty"`
	Error      string      `json:"error,omitempty"`
}

// CheckAll reports every target's update state. It never fails as a whole: a store
// that cannot be reached marks its own apps StateError and leaves the others alone.
func (in *Installer) CheckAll(ctx context.Context, targets []CheckTarget) []AppUpdate {
	out := make([]AppUpdate, len(targets))
	refs := make([]appstore.Ref, len(targets))
	current := make([][]byte, len(targets))
	untracked := false

	for i, t := range targets {
		out[i] = AppUpdate{ID: t.ID, Name: t.Name, Icon: t.Icon, Protected: t.Protected}
		if !t.Managed {
			out[i].State = StateUnmanaged
			continue
		}
		raw, err := os.ReadFile(filepath.Join(in.cfg.AppsDir(), t.ID, "docker-compose.yml"))
		if err != nil {
			// Listed as managed but its strict base is gone: nothing to compare against.
			out[i].State = StateUnmanaged
			continue
		}
		current[i] = raw
		refs[i] = in.readUpdateRef(t.ID)
		if refs[i].ID == "" {
			out[i].State = StateUntracked
			untracked = true
		}
	}

	// One sync per store, however many apps follow it.
	synced := map[string]error{}
	for i, ref := range refs {
		if out[i].State != "" || ref.Merged() {
			continue
		}
		key := storeKey(ref)
		if _, done := synced[key]; !done {
			synced[key] = in.store.Sync(ctx, ref)
		}
	}

	for i, ref := range refs {
		if out[i].State != "" {
			continue
		}
		out[i].Ref = ref.Path()
		if err := synced[storeKey(ref)]; err != nil && !ref.Merged() {
			out[i].State, out[i].Error = StateError, err.Error()
			continue
		}
		app, newBase, err := in.store.GetFrom(ctx, ref)
		if err != nil {
			out[i].State, out[i].Error = StateError, err.Error()
			continue
		}
		out[i].StoreName = app.StoreName
		if bytes.Equal(current[i], newBase) {
			out[i].State = StateCurrent
			continue
		}
		out[i].State = StateAvailable
		out[i].Images = imageChanges(current[i], newBase)
	}

	if untracked {
		idx := in.catalogIndex()
		for i := range out {
			if out[i].State == StateUntracked {
				out[i].Suggestion = idx.suggest(targets[i].ID, current[i])
			}
		}
	}
	return out
}

func storeKey(ref appstore.Ref) string { return ref.URL + "\x00" + ref.Apps() }

// imageChanges lists the services whose image differs between two compose files,
// sorted by service. Empty when the files differ in something other than images,
// which the UI shows as "definition changed".
func imageChanges(from, to []byte) []ImageChange {
	a, b := imagesOf(from), imagesOf(to)
	names := map[string]bool{}
	for n := range a {
		names[n] = true
	}
	for n := range b {
		names[n] = true
	}
	var out []ImageChange
	for n := range names {
		if a[n] != b[n] {
			out = append(out, ImageChange{Service: n, From: a[n], To: b[n]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

func imagesOf(raw []byte) map[string]string {
	f, err := composefile.Parse(raw)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(f.Services))
	for name, s := range f.Services {
		out[name] = s.Image
	}
	return out
}

// imageRepo reduces an image reference to its repository, so that two versions of
// one image compare equal: tag and digest dropped, Docker Hub's implied registry and
// library/ namespace spelled out or not.
func imageRepo(image string) string {
	s := strings.ToLower(strings.TrimSpace(image))
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, ":"); i > strings.LastIndex(s, "/") {
		s = s[:i]
	}
	s = strings.TrimPrefix(s, "docker.io/")
	s = strings.TrimPrefix(s, "index.docker.io/")
	s = strings.TrimPrefix(s, "library/")
	return s
}

// mainRepo is the image repository of the app's main service — the one x-casaos
// `main` names, else the only service there is. Matching on every service instead
// would pair unrelated apps through a shared sidecar (a proxy, a database), which is
// exactly the image most store apps have in common.
func mainRepo(raw []byte) string {
	f, err := composefile.Parse(raw)
	if err != nil {
		return ""
	}
	name, _ := f.XCasaOS["main"].(string)
	svc, ok := f.Services[name]
	if !ok {
		if len(f.Services) != 1 {
			return ""
		}
		for _, only := range f.Services {
			svc = only
		}
	}
	r := imageRepo(svc.Image)
	// An image spelled through a variable says nothing comparable.
	if strings.Contains(r, "$") {
		return ""
	}
	return r
}

// catalogEntry is one store app, reduced to what matching needs.
type catalogEntry struct {
	app     *appstore.CatalogApp
	project string
	repo    string
}

type catalogIndex []catalogEntry

// catalogIndex parses every store app once per CheckAll — only when some app is
// untracked, which on a box installed entirely from Maison is never.
func (in *Installer) catalogIndex() catalogIndex {
	var idx catalogIndex
	for _, a := range in.store.CatalogAll() {
		raw, err := in.store.Compose(a)
		if err != nil {
			continue
		}
		f, err := composefile.Parse(raw)
		if err != nil {
			continue
		}
		idx = append(idx, catalogEntry{app: a, project: projectName(f.Name, a.ID), repo: mainRepo(raw)})
	}
	return idx
}

// suggest finds the store app an untracked project most plausibly came from.
//
// Two kinds of evidence: the name — the project an install of that store app would
// have created, which is how CasaOS named it too — and the main service's image. Both
// together is a match. The image alone beats the name alone: a stack someone called
// "files" that runs dufs is dufs, while a store app whose image moved on (a
// linuxserver build swapped for the official one) still matches by name when nothing
// else does. Whichever wins has to be one app; the same app shipped by several stores
// resolves to the copy that won the merge, and anything still ambiguous is no
// suggestion rather than a guess.
func (idx catalogIndex) suggest(project string, installed []byte) *Suggestion {
	have := mainRepo(installed)
	var both, byImage, byName []catalogEntry
	for _, e := range idx {
		named := e.project == project || strings.EqualFold(e.app.ID, project)
		imaged := have != "" && e.repo == have
		switch {
		case named && imaged:
			both = append(both, e)
		case imaged:
			byImage = append(byImage, e)
		case named:
			byName = append(byName, e)
		}
	}
	pick := one(both)
	if pick == nil {
		pick = one(byImage)
	}
	if pick == nil {
		pick = one(byName)
	}
	if pick == nil {
		return nil
	}
	return &Suggestion{
		Ref:         appstore.NewRef(pick.app.StoreURL, pick.app.AppsPath, pick.app.ID).Path(),
		Name:        pick.app.Name,
		StoreName:   pick.app.StoreName,
		ImagesMatch: have != "" && pick.repo == have,
	}
}

// one narrows candidates to a single app: itself when there is one, the merge's
// winner when several stores ship it, nil when that still leaves a choice.
func one(c []catalogEntry) *catalogEntry {
	if len(c) == 1 {
		return &c[0]
	}
	var primary []catalogEntry
	for _, e := range c {
		if e.app.Primary {
			primary = append(primary, e)
		}
	}
	if len(primary) == 1 {
		return &primary[0]
	}
	return nil
}
