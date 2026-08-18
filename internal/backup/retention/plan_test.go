package retention

import (
	"testing"
	"time"

	"github.com/yundera/maison/internal/apps"
	"github.com/yundera/maison/internal/backupconfig"
)

// at builds a backup whose stamp is the given time, which is the only thing the
// planner reads.
func at(t time.Time) apps.Backup {
	s := t.Format(apps.StampLayout)
	return apps.Backup{App: "demo", Name: s, Stamp: s, Date: t.Format("2006-01-02")}
}

// daily returns n backups, one per day, ending on end (newest last).
func daily(end time.Time, n int) []apps.Backup {
	var out []apps.Backup
	for i := n - 1; i >= 0; i-- {
		out = append(out, at(end.AddDate(0, 0, -i)))
	}
	return out
}

func stamps(list []apps.Backup) []string {
	out := make([]string, len(list))
	for i, b := range list {
		out[i] = b.Stamp
	}
	return out
}

func smart() backupconfig.Resolved {
	return backupconfig.Resolved{Mode: backupconfig.ModeSmart, Keep: backupconfig.SmartKeep()}
}

var now = time.Date(2026, 8, 18, 3, 30, 0, 0, time.UTC)

func TestSmartKeepsOnePerPeriod(t *testing.T) {
	// Two years of dailies: 7 days + 4 weeks + 12 months of survivors, minus the
	// overlap where a daily is also the newest of its week or month.
	list := daily(now, 730)
	keep, drop := Plan(now, list, apps.RetentionSnapshot, smart())

	if len(keep)+len(drop) != len(list) {
		t.Fatalf("plan lost backups: %d kept + %d dropped != %d", len(keep), len(drop), len(list))
	}
	// Every one of the last 7 days survives.
	for i := 0; i < 7; i++ {
		want := at(now.AddDate(0, 0, -i)).Stamp
		if !has(keep, want) {
			t.Errorf("daily tier dropped %s", want)
		}
	}
	// One per month for the last twelve, and nothing from before that.
	if has(keep, at(now.AddDate(-2, 0, 0)).Stamp) {
		t.Error("kept a backup older than every tier")
	}
	if n := len(keep); n < 20 || n > 26 {
		t.Errorf("smart retention kept %d of 730, want ~23 (7 daily + 4 weekly + 12 monthly, overlapping)", n)
	}
}

func TestSmartCountsPeriodsThatHaveBackupsNotElapsedTime(t *testing.T) {
	// A box that was switched off for three weeks. Under a sliding window every daily
	// would have aged out; counting occupied periods keeps the last 7 it has.
	off := now.AddDate(0, 0, -21)
	list := daily(off, 10)

	keep, _ := Plan(now, list, apps.RetentionSnapshot, smart())
	for i := 0; i < 7; i++ {
		want := at(off.AddDate(0, 0, -i)).Stamp
		if !has(keep, want) {
			t.Errorf("dropped %s: dailies expired by elapsed time rather than by count", want)
		}
	}
}

func TestNewestSurvivesEveryPolicy(t *testing.T) {
	list := daily(now, 5)
	for name, r := range map[string]backupconfig.Resolved{
		"empty tiers": {Mode: backupconfig.ModeCustom, Keep: backupconfig.Keep{}},
		"count zero":  {Mode: backupconfig.ModeCount, Count: 0},
		"age zero":    {Mode: backupconfig.ModeAge, MaxAge: 0},
	} {
		keep, _ := Plan(now, list, apps.RetentionSnapshot, r)
		if len(keep) < 1 || keep[0].Stamp != at(now).Stamp {
			t.Errorf("%s: newest backup was not kept (kept %v)", name, stamps(keep))
		}
	}
}

func TestCountKeepsTheNewestN(t *testing.T) {
	list := daily(now, 10)
	keep, drop := Plan(now, list, apps.RetentionSnapshot,
		backupconfig.Resolved{Mode: backupconfig.ModeCount, Count: 3})
	if len(keep) != 3 || len(drop) != 7 {
		t.Fatalf("kept %d dropped %d, want 3/7", len(keep), len(drop))
	}
	if keep[0].Stamp != at(now).Stamp || keep[2].Stamp != at(now.AddDate(0, 0, -2)).Stamp {
		t.Errorf("kept the wrong three: %v", stamps(keep))
	}
}

