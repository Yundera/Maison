package apps

import (
	"testing"

	"github.com/yundera/maison/internal/dockerx"
)

// Every container either belongs to an app in the listing or is untracked —
// including plain `docker run` containers, which carry no compose project at all.
func TestUntrackedKeepsWhatNoAppAccountsFor(t *testing.T) {
	all := []dockerx.Container{
		{ID: "aaaaaaaaaaaaaaaa", Name: "jellyfin", Project: "jellyfin", Service: "jellyfin"},
		{ID: "bbbbbbbbbbbbbbbb", Name: "watchtower"},
		{ID: "cccccccccccccccc", Name: "scratch-db-1", Project: "scratch", Service: "db"},
		{ID: "dddddddddddddddd", Name: "adminer"},
	}
	got := untracked(all, []App{{ID: "jellyfin"}})
	want := []string{"scratch-db-1", "adminer", "watchtower"}
	if len(got) != len(want) {
		t.Fatalf("got %d untracked, want %d: %+v", len(got), len(want), got)
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("[%d] = %q, want %q", i, got[i].Name, name)
		}
	}
	if got[0].ID != "cccccccccccc" {
		t.Errorf("id = %q, want the 12-char short id", got[0].ID)
	}
}

func TestUntrackedEmptyIsNotNil(t *testing.T) {
	if got := untracked(nil, nil); got == nil {
		t.Fatal("want an empty list, got nil (it is rendered as JSON)")
	}
}
