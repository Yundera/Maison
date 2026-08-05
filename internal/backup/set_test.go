package backup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backup/backuptest"
)

const (
	older = "2026-01-01_120000"
	newer = "2026-02-02_120000"
)

func names(bs []apps.Backup) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Name
	}
	return out
}

// The write engine is a choice; reading must not follow it. This is the rule that
// keeps a user's existing backups reachable after they switch engines — without it
// they are all still there and none of them can be found.
func TestReadingIgnoresTheSelectedEngine(t *testing.T) {
	local := backuptest.NewLocalLike(apps.EngineLocal)
	remote := backuptest.NewRemote("kopia")
	local.Seed("jellyfin", older)
	remote.Seed("jellyfin", newer)

	s := New(local, remote)
	if err := s.SetWriter("kopia"); err != nil {
		t.Fatalf("SetWriter: %v", err)
	}

	// Selected engine is kopia, but the local archive must still be listed...
	got := names(s.List(context.Background(), "jellyfin"))
	if len(got) != 2 || got[0] != newer || got[1] != older {
		t.Fatalf("List = %v, want newest-first [%s %s]", got, newer, older)
	}

	// ...and still be reachable for restore, from the engine that actually has it.
	p, _, err := s.Locate(context.Background(), "jellyfin", older)
	if err != nil {
		t.Fatalf("Locate(%s): %v", older, err)
	}
	if p.ID() != apps.EngineLocal {
		t.Fatalf("Locate(%s) dispatched to %q, want the engine holding it (%q)", older, p.ID(), apps.EngineLocal)
	}
}

// The same backup in two engines is one backup to the user: one row, marked as
// being in both.
func TestUnionDedupesAndMarksBothTiers(t *testing.T) {
	local := backuptest.NewLocalLike(apps.EngineLocal)
	remote := backuptest.NewRemote("kopia")
	local.Seed("jellyfin", newer)
	remote.Seed("jellyfin", newer)

	got := New(local, remote).List(context.Background(), "jellyfin")
	if len(got) != 1 {
		t.Fatalf("List returned %d rows for one backup held twice: %v", len(got), names(got))
	}
	if got[0].Tier != apps.TierBoth {
		t.Errorf("Tier = %q, want %q", got[0].Tier, apps.TierBoth)
	}
	// The row should name the engine the restore will actually come from.
	if got[0].Engine != apps.EngineLocal {
		t.Errorf("Engine = %q, want the instant-restore engine %q", got[0].Engine, apps.EngineLocal)
	}
}

// Among engines holding the same backup, the one that can restore it without
// copying wins — otherwise a backup that is sitting on the local disk would be
// downloaded again.
func TestLocatePrefersInstantRestore(t *testing.T) {
	remote := backuptest.NewRemote("kopia")
	local := backuptest.NewLocalLike(apps.EngineLocal)
	remote.Seed("jellyfin", newer)
	local.Seed("jellyfin", newer)

	// Registered remote-first, so a naive "first match wins" would pick the wrong one.
	p, _, err := New(remote, local).Locate(context.Background(), "jellyfin", newer)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if p.ID() != apps.EngineLocal {
		t.Fatalf("Locate picked %q, want %q (instant restore)", p.ID(), apps.EngineLocal)
	}
}

// An unconfigured remote engine is the normal state of a box whose host-side setup
// has not run. It must contribute nothing rather than empty the page.
func TestUnconfiguredEngineDoesNotBreakListing(t *testing.T) {
	local := backuptest.NewLocalLike(apps.EngineLocal)
	local.Seed("jellyfin", newer)
	remote := backuptest.NewRemote("kopia")
	remote.ListErr = apps.ErrNotConfigured

	got := New(local, remote).List(context.Background(), "jellyfin")
	if len(got) != 1 || got[0].Name != newer {
		t.Fatalf("List = %v, want the local archive to survive an unconfigured engine", names(got))
	}
}

// A broken repository should degrade the page, not empty it — same reasoning as
// above, but for a real failure rather than an expected state.
func TestBrokenEngineDoesNotBreakListing(t *testing.T) {
	local := backuptest.NewLocalLike(apps.EngineLocal)
	local.Seed("jellyfin", newer)
	remote := backuptest.NewRemote("kopia")
	remote.ListErr = errors.New("repository unreachable")

	if got := New(local, remote).List(context.Background(), "jellyfin"); len(got) != 1 {
		t.Fatalf("List = %v, want the local archive to survive a broken engine", names(got))
	}
}

// Deleting a row the user sees once must delete the backup everywhere it is, or the
// row reappears on the next refresh with no explanation.
func TestDeleteRemovesFromEveryEngineHoldingIt(t *testing.T) {
	local := backuptest.NewLocalLike(apps.EngineLocal)
	remote := backuptest.NewRemote("kopia")
	local.Seed("jellyfin", newer)
	remote.Seed("jellyfin", newer)

	s := New(local, remote)
	if err := s.Delete(context.Background(), "jellyfin", newer); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := s.List(context.Background(), "jellyfin"); len(got) != 0 {
		t.Fatalf("after Delete, List = %v, want empty", names(got))
	}
	for _, f := range []*backuptest.Fake{local, remote} {
		if !strings.Contains(strings.Join(f.Calls, " "), "delete:jellyfin/"+newer) {
			t.Errorf("engine %s was not asked to delete: calls = %v", f.ID(), f.Calls)
		}
	}
}

func TestDeleteUnknownBackupIsAnError(t *testing.T) {
	s := New(backuptest.NewLocalLike(apps.EngineLocal))
	if err := s.Delete(context.Background(), "jellyfin", newer); err == nil {
		t.Fatal("Delete of a backup no engine holds should fail")
	}
}

// Writing somewhere other than where the user asked is how someone ends up
// believing their data is offsite when it is not, so an unknown engine must be a
// refusal rather than a fallback.
func TestSetWriterRefusesUnknownEngine(t *testing.T) {
	s := New(backuptest.NewLocalLike(apps.EngineLocal))
	if err := s.SetWriter("kopia"); err == nil {
		t.Fatal("SetWriter accepted an unregistered engine")
	}
	if got := s.Writer().ID(); got != apps.EngineLocal {
		t.Fatalf("Writer = %q after a refused SetWriter, want it unchanged (%q)", got, apps.EngineLocal)
	}
}

// The first engine registered is the write target, which makes the always-present
// local engine the default without anyone having to configure it.
func TestFirstRegisteredEngineIsTheDefaultWriter(t *testing.T) {
	s := New(backuptest.NewLocalLike(apps.EngineLocal), backuptest.NewRemote("kopia"))
	if got := s.Writer().ID(); got != apps.EngineLocal {
		t.Fatalf("default Writer = %q, want %q", got, apps.EngineLocal)
	}
}

// Re-registering a reconfigured engine must not move it, or the picker reorders
// itself under the user for no visible reason.
func TestReRegisterKeepsPosition(t *testing.T) {
	s := New(backuptest.NewLocalLike(apps.EngineLocal), backuptest.NewRemote("kopia"))
	s.Register(backuptest.NewRemote("kopia"))
	if got := s.IDs(); len(got) != 2 || got[0] != apps.EngineLocal || got[1] != "kopia" {
		t.Fatalf("IDs = %v, want [local kopia]", got)
	}
}
