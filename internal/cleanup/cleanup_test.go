package cleanup

import (
	"testing"

	"github.com/yundera/maison/internal/dockerx"
)

func imageIDs(p Plan) map[string]bool {
	out := map[string]bool{}
	for _, im := range p.Images {
		out[im.ID] = true
	}
	return out
}

// The case that rules out `docker system prune --all`: a stopped app's containers
// and image are the app, not rubbish.
func TestStoppedAppIsNotCleaned(t *testing.T) {
	p := Build(Input{
		Images: []dockerx.ImageInfo{{ID: "sha:app", Tags: []string{"nginx:1.25"}, Size: 100, Containers: -1}},
		Containers: []dockerx.ContainerInfo{{
			ID: "c1", ImageID: "sha:app", State: "exited", Project: "web", WorkingDir: "/DATA/AppData/casaos/apps/web",
		}},
		FolderGone: func(string) bool { return false },
	})
	if len(p.Images) != 0 || len(p.Orphans) != 0 {
		t.Fatalf("a stopped app must be left whole, got %+v", p)
	}
	if p.ImagesKept != 1 {
		t.Fatalf("kept = %d, want 1", p.ImagesKept)
	}
}

func TestUnusedImageIsCleanedUnlessAnAppNamesIt(t *testing.T) {
	in := Input{
		Images: []dockerx.ImageInfo{
			{ID: "sha:old", Tags: []string{"nginx:1.24"}, Size: 100, SharedSize: 40, Containers: 0},
			{ID: "sha:named", Tags: []string{"nginx:1.25"}, Size: 100, Containers: 0},
			{ID: "sha:dangling", Size: 10, Containers: 0},
			{ID: "sha:latest", Tags: []string{"redis:latest"}, Size: 10, Containers: 0},
			{ID: "sha:ghcr", Tags: []string{"ghcr.io/acme/app:2.0"}, Size: 10, Containers: 0},
		},
		// Spelled differently from the tags on purpose: fully qualified, implicit
		// :latest, and an unresolved variable.
		Referenced: []string{"docker.io/library/nginx:1.25", "redis", "ghcr.io/acme/app:${APP_TAG}"},
	}
	p := Build(in)
	got := imageIDs(p)
	if !got["sha:old"] || !got["sha:dangling"] {
		t.Fatalf("unused images should be cleaned, got %v", got)
	}
	for _, keep := range []string{"sha:named", "sha:latest", "sha:ghcr"} {
		if got[keep] {
			t.Fatalf("%s is named by an installed app and must be kept", keep)
		}
	}
	if p.ImagesBytes != 60+10 {
		t.Fatalf("bytes = %d, want 70 (shared layers excluded)", p.ImagesBytes)
	}
}

func TestDigestPinnedReferenceKeepsImage(t *testing.T) {
	d := "sha256:" + "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	p := Build(Input{
		Images:     []dockerx.ImageInfo{{ID: "sha:x", Digests: []string{"alpine@" + d}, Containers: 0}},
		Referenced: []string{"alpine:3.20@" + d},
	})
	if len(p.Images) != 0 {
		t.Fatalf("a digest-pinned reference must keep its image, got %+v", p.Images)
	}
}

// A stopped container keeps its network ids; `docker network prune` ignores them
// and the container then fails to start.
func TestNetworkOfStoppedContainerIsKept(t *testing.T) {
	compose := map[string]string{"com.docker.compose.network": "default"}
	p := Build(Input{
		Networks: []dockerx.NetworkInfo{
			{ID: "n-stopped", Name: "web_default", Scope: "local", Labels: compose},
			{ID: "n-left", Name: "gone_default", Scope: "local", Labels: compose},
			{ID: "n-hand", Name: "mynet", Scope: "local"},
			{ID: "n-pcs", Name: "pcs", Scope: "local", Labels: compose},
			{ID: "n-bridge", Name: "bridge", Scope: "local"},
		},
		Containers:   []dockerx.ContainerInfo{{ID: "c1", State: "exited", Networks: []string{"n-stopped"}}},
		KeepNetworks: map[string]bool{"pcs": true},
	})
	if len(p.Networks) != 1 || p.Networks[0].ID != "n-left" {
		t.Fatalf("only the unattached compose leftover should go, got %+v", p.Networks)
	}
}

func TestOrphans(t *testing.T) {
	gone := map[string]bool{"/DATA/AppData/casaos/apps/ghost": true, "/DATA/AppData/sys": true, "/DATA/AppData/self": true}
	p := Build(Input{
		Containers: []dockerx.ContainerInfo{
			{ID: "g1", Name: "ghost-db", Project: "ghost", State: "exited", WorkingDir: "/DATA/AppData/casaos/apps/ghost"},
			{ID: "g2", Name: "ghost-web", Project: "ghost", State: "running", WorkingDir: "/DATA/AppData/casaos/apps/ghost"},
			{ID: "a1", Name: "app", Project: "app", State: "exited", WorkingDir: "/DATA/AppData/casaos/apps/app"},
			{ID: "s1", Name: "sys", Project: "sys", State: "exited", WorkingDir: "/DATA/AppData/sys"},
			{ID: "selfabc", Name: "maison", Project: "self", State: "running", WorkingDir: "/DATA/AppData/self"},
			{ID: "r1", Name: "manual-running", State: "running"},
			{ID: "r2", Name: "manual-exited", State: "exited"},
			{ID: "r3", Name: "manual-created", State: "created"},
		},
		Protected:  map[string]bool{"sys": true},
		FolderGone: func(d string) bool { return gone[d] },
		Self:       "self",
	})
	if len(p.Orphans) != 2 {
		t.Fatalf("want ghost + manual-exited, got %+v", p.Orphans)
	}
	g := p.Orphans[0]
	if g.Key != "ghost" || g.Reason != ReasonFolderGone || len(g.Containers) != 2 || !g.Running {
		t.Fatalf("ghost project grouped wrong: %+v", g)
	}
	s := p.Orphans[1]
	if s.Key != "r2" || s.Reason != ReasonStandalone {
		t.Fatalf("standalone orphan wrong: %+v", s)
	}
}

func TestExpandRef(t *testing.T) {
	env := map[string]string{"TAG": "1.2", "EMPTY": ""}
	cases := map[string]string{
		"app:${TAG}":            "app:1.2",
		"app:$TAG":              "app:1.2",
		"app:${NOPE:-3.0}":      "app:3.0",
		"app:${EMPTY:-3.0}":     "app:3.0",
		"app:${EMPTY-3.0}":      "app:",
		"app:${NOPE}":           "app:${NOPE}",
		"ghcr.io/a/b:${TAG}-rc": "ghcr.io/a/b:1.2-rc",
	}
	for in, want := range cases {
		if got := ExpandRef(in, env); got != want {
			t.Errorf("ExpandRef(%q) = %q, want %q", in, got, want)
		}
	}
}
