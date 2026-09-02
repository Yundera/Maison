package server

import (
	"path/filepath"
	"testing"

	"github.com/yundera/maison/internal/backupconfig"
	"github.com/yundera/maison/internal/notify"
	"github.com/yundera/maison/internal/usersettings"
)

func newSettings(t *testing.T) *usersettings.Store {
	t.Helper()
	return usersettings.New(filepath.Join(t.TempDir(), "settings.json"))
}

// A box that configured a relay before the move must keep alerting after it. The
// value lands in the settings store and the old key is cleared, so the next boot has
// nothing left to adopt.
func TestAdoptLegacySMTPMovesTheConfiguration(t *testing.T) {
	settings := newSettings(t)
	conf := backupconfig.Config{LegacySMTP: &notify.SMTP{Host: "relay", Port: 25, From: "a@b", To: "c@d"}}

	if !adoptLegacySMTP(settings, &conf) {
		t.Fatal("adoptLegacySMTP reported no change")
	}
	if conf.LegacySMTP != nil {
		t.Errorf("legacy key left behind: %+v", conf.LegacySMTP)
	}
	got := settings.EffectiveSMTP(notify.SMTP{})
	if got.Host != "relay" || got.To != "c@d" {
		t.Errorf("settings resolve to %+v, want the adopted relay", got)
	}
}

// One-way: a relay the user has since set in the new place wins, and the stale copy
// is dropped rather than reverting them to it.
func TestAdoptLegacySMTPDoesNotOverwriteTheNewPlace(t *testing.T) {
	settings := newSettings(t)
	if err := settings.Set(usersettings.Settings{SMTP: &notify.SMTP{Host: "current", From: "a@b", To: "c@d"}}); err != nil {
		t.Fatal(err)
	}
	conf := backupconfig.Config{LegacySMTP: &notify.SMTP{Host: "stale", From: "x@y", To: "z@w"}}

	if !adoptLegacySMTP(settings, &conf) {
		t.Fatal("the stale copy should still have been cleared")
	}
	if conf.LegacySMTP != nil {
		t.Errorf("legacy key left behind: %+v", conf.LegacySMTP)
	}
	if got := settings.EffectiveSMTP(notify.SMTP{}); got.Host != "current" {
		t.Errorf("host %q, want the one already configured in settings.json", got.Host)
	}
}

// The ordinary case — no legacy key — must not touch either store, so a box that has
// never configured mail does not get a settings write on every boot.
func TestAdoptLegacySMTPIsANoOpWithoutOne(t *testing.T) {
	settings := newSettings(t)
	conf := backupconfig.Config{Hour: 3}

	if adoptLegacySMTP(settings, &conf) {
		t.Error("reported a change with nothing to adopt")
	}
	if settings.Get().SMTP != nil {
		t.Errorf("settings gained an SMTP block: %+v", settings.Get().SMTP)
	}
}