func TestAgeDropsOnlyWhatIsOlderThanTheCutoff(t *testing.T) {
	list := daily(now, 10)
	keep, drop := Plan(now, list, apps.RetentionLifecycle, backupconfig.Resolved{
		Mode: backupconfig.ModeAge, MaxAge: 3 * 24 * time.Hour,
	})
	if len(drop) != 0 {
		t.Fatalf("lifecycle storage expires itself; Maison dropped %d", len(drop))
	}
	keep, drop = Plan(now, list, apps.RetentionSnapshot, backupconfig.Resolved{
		Mode: backupconfig.ModeAge, MaxAge: 3 * 24 * time.Hour,
	})
	if len(keep) != 4 {
		t.Fatalf("kept %v, want the 4 within 3 days", stamps(keep))
	}
	if len(drop) != 6 {
		t.Fatalf("dropped %d, want 6", len(drop))
	}
}

func TestChainTruncatesTheTailAndNeverTheMiddle(t *testing.T) {
	// Tiers want to thin the middle. On chained storage that would break every restore
	// point behind each hole, so the plan is demoted to truncating the oldest run.
	list := daily(now, 400)

	snap, _ := Plan(now, list, apps.RetentionSnapshot, smart())
	chain, chainDrop := Plan(now, list, apps.RetentionChain, smart())

	if len(chain) <= len(snap) {
		t.Fatalf("chain kept %d, snapshot kept %d: chain must keep at least as much", len(chain), len(snap))
	}
	// Everything kept is contiguous from the newest.
	for i, b := range chain {
		if b.Stamp != at(now.AddDate(0, 0, -i)).Stamp {
			t.Fatalf("chain kept a non-contiguous run at %d: %s", i, b.Stamp)
		}
	}
	// And what is dropped is strictly older than everything kept.
	oldestKept := chain[len(chain)-1].Stamp
	for _, b := range chainDrop {
		if b.Stamp >= oldestKept {
			t.Fatalf("chain dropped %s, which is not in the tail (oldest kept %s)", b.Stamp, oldestKept)
		}
	}
}

func TestUnknownModelExpiresNothing(t *testing.T) {
	list := daily(now, 400)
	_, drop := Plan(now, list, apps.RetentionNone, smart())
	if len(drop) != 0 {
		t.Fatalf("an engine that declared no model dropped %d backups", len(drop))
	}
}

func TestUnparseableStampIsNeverDropped(t *testing.T) {
	list := append(daily(now, 30), apps.Backup{App: "demo", Name: "not-a-stamp", Stamp: "not-a-stamp"})
	_, drop := Plan(now, list, apps.RetentionSnapshot,
		backupconfig.Resolved{Mode: backupconfig.ModeCount, Count: 1})
	for _, b := range drop {
		if b.Stamp == "not-a-stamp" {
			t.Fatal("dropped a backup whose stamp Maison cannot read")
		}
	}
}

func TestModeAllAndEmptyPolicyDropNothing(t *testing.T) {
	list := daily(now, 100)
	for name, r := range map[string]backupconfig.Resolved{
		"all":     {Mode: backupconfig.ModeAll, Keep: backupconfig.Keep{Latest: backupconfig.KeepAll}},
		"zero":    {},
		"inherit": {Mode: backupconfig.ModeInherit},
	} {
		if _, drop := Plan(now, list, apps.RetentionSnapshot, r); len(drop) != 0 {
			t.Errorf("%s: dropped %d backups", name, len(drop))
		}
	}
}

func TestEmptyListIsNotAPanic(t *testing.T) {
	if keep, drop := Plan(now, nil, apps.RetentionSnapshot, smart()); keep != nil || drop != nil {
		t.Fatal("expected nothing from nothing")
	}
}

func TestPlanDoesNotReorderOrLoseInput(t *testing.T) {
	list := daily(now, 50)
	before := stamps(list)
	keep, drop := Plan(now, list, apps.RetentionSnapshot, smart())
	if got := stamps(list); !equal(got, before) {
		t.Fatal("Plan reordered its caller's slice")
	}
	seen := map[string]int{}
	for _, b := range append(append([]apps.Backup{}, keep...), drop...) {
		seen[b.Stamp]++
	}
	if len(seen) != len(list) {
		t.Fatalf("plan covered %d distinct backups, input had %d", len(seen), len(list))
	}
	for s, n := range seen {
		if n != 1 {
			t.Fatalf("%s appears %d times across keep and drop", s, n)
		}
	}
}

func has(list []apps.Backup, stamp string) bool {
	for _, b := range list {
		if b.Stamp == stamp {
			return true
		}
	}
	return false
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
