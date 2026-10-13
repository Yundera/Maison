package backupconfig

import (
	"path/filepath"
	"testing"
)

// The settings page sends its whole document back. A page loaded before an approval
// must not withdraw it by saving, and a client must not be able to grant one.
func TestPreserveOwnedKeepsApprovalsOutOfTheClientsHands(t *testing.T) {
	cur := Config{
		ApprovalsSeeded: true,
		Engines: map[string]EngineSettings{
			"kopia":  {Approved: "ghcr.io/yundera/maison-kopia-engine"},
			"restic": {},
		},
	}
	yes := true
	next := Config{
		Engines: map[string]EngineSettings{
			"kopia":  {Schedule: &yes},         // stale page: no approval field
			"restic": {Approved: "evil/image"}, // a client trying to grant one
		},
	}
	got := PreserveOwned(next, cur)
	if !got.ApprovalsSeeded {
		t.Error("ApprovalsSeeded was reset by a client")
	}
	if got.Engines["kopia"].Approved != "ghcr.io/yundera/maison-kopia-engine" {
		t.Errorf("kopia approval lost: %+v", got.Engines["kopia"])
	}
	if got.Engines["kopia"].Schedule == nil || !*got.Engines["kopia"].Schedule {
		t.Error("the client's own change to kopia was dropped")
	}
	if got.Engines["restic"].Approved != "" {
		t.Error("a client granted an approval")
	}
}

func TestUpdateIsAReadModifyWriteThatPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.json")
	s := New(path)
	if err := s.Update(func(c *Config) {
		es := c.Engines["kopia"]
		es.Approved = "repo"
		c.Engines["kopia"] = es
		c.ApprovalsSeeded = true
	}); err != nil {
		t.Fatal(err)
	}
	re := New(path).Get()
	if re.Approved("kopia") != "repo" || !re.ApprovalsSeeded {
		t.Errorf("reloaded = %+v, want the approval persisted", re)
	}
}
